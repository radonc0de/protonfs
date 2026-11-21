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
  protonfs disable DIR           stop and remove the service for DIR
  protonfs status                list protonfs services
  protonfs mount DIR             mount in the foreground (Ctrl-C to stop)
  protonfs unmount DIR           unmount
  protonfs version

Settings (env or ~/.config/protonfs/config.env):
  PROTONFS_CACHE_SIZE=10G        max local cache (LRU); unsaved writes never evicted
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
	cache, err := pfs.OpenCache(instanceCacheDir(mnt), cacheSize)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}

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
	default:
		return fmt.Sprintf("%dM", n>>20)
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
	if len(args) != 1 {
		return errors.New("usage: protonfs disable DIR")
	}
	mnt, err := absDir(args[0])
	if err != nil {
		return err
	}
	name, err := unitName(mnt)
	if err != nil {
		return err
	}
	return systemctl("disable", "--now", name)
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
		fmt.Printf("%-40s %s\n", mnt, state)
	}
	return nil
}
