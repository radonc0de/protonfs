// protonfs mounts Proton Drive as a regular filesystem.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radonc0de/protonfs/internal/bridge"
	"github.com/radonc0de/protonfs/internal/pfs"
)

var version = "dev"

const usage = `protonfs - Proton Drive as a filesystem

Usage:
  protonfs login                 sign in to Proton (runs: proton-drive auth login)
  protonfs enable DIR [--linger] mount DIR now and at every login (systemd user service)
  protonfs disable DIR [--purge] stop and remove the service for DIR (--purge: and its cache)
  protonfs status                list protonfs services
  protonfs cache                 show cache usage per mount
  protonfs cache clear [DIR]     delete cached files (all mounts, or DIR's)
  protonfs mount DIR             mount in the foreground (Ctrl-C to stop)
  protonfs unmount DIR           unmount
  protonfs version

Settings (env or ~/.config/protonfs/config.env):
  PROTONFS_CACHE_SIZE=10G        max local cache (LRU); unsaved writes never evicted
  PROTONFS_CACHE_MAX_AGE=30d     also drop cached files unused this long (default: off)
  PROTONFS_TTL=30                seconds to cache directory listings
  PROTONFS_DELETE=permanent      rm deletes permanently (default: Proton trash)
`

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "mount":
		err = cmdMount(args)
	case "unmount", "umount":
		err = cmdUnmount(args)
	case "enable":
		err = cmdEnable(args)
	case "disable":
		err = cmdDisable(args)
	case "status":
		err = cmdStatus()
	case "cache":
		err = cmdCache(args)
	case "login":
		err = cmdLogin()
	case "version", "--version", "-v":
		fmt.Println("protonfs", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "protonfs:", err)
		os.Exit(1)
	}
}

// ---------- helpers ----------

func home() string { h, _ := os.UserHomeDir(); return h }

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "protonfs")
	}
	return filepath.Join(home(), ".config", "protonfs")
}

func cacheBase() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "protonfs")
	}
	return filepath.Join(home(), ".cache", "protonfs")
}

func dataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "protonfs")
	}
	return filepath.Join(home(), ".local", "share", "protonfs")
}

// loadConfig reads KEY=VALUE lines from config.env into the environment
// (without overriding variables that are already set).
func loadConfig() {
	b, err := os.ReadFile(filepath.Join(configDir(), "config.env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

// parseAge parses durations like 30d, 12h, 90m; "", "0" and "off" mean no limit.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "0", "off", "none":
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := strconv.ParseFloat(n, 64); err == nil && d > 0 {
			return time.Duration(d * float64(24*time.Hour)), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("invalid age %q (use e.g. 30d, 12h, or off)", s)
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult != 1 {
			s = s[:len(s)-1]
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}

func absDir(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home(), p[2:])
	}
	return filepath.Abs(p)
}

// checkOwnedDir verifies p is a directory owned by the current user.
func checkOwnedDir(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		if isStaleMount(err) {
			return nil
		}
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by you; pick a directory you own (or chown it)", p)
	}
	return nil
}

func isStaleMount(err error) bool { return errors.Is(err, syscall.ENOTCONN) }

func fusermount() string {
	for _, c := range []string{"fusermount3", "fusermount"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return "fusermount"
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func findBridge() (node, script string, err error) {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	script = firstExisting(
		os.Getenv("PROTONFS_BRIDGE"),
		filepath.Join(dataDir(), "protonfs-bridge.mjs"),
		filepath.Join(exeDir, "..", "share", "protonfs", "protonfs-bridge.mjs"),
		filepath.Join(exeDir, "protonfs-bridge.mjs"),
		filepath.Join(exeDir, "bridge", "dist", "protonfs-bridge.mjs"),
	)
	if script == "" {
		return "", "", errors.New("protonfs-bridge.mjs not found (reinstall, or set PROTONFS_BRIDGE)")
	}
	node = firstExisting(os.Getenv("PROTONFS_NODE"), filepath.Join(dataDir(), "node", "bin", "node"))
	if node == "" {
		if p, e := exec.LookPath("node"); e == nil {
			node = p
		}
	}
	if node == "" {
		return "", "", errors.New("node (Node.js >= 20) not found (reinstall, or set PROTONFS_NODE)")
	}
	return node, script, nil
}

// mountMarker, inside a cache dir, records which mountpoint it belongs to.
const mountMarker = "mountpoint"

func instanceCacheDir(mnt string) string {
	h := sha256.Sum256([]byte(mnt))
	name := strings.Trim(strings.ReplaceAll(mnt, "/", "-"), "-")
	if len(name) > 40 {
		name = name[len(name)-40:]
	}
	return filepath.Join(cacheBase(), name+"-"+hex.EncodeToString(h[:4]))
}

// ---------- commands ----------

func cmdMount(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: protonfs mount DIR")
	}
	loadConfig()
	mnt, err := absDir(args[0])
	if err != nil {
		return err
	}
	if _, err := os.Stat(mnt); isStaleMount(err) {
		exec.Command(fusermount(), "-uz", mnt).Run()
	}
	if err := checkOwnedDir(mnt); err != nil {
		return err
	}

	cacheSize := int64(10 << 30)
	if s := os.Getenv("PROTONFS_CACHE_SIZE"); s != "" {
		if cacheSize, err = parseSize(s); err != nil {
			return err
		}
	}
	maxAge, err := parseAge(os.Getenv("PROTONFS_CACHE_MAX_AGE"))
	if err != nil {
		return fmt.Errorf("PROTONFS_CACHE_MAX_AGE: %w", err)
	}
	ttl := 30 * time.Second
	if s := os.Getenv("PROTONFS_TTL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			ttl = time.Duration(n) * time.Second
		}
	}

	node, script, err := findBridge()
	if err != nil {
		return err
	}
	cache, err := pfs.OpenCache(instanceCacheDir(mnt), cacheSize, maxAge)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	os.WriteFile(filepath.Join(cache.Dir, mountMarker), []byte(mnt+"\n"), 0o600)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	br := bridge.New(node, script)
	if err := br.Start(ctx); err != nil {
		return err
	}
	defer br.Close()

	pf, err := pfs.New(br, cache, pfs.Options{TTL: ttl, PermanentDelete: os.Getenv("PROTONFS_DELETE") == "permanent"})
	if err != nil {
		return fmt.Errorf("connecting to Proton Drive: %w", err)
	}

	sec := time.Second
	server, err := fs.Mount(mnt, pf.Root(), &fs.Options{
		EntryTimeout:    &sec,
		AttrTimeout:     &sec,
		NegativeTimeout: &sec,
		MountOptions: fuse.MountOptions{
			FsName:        "protonfs",
			Name:          "protonfs",
			DisableXAttrs: true,
			MaxReadAhead:  1 << 20,
		},
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", mnt, err)
	}
	log.Printf("protonfs %s mounted at %s (cache %s, max %s)", version, mnt, cache.Dir, humanSize(cacheSize))

	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		log.Printf("stopping...")
		pf.FlushAll()
		if err := server.Unmount(); err != nil {
			log.Printf("unmount: %v (files still open?); forcing lazy unmount", err)
			exec.Command(fusermount(), "-uz", mnt).Run()
		}
	case <-done:
		log.Printf("unmounted externally")
	}
	pf.FlushAll()
	log.Printf("bye")
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dK", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func cmdUnmount(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: protonfs unmount DIR")
	}
	mnt, err := absDir(args[0])
	if err != nil {
		return err
	}
	out, err := exec.Command(fusermount(), "-u", mnt).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func cmdLogin() error {
	pd, err := exec.LookPath("proton-drive")
	if err != nil {
		pd = filepath.Join(home(), ".local", "bin", "proton-drive")
	}
	c := exec.Command(pd, "auth", "login")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func systemctl(args ...string) error {
	c := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func unitName(mnt string) (string, error) {
	out, err := exec.Command("systemd-escape", "--path", "--template=protonfs@.service", mnt).Output()
	if err != nil {
		return "", fmt.Errorf("systemd-escape: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

const unitTemplate = `[Unit]
Description=protonfs: Proton Drive mounted at %f
StartLimitIntervalSec=0

[Service]
Type=simple
Environment=PROTON_DRIVE_BIN={{PD}}
EnvironmentFile=-%h/.config/protonfs/config.env
ExecStart={{EXE}} mount %f
Restart=on-failure
RestartSec=15
KillMode=mixed
# leave time for pending uploads on stop/logout
TimeoutStopSec=600

[Install]
WantedBy=default.target
`

func cmdEnable(args []string) error {
	fl := flag.NewFlagSet("enable", flag.ContinueOnError)
	linger := fl.Bool("linger", false, "keep mounted after you log out (loginctl enable-linger)")
	var dirs []string
	for _, a := range args { // allow flags after DIR
		if strings.HasPrefix(a, "-") {
			if err := fl.Parse([]string{a}); err != nil {
				return err
			}
		} else {
			dirs = append(dirs, a)
		}
	}
	if len(dirs) != 1 {
		return errors.New("usage: protonfs enable DIR [--linger]")
	}
	mnt, err := absDir(dirs[0])
	if err != nil {
		return err
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil && !isStaleMount(err) {
		return err
	}
	if err := checkOwnedDir(mnt); err != nil {
		return err
	}
	if _, _, err := findBridge(); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	pd, _ := exec.LookPath("proton-drive")
	unitDir := filepath.Join(home(), ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unit := strings.NewReplacer("{{EXE}}", exe, "{{PD}}", pd).Replace(unitTemplate)
	if err := os.WriteFile(filepath.Join(unitDir, "protonfs@.service"), []byte(unit), 0o644); err != nil {
		return err
	}
	name, err := unitName(mnt)
	if err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", name); err != nil {
		return err
	}
	if *linger {
		c := exec.Command("loginctl", "enable-linger")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: loginctl enable-linger failed:", err)
		}
	}

	// Wait briefly for the mount to come up so we can report success.
	for i := 0; i < 60; i++ {
		if isMounted(mnt) {
			fmt.Printf("Proton Drive is mounted at %s (service %s)\n", mnt, name)
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("service started but %s is not mounted yet; check: journalctl --user -u %s", mnt, name)
}

func isMounted(mnt string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	esc := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`).Replace(mnt)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && f[4] == esc && strings.Contains(line, "protonfs") {
			return true
		}
	}
	return false
}

func cmdDisable(args []string) error {
	purge := false
	var dirs []string
	for _, a := range args {
		switch a {
		case "--purge":
			purge = true
		default:
			dirs = append(dirs, a)
		}
	}
	if len(dirs) != 1 {
		return errors.New("usage: protonfs disable DIR [--purge]")
	}
	mnt, err := absDir(dirs[0])
	if err != nil {
		return err
	}
	name, err := unitName(mnt)
	if err != nil {
		return err
	}
	if err := systemctl("disable", "--now", name); err != nil {
		return err
	}
	if !purge {
		return nil
	}
	if isMounted(mnt) {
		return fmt.Errorf("%s is still mounted (by something other than the service?); unmount it, then run: protonfs cache clear %s", mnt, mnt)
	}
	return purgeCacheDir(instanceCacheDir(mnt))
}

// purgeCacheDir deletes a mount's cache directory, except unsaved data in
// lost+found.
func purgeCacheDir(dir string) error {
	lf := filepath.Join(dir, pfs.LostFound)
	if n, _ := pfs.DirFiles(lf); n > 0 {
		if err := clearCacheDir(dir); err != nil {
			return err
		}
		os.Remove(filepath.Join(dir, mountMarker))
		fmt.Printf("Removed cache %s, but kept %s: it holds %d file(s) of unsaved changes from a crash. Delete it yourself once you've checked them.\n", dir, lf, n)
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("Removed cache %s\n", dir)
	return nil
}

// clearCacheDir deletes cached (clean) file contents in dir. The mount
// must not be running.
func clearCacheDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, de := range ents {
		n := de.Name()
		if strings.HasPrefix(n, "rev-") {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

type cacheInfo struct {
	dir, mnt       string
	n, nLost       int
	size, lostSize int64
}

func listCaches() []cacheInfo {
	ents, _ := os.ReadDir(cacheBase())
	var out []cacheInfo
	for _, de := range ents {
		if !de.IsDir() {
			continue
		}
		ci := cacheInfo{dir: filepath.Join(cacheBase(), de.Name())}
		if b, err := os.ReadFile(filepath.Join(ci.dir, mountMarker)); err == nil {
			ci.mnt = strings.TrimSpace(string(b))
		}
		ci.n, ci.size = cachedFiles(ci.dir)
		ci.nLost, ci.lostSize = pfs.DirFiles(filepath.Join(ci.dir, pfs.LostFound))
		out = append(out, ci)
	}
	return out
}

// cachedFiles counts cached (clean) file contents in a mount's cache dir.
func cachedFiles(dir string) (n int, size int64) {
	ents, _ := os.ReadDir(dir)
	for _, de := range ents {
		if info, err := de.Info(); err == nil && strings.HasPrefix(de.Name(), "rev-") && info.Mode().IsRegular() {
			n++
			size += info.Size()
		}
	}
	return n, size
}

func cmdCache(args []string) error {
	if len(args) == 0 {
		caches := listCaches()
		if len(caches) == 0 {
			fmt.Println("no cache yet")
			return nil
		}
		for _, c := range caches {
			mnt := c.mnt
			if mnt == "" {
				mnt = "(unknown mount)"
			}
			line := fmt.Sprintf("%-40s %8s in %4d file(s)   %s", mnt, humanSize(c.size), c.n, c.dir)
			if c.nLost > 0 {
				line += fmt.Sprintf("\n%-40s %8s in %4d file(s) of unsaved changes from a crash in %s", "", humanSize(c.lostSize), c.nLost, filepath.Join(c.dir, pfs.LostFound))
			}
			fmt.Println(line)
		}
		return nil
	}
	if args[0] != "clear" || len(args) > 2 {
		return errors.New("usage: protonfs cache [clear [DIR]]")
	}
	var targets []cacheInfo
	if len(args) == 2 {
		mnt, err := absDir(args[1])
		if err != nil {
			return err
		}
		targets = []cacheInfo{{dir: instanceCacheDir(mnt), mnt: mnt}}
	} else {
		targets = listCaches()
	}
	for _, c := range targets {
		if err := clearOne(c); err != nil {
			return err
		}
	}
	return nil
}

// clearOne clears one mount's cache. A running service is stopped first (which
// uploads pending changes) and started again afterwards.
func clearOne(c cacheInfo) error {
	restart := ""
	if c.mnt != "" && isMounted(c.mnt) {
		name, err := unitName(c.mnt)
		if err != nil {
			return err
		}
		if exec.Command("systemctl", "--user", "is-active", "--quiet", name).Run() != nil {
			return fmt.Errorf("%s is mounted outside the service; unmount it first", c.mnt)
		}
		fmt.Printf("Stopping %s (pending uploads finish first)...\n", name)
		if err := systemctl("stop", name); err != nil {
			return err
		}
		restart = name
	}
	n, size := cachedFiles(c.dir)
	if err := clearCacheDir(c.dir); err != nil {
		return err
	}
	n2, size2 := cachedFiles(c.dir)
	label := c.mnt
	if label == "" {
		label = c.dir
	}
	fmt.Printf("%s: freed %s (%d file(s))\n", label, humanSize(size-size2), n-n2)
	if l, _ := pfs.DirFiles(filepath.Join(c.dir, pfs.LostFound)); l > 0 {
		fmt.Printf("  note: %s still holds %d file(s) of unsaved changes from a crash\n", filepath.Join(c.dir, pfs.LostFound), l)
	}
	if restart != "" {
		return systemctl("start", restart)
	}
	return nil
}

func cmdStatus() error {
	out, _ := exec.Command("systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "protonfs@*").Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		fmt.Println("no protonfs services (use: protonfs enable DIR)")
		return nil
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		p, _ := exec.Command("systemd-escape", "--unescape", "--path", "--instance", f[0]).Output()
		mnt := strings.TrimSpace(string(p))
		state := f[3]
		if isMounted(mnt) {
			state += ", mounted"
		}
		_, size := cachedFiles(instanceCacheDir(mnt))
		fmt.Printf("%-40s %-20s cache %s\n", mnt, state, humanSize(size))
	}
	return nil
}
