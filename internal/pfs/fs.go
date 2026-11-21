// Package pfs implements the protonfs FUSE filesystem.
//
// Layout:  /my-files/...         your Drive
//
//	/devices/<name>/...   synced computers
//	/shared-with-me/...   items others shared with you
//
// Reads stream through a cache file that a background download fills. Writes
// go to a local file that is uploaded (as a new file or a new revision)
// shortly after the last writer closes it, or immediately on fsync.
package pfs

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/radonc0de/protonfs/internal/bridge"
)

const (
	renameNoReplace = 1 // RENAME_NOREPLACE
	renameExchange  = 2 // RENAME_EXCHANGE

	uploadDebounce = 750 * time.Millisecond
	uploadRetry    = 30 * time.Second
	maxUploadTries = 5
)

type Options struct {
	TTL             time.Duration // directory listing cache
	PermanentDelete bool          // rm deletes instead of trashing
}

type remoteNode struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	Ctime  int64  `json:"ctime"`
	RevUID string `json:"revUid"`
}

type entry struct {
	ino     uint64
	name    string
	parent  *entry
	dir     bool
	virtual string // "root", "devices", "shared" or ""
	uid     string // "" for virtual dirs and files not uploaded yet
	revUID  string
	size    int64
	mtime   time.Time
	ctime   time.Time
	removed bool

	children   map[string]*entry
	childrenAt time.Time
	loading    chan struct{}

	local       *localFile
	openCount   int
	writers     int
	uploadTimer *time.Timer
	uploading   bool
	reupload    bool
	failures    int
}

type localFile struct {
	f        *os.File
	path     string
	cacheRev string // pinned clean cache revision, "" if not in the cache index
	size     int64
	have     int64 // bytes present so far
	complete bool
	dirty    bool
	err      error
	cancel   context.CancelFunc
}

// FS is the filesystem state. One mutex guards every entry and localFile.
type FS struct {
	mu      sync.Mutex
	cond    *sync.Cond
	br      *bridge.Client
	cache   *Cache
	opts    Options
	root    *entry
	nextIno uint64
	uid     uint32
	gid     uint32
	uploads sync.WaitGroup

	quotaMax, quotaUsed int64
	quotaAt             time.Time
}

func New(br *bridge.Client, cache *Cache, opts Options) (*FS, error) {
	var hello struct {
		MyFilesUID string `json:"myFilesUid"`
		AppVersion string `json:"appVersion"`
	}
	if err := br.Call(context.Background(), "hello", nil, &hello); err != nil {
		return nil, err
	}
	f := &FS{br: br, cache: cache, opts: opts, nextIno: 1, uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	f.cond = sync.NewCond(&f.mu)
	now := time.Now()
	f.root = &entry{ino: 1, dir: true, virtual: "root", mtime: now, ctime: now, children: map[string]*entry{}}
	add := func(name, virtual, uid string) {
		f.root.children[name] = &entry{ino: f.newIno(), name: name, parent: f.root, dir: true, virtual: virtual, uid: uid, mtime: now, ctime: now}
	}
	add("my-files", "", hello.MyFilesUID)
	add("devices", "devices", "")
	add("shared-with-me", "shared", "")
	return f, nil
}

func (f *FS) newIno() uint64 { f.nextIno++; return f.nextIno }

// Root returns the go-fuse root node.
func (f *FS) Root() fs.InodeEmbedder { return &node{fs: f, e: f.root} }

func bg() context.Context { return context.Background() }

func fullPath(e *entry) string {
	var parts []string
	for c := e; c != nil && c.virtual != "root"; c = c.parent {
		parts = append([]string{c.name}, parts...)
	}
	return "/" + strings.Join(parts, "/")
}

// ---------- directory listings ----------

// refresh makes sure e.children is loaded and not older than the TTL.
func (f *FS) refresh(e *entry, force bool) syscall.Errno {
	f.mu.Lock()
	if !e.dir {
		f.mu.Unlock()
		return syscall.ENOTDIR
	}
	if e.virtual == "root" {
		f.mu.Unlock()
		return 0
	}
	for e.loading != nil {
		ch := e.loading
		f.mu.Unlock()
		<-ch
		f.mu.Lock()
		if !force && e.children != nil {
			f.mu.Unlock()
			return 0
		}
	}
	if !force && e.children != nil && time.Since(e.childrenAt) < f.opts.TTL {
		f.mu.Unlock()
		return 0
	}
	ch := make(chan struct{})
	e.loading = ch
	virtual, uid := e.virtual, e.uid
	f.mu.Unlock()

	var nodes []remoteNode
	var err error
	switch virtual {
	case "devices":
		err = f.br.Call(bg(), "devices", nil, &nodes)
	case "shared":
		err = f.br.Call(bg(), "sharedWithMe", nil, &nodes)
	default:
		err = f.br.Call(bg(), "list", map[string]any{"uid": uid}, &nodes)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	e.loading = nil
	close(ch)
	if err != nil {
		if e.children != nil {
			log.Printf("listing %s failed, serving cached: %v", fullPath(e), err)
			return 0
		}
		return bridge.Errno(err)
	}
	f.mergeLocked(e, nodes)
	return 0
}

func (f *FS) mergeLocked(dir *entry, nodes []remoteNode) {
	byUID := map[string]*entry{}
	for _, c := range dir.children {
		if c.uid != "" {
			byUID[c.uid] = c
		}
	}
	fresh := make(map[string]*entry, len(nodes))
	for _, n := range nodes {
		if n.Type != "file" && n.Type != "folder" {
			continue
		}
		name := strings.ReplaceAll(n.Name, "/", "∕")
		if name == "" || name == "." || name == ".." {
			name = n.UID
		}
		if _, dup := fresh[name]; dup {
			suffix := n.UID
			if len(suffix) > 8 {
				suffix = suffix[len(suffix)-8:]
			}
			name = name + " [" + suffix + "]"
		}
		e := byUID[n.UID]
		if e == nil {
			e = &entry{ino: f.newIno(), uid: n.UID, dir: n.Type == "folder"}
		}
		e.name, e.parent = name, dir
		busy := e.uploading || (e.local != nil && e.local.dirty)
		if !busy {
			if e.revUID != "" && e.revUID != n.RevUID {
				f.cache.Forget(e.revUID) // changed elsewhere; old content is stale
			}
			e.size, e.revUID = n.Size, n.RevUID
			if n.Mtime > 0 {
				e.mtime = time.UnixMilli(n.Mtime)
			}
		}
		if n.Ctime > 0 {
			e.ctime = time.UnixMilli(n.Ctime)
		}
		fresh[name] = e
	}
	kept := make(map[*entry]bool, len(fresh))
	for _, e := range fresh {
		kept[e] = true
	}
	// Keep local-only state that the server doesn't know about yet.
	for name, c := range dir.children {
		if kept[c] {
			continue
		}
		if c.uid == "" || c.uploading || (c.local != nil && c.local.dirty) {
			if _, taken := fresh[name]; !taken {
				fresh[name] = c
			}
			continue
		}
		// Deleted or moved away elsewhere: drop its cached content.
		if !c.dir {
			f.cache.Forget(cacheRevOf(c))
		}
	}
	dir.children = fresh
	dir.childrenAt = time.Now()
}

func (f *FS) child(dir *entry, name string) (*entry, syscall.Errno) {
	if errno := f.refresh(dir, false); errno != 0 {
		return nil, errno
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c := dir.children[name]
	if c == nil {
		return nil, syscall.ENOENT
	}
	return c, 0
}

func writableDir(d *entry) syscall.Errno {
	if d.virtual != "" || d.uid == "" {
		return syscall.EACCES
	}
	return 0
}

// ---------- attributes ----------

func (f *FS) sizeLocked(e *entry) int64 {
	if lf := e.local; lf != nil && (lf.dirty || lf.complete || e.uid == "" || e.uploading) {
		return lf.size
	}
	return e.size
}

func (f *FS) fillAttrLocked(e *entry, a *fuse.Attr) {
	a.Ino = e.ino
	if e.dir {
		a.Mode = syscall.S_IFDIR | 0o755
		if e.virtual != "" {
			a.Mode = syscall.S_IFDIR | 0o555
		}
		a.Nlink = 2
		a.Size = 4096
	} else {
		a.Mode = syscall.S_IFREG | 0o644
		a.Nlink = 1
		a.Size = uint64(f.sizeLocked(e))
	}
	a.Blocks = (a.Size + 511) / 512
	a.Blksize = 4096
	mt, ct := e.mtime, e.ctime
	if ct.IsZero() {
		ct = mt
	}
	a.SetTimes(&mt, &mt, &ct)
	a.Owner = fuse.Owner{Uid: f.uid, Gid: f.gid}
}

// ---------- local files ----------

func cacheRevOf(e *entry) string {
	if e.revUID != "" {
		return e.revUID
	}
	return "node:" + e.uid
}

// ensureLocalLocked returns the local file for e, starting a download if needed.
func (f *FS) ensureLocalLocked(e *entry) (*localFile, syscall.Errno) {
	if e.local != nil && e.local.err == nil {
		return e.local, 0
	}
	if e.local != nil {
		f.dropLocalLocked(e)
	}
	if e.uid == "" {
		return f.newEmptyLocalLocked(e)
	}
	rev := cacheRevOf(e)
	if p := f.cache.Acquire(rev); p != "" {
		fh, err := os.OpenFile(p, os.O_RDWR, 0)
		if err == nil {
			st, _ := fh.Stat()
			e.local = &localFile{f: fh, path: p, cacheRev: rev, size: st.Size(), have: st.Size(), complete: true}
			return e.local, 0
		}
		f.cache.Release(rev)
	}
	part := f.cache.PartPath(rev)
	fh, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, syscall.EIO
	}
	ctx, cancel := context.WithCancel(bg())
	lf := &localFile{f: fh, path: part, size: e.size, cancel: cancel}
	e.local = lf
	go f.download(ctx, e, lf, e.uid, rev, part)
	return lf, 0
}

func (f *FS) newEmptyLocalLocked(e *entry) (*localFile, syscall.Errno) {
	p := f.cache.NewDirtyPath()
	fh, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, syscall.EIO
	}
	e.local = &localFile{f: fh, path: p, complete: true}
	return e.local, 0
}

func (f *FS) download(ctx context.Context, e *entry, lf *localFile, uid, rev, part string) {
	var res struct {
		Size int64 `json:"size"`
	}
	err := f.br.CallProgress(ctx, "download", map[string]any{"uid": uid, "path": part}, &res, func(b int64) {
		f.mu.Lock()
		if b > lf.have {
			lf.have = b
			if b > lf.size {
				lf.size = b
			}
		}
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.cond.Broadcast()
	lf.cancel = nil
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("download %s failed: %v", fullPath(e), err)
		}
		lf.err = err
		os.Remove(part)
		return
	}
	lf.have, lf.size, lf.complete = res.Size, res.Size, true
	if !lf.dirty {
		e.size = res.Size
		if p, err := f.cache.Commit(part, rev, res.Size); err == nil {
			lf.path, lf.cacheRev = p, rev
		}
	}
	if e.local != lf || (e.openCount == 0 && !lf.dirty) {
		f.closeLocalLocked(lf)
		if e.local == lf {
			e.local = nil
		}
	}
}

// waitLocked blocks until bytes [0,end) are present (end<0: whole file).
func (f *FS) waitLocked(lf *localFile, end int64) syscall.Errno {
	for !lf.complete && lf.err == nil && (end < 0 || lf.have < end) {
		f.cond.Wait()
	}
	if lf.err != nil && (end < 0 || lf.have < end) {
		return syscall.EIO
	}
	return 0
}

func (f *FS) closeLocalLocked(lf *localFile) {
	if lf.cancel != nil {
		lf.cancel()
		lf.cancel = nil
	}
	if lf.f != nil {
		lf.f.Close()
		lf.f = nil
	}
	if lf.cacheRev != "" {
		f.cache.Release(lf.cacheRev)
		lf.cacheRev = ""
	} else if lf.complete && !lf.dirty {
		os.Remove(lf.path) // uncommitted clean copy
	}
}

// dropLocalLocked forgets e's local copy; dirty data is discarded.
func (f *FS) dropLocalLocked(e *entry) {
	lf := e.local
	if lf == nil {
		return
	}
	e.local = nil
	dirty := lf.dirty || lf.cacheRev == ""
	f.closeLocalLocked(lf)
	if dirty {
		os.Remove(lf.path)
	}
}

func (f *FS) maybeReleaseLocked(e *entry) {
	if e.openCount > 0 || e.uploading || e.uploadTimer != nil || e.local == nil {
		return
	}
	if e.local.dirty && !e.removed {
		return
	}
	f.dropLocalLocked(e)
}

// makeWritableLocked ensures e has a complete, private (non-cache) copy.
func (f *FS) makeWritableLocked(e *entry) (*localFile, syscall.Errno) {
	lf, errno := f.ensureLocalLocked(e)
	if errno != 0 {
		return nil, errno
	}
	if errno := f.waitLocked(lf, -1); errno != 0 {
		return nil, errno
	}
	if lf != e.local {
		return f.makeWritableLocked(e)
	}
	if lf.cacheRev != "" {
		p, err := f.cache.Detach(lf.cacheRev)
		if err != nil {
			return nil, syscall.EIO
		}
		lf.path, lf.cacheRev = p, ""
	}
	return lf, 0
}

func (f *FS) truncateLocked(e *entry, size int64) syscall.Errno {
	f.stopTimerLocked(e)
	if size == 0 {
		f.dropLocalLocked(e)
		lf, errno := f.newEmptyLocalLocked(e)
		if errno != 0 {
			return errno
		}
		lf.dirty = true
	} else {
		lf, errno := f.makeWritableLocked(e)
		if errno != 0 {
			return errno
		}
		if err := lf.f.Truncate(size); err != nil {
			return syscall.EIO
		}
		lf.size, lf.have, lf.dirty = size, size, true
	}
	e.mtime = time.Now()
	if e.writers == 0 {
		f.scheduleUploadLocked(e)
	}
	return 0
}

// ---------- uploads ----------

func (f *FS) stopTimerLocked(e *entry) {
	if e.uploadTimer != nil {
		e.uploadTimer.Stop()
		e.uploadTimer = nil
	}
}

func (f *FS) scheduleUploadLocked(e *entry) {
	f.scheduleUploadAfterLocked(e, uploadDebounce)
}

func (f *FS) scheduleUploadAfterLocked(e *entry, d time.Duration) {
	if e.local == nil || !e.local.dirty || e.removed {
		return
	}
	f.stopTimerLocked(e)
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if e.uploadTimer != t {
			return
		}
		e.uploadTimer = nil
		if e.writers > 0 {
			return // the last writer's release reschedules
		}
		f.startUploadLocked(e)
	})
	e.uploadTimer = t
}

func (f *FS) startUploadLocked(e *entry) {
	lf := e.local
	if lf == nil || !lf.dirty || e.removed || e.parent == nil || e.parent.uid == "" {
		return
	}
	if e.uploading {
		e.reupload = true
		return
	}
	e.uploading = true
	lf.dirty = false
	args := map[string]any{"parentUid": e.parent.uid, "name": e.name, "path": lf.path, "mtime": e.mtime.UnixMilli()}
	if e.uid != "" {
		args["uid"] = e.uid
	}
	p := fullPath(e)
	f.uploads.Add(1)
	go func() {
		defer f.uploads.Done()
		var res struct {
			UID    string `json:"uid"`
			RevUID string `json:"revUid"`
			Size   int64  `json:"size"`
		}
		err := f.br.Call(bg(), "upload", args, &res)
		f.mu.Lock()
		defer f.mu.Unlock()
		defer f.cond.Broadcast()
		e.uploading = false
		if err != nil {
			lf.dirty = true
			e.failures++
			if e.failures >= maxUploadTries {
				log.Printf("UPLOAD FAILED %s (giving up after %d tries; data kept at %s): %v", p, e.failures, lf.path, err)
				return
			}
			log.Printf("upload %s failed (try %d, retrying): %v", p, e.failures, err)
			f.scheduleUploadAfterLocked(e, uploadRetry)
			return
		}
		e.failures = 0
		if e.revUID != res.RevUID {
			f.cache.Forget(e.revUID) // superseded by the revision we just uploaded
		}
		e.uid, e.revUID, e.size = res.UID, res.RevUID, res.Size
		log.Printf("uploaded %s (%d bytes)", p, res.Size)
		if e.removed {
			// Deleted locally while uploading: remove it remotely too.
			go f.remove(res.UID)
		}
		if !lf.dirty && e.local == lf && lf.f != nil {
			if np, err := f.cache.Commit(lf.path, res.RevUID, res.Size); err == nil {
				lf.path, lf.cacheRev = np, res.RevUID
			}
		}
		if e.reupload || lf.dirty {
			e.reupload = false
			if e.writers == 0 {
				f.scheduleUploadLocked(e)
			}
			return
		}
		f.maybeReleaseLocked(e)
	}()
}

func (f *FS) waitUploadLocked(e *entry) {
	for e.uploading {
		f.cond.Wait()
	}
}

func (f *FS) remove(uid string) error {
	op := "trash"
	if f.opts.PermanentDelete {
		op = "delete"
	}
	return f.br.Call(bg(), op, map[string]any{"uid": uid}, nil)
}

// FlushAll uploads everything pending and waits (used on shutdown).
func (f *FS) FlushAll() {
	f.mu.Lock()
	n := 0
	var walk func(d *entry)
	walk = func(d *entry) {
		for _, c := range d.children {
			if c.uploadTimer != nil || (c.local != nil && c.local.dirty && !c.uploading) {
				f.stopTimerLocked(c)
				if c.local != nil && c.local.dirty {
					f.startUploadLocked(c)
				}
			}
			if c.uploading {
				n++
			}
			if c.children != nil {
				walk(c)
			}
		}
	}
	walk(f.root)
	f.mu.Unlock()
	if n > 0 {
		log.Printf("waiting for %d upload(s)...", n)
	}
	f.uploads.Wait()
}

// ---------- go-fuse glue ----------

type node struct {
	fs.Inode
	fs *FS
	e  *entry
}

var (
	_ fs.NodeLookuper  = (*node)(nil)
	_ fs.NodeReaddirer = (*node)(nil)
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeSetattrer = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
	_ fs.NodeCreater   = (*node)(nil)
	_ fs.NodeMkdirer   = (*node)(nil)
	_ fs.NodeUnlinker  = (*node)(nil)
	_ fs.NodeRmdirer   = (*node)(nil)
	_ fs.NodeRenamer   = (*node)(nil)
	_ fs.NodeStatfser  = (*node)(nil)
)

func (n *node) inode(ctx context.Context, e *entry, out *fuse.EntryOut) *fs.Inode {
	n.fs.mu.Lock()
	n.fs.fillAttrLocked(e, &out.Attr)
	mode := uint32(syscall.S_IFREG)
	if e.dir {
		mode = syscall.S_IFDIR
	}
	ino := e.ino
	n.fs.mu.Unlock()
	return n.NewInode(ctx, &node{fs: n.fs, e: e}, fs.StableAttr{Mode: mode, Ino: ino})
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	c, errno := n.fs.child(n.e, name)
	if errno != 0 {
		return nil, errno
	}
	return n.inode(ctx, c, out), 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if errno := n.fs.refresh(n.e, false); errno != 0 {
		return nil, errno
	}
	n.fs.mu.Lock()
	defer n.fs.mu.Unlock()
	list := make([]fuse.DirEntry, 0, len(n.e.children))
	for name, c := range n.e.children {
		mode := uint32(syscall.S_IFREG)
		if c.dir {
			mode = syscall.S_IFDIR
		}
		list = append(list, fuse.DirEntry{Name: name, Mode: mode, Ino: c.ino})
	}
	return fs.NewListDirStream(list), 0
}

func (n *node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.fs.mu.Lock()
	defer n.fs.mu.Unlock()
	n.fs.fillAttrLocked(n.e, &out.Attr)
	return 0
}

func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	f := n.fs
	f.mu.Lock()
	defer f.mu.Unlock()
	e := n.e
	if size, ok := in.GetSize(); ok {
		if e.dir {
			return syscall.EISDIR
		}
		if e.parent == nil || e.parent.virtual == "root" {
			return syscall.EACCES
		}
		if errno := f.truncateLocked(e, int64(size)); errno != 0 {
			return errno
		}
	}
	if mt, ok := in.GetMTime(); ok {
		e.mtime = mt
	}
	f.fillAttrLocked(e, &out.Attr)
	return 0
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	f := n.fs
	f.mu.Lock()
	stale := time.Since(f.quotaAt) > time.Minute
	f.mu.Unlock()
	if stale {
		var q struct{ Max, Used int64 }
		if err := f.br.Call(ctx, "quota", nil, &q); err == nil {
			f.mu.Lock()
			f.quotaMax, f.quotaUsed, f.quotaAt = q.Max, q.Used, time.Now()
			f.mu.Unlock()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	const bs = 4096
	total := f.quotaMax
	if total <= 0 {
		total = 1 << 40
	}
	out.Bsize, out.Frsize, out.NameLen = bs, bs, 255
	out.Blocks = uint64(total / bs)
	used := uint64(f.quotaUsed / bs)
	if used > out.Blocks {
		used = out.Blocks
	}
	out.Bfree, out.Bavail = out.Blocks-used, out.Blocks-used
	out.Files, out.Ffree = 1<<20, 1<<20
	return 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f := n.fs
	e := n.e
	if e.dir {
		return nil, 0, syscall.EISDIR
	}
	writing := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	f.mu.Lock()
	defer f.mu.Unlock()
	if writing {
		if e.parent == nil || e.parent.virtual == "root" {
			return nil, 0, syscall.EACCES
		}
		f.stopTimerLocked(e)
		if flags&syscall.O_TRUNC != 0 {
			if errno := f.truncateLocked(e, 0); errno != 0 {
				return nil, 0, errno
			}
			f.stopTimerLocked(e)
		} else if _, errno := f.ensureLocalLocked(e); errno != 0 {
			return nil, 0, errno
		}
		e.writers++
	} else if _, errno := f.ensureLocalLocked(e); errno != 0 {
		return nil, 0, errno
	}
	e.openCount++
	return &handle{fs: f, e: e, writing: writing}, 0, 0
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	f := n.fs
	if errno := writableDir(n.e); errno != 0 {
		return nil, nil, 0, errno
	}
	if errno := f.refresh(n.e, false); errno != 0 {
		return nil, nil, 0, errno
	}
	f.mu.Lock()
	e := n.e.children[name]
	if e != nil && e.dir {
		f.mu.Unlock()
		return nil, nil, 0, syscall.EISDIR
	}
	if e == nil {
		now := time.Now()
		e = &entry{ino: f.newIno(), name: name, parent: n.e, mtime: now, ctime: now}
		n.e.children[name] = e
	}
	if errno := f.truncateLocked(e, 0); errno != 0 {
		f.mu.Unlock()
		return nil, nil, 0, errno
	}
	f.stopTimerLocked(e)
	e.writers++
	e.openCount++
	f.mu.Unlock()
	return n.inode(ctx, e, out), &handle{fs: f, e: e, writing: true}, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	f := n.fs
	if errno := writableDir(n.e); errno != 0 {
		return nil, errno
	}
	if c, _ := f.child(n.e, name); c != nil {
		return nil, syscall.EEXIST
	}
	var rn remoteNode
	if err := f.br.Call(bg(), "mkdir", map[string]any{"parentUid": n.e.uid, "name": name}, &rn); err != nil {
		return nil, bridge.Errno(err)
	}
	f.mu.Lock()
	now := time.Now()
	e := &entry{ino: f.newIno(), name: name, parent: n.e, dir: true, uid: rn.UID, mtime: now, ctime: now,
		children: map[string]*entry{}, childrenAt: now}
	n.e.children[name] = e
	f.mu.Unlock()
	return n.inode(ctx, e, out), 0
}

// removeEntry deletes c remotely (trash by default) and from the tree.
func (f *FS) removeEntry(c *entry) syscall.Errno {
	f.mu.Lock()
	f.stopTimerLocked(c)
	uid, uploading := c.uid, c.uploading
	c.removed = true
	f.mu.Unlock()
	if uid != "" && !uploading {
		if err := f.remove(uid); err != nil {
			f.mu.Lock()
			c.removed = false
			f.mu.Unlock()
			return bridge.Errno(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.parent != nil && c.parent.children[c.name] == c {
		delete(c.parent.children, c.name)
	}
	if !c.dir && c.uid != "" {
		f.cache.Forget(cacheRevOf(c))
	}
	if c.openCount == 0 {
		f.dropLocalLocked(c)
	}
	return 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	if errno := writableDir(n.e); errno != 0 {
		return errno
	}
	c, errno := n.fs.child(n.e, name)
	if errno != 0 {
		return errno
	}
	if c.dir {
		return syscall.EISDIR
	}
	return n.fs.removeEntry(c)
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if errno := writableDir(n.e); errno != 0 {
		return errno
	}
	c, errno := n.fs.child(n.e, name)
	if errno != 0 {
		return errno
	}
	if !c.dir {
		return syscall.ENOTDIR
	}
	if errno := n.fs.refresh(c, true); errno != 0 {
		return errno
	}
	n.fs.mu.Lock()
	empty := len(c.children) == 0
	n.fs.mu.Unlock()
	if !empty {
		return syscall.ENOTEMPTY
	}
	return n.fs.removeEntry(c)
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	f := n.fs
	np, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	if flags&renameExchange != 0 {
		return syscall.EINVAL
	}
	if errno := writableDir(n.e); errno != 0 {
		return errno
	}
	if errno := writableDir(np.e); errno != 0 {
		return errno
	}
	e, errno := f.child(n.e, name)
	if errno != 0 {
		return errno
	}
	existing, _ := f.child(np.e, newName)
	if existing == e {
		return 0
	}
	if existing != nil {
		if flags&renameNoReplace != 0 {
			return syscall.EEXIST
		}
		if existing.dir && !e.dir {
			return syscall.EISDIR
		}
		if !existing.dir && e.dir {
			return syscall.ENOTDIR
		}
		if existing.dir {
			if errno := f.refresh(existing, true); errno != 0 {
				return errno
			}
			f.mu.Lock()
			nonEmpty := len(existing.children) > 0
			f.mu.Unlock()
			if nonEmpty {
				return syscall.ENOTEMPTY
			}
		}
		if errno := f.removeEntry(existing); errno != 0 {
			return errno
		}
	}

	f.mu.Lock()
	f.waitUploadLocked(e) // a first upload must land before we move it
	uid := e.uid
	oldParent := e.parent
	f.mu.Unlock()

	if uid != "" {
		if np.e != oldParent {
			if err := f.br.Call(bg(), "move", map[string]any{"uid": uid, "parentUid": np.e.uid}, nil); err != nil {
				return bridge.Errno(err)
			}
		}
		if newName != name {
			if err := f.br.Call(bg(), "rename", map[string]any{"uid": uid, "name": newName}, nil); err != nil {
				return bridge.Errno(err)
			}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if oldParent.children[name] == e {
		delete(oldParent.children, name)
	}
	e.parent, e.name = np.e, newName
	if np.e.children == nil {
		np.e.children = map[string]*entry{}
	}
	np.e.children[newName] = e
	return 0
}

// ---------- file handles ----------

type handle struct {
	fs      *FS
	e       *entry
	writing bool
}

var (
	_ fs.FileReader   = (*handle)(nil)
	_ fs.FileWriter   = (*handle)(nil)
	_ fs.FileReleaser = (*handle)(nil)
	_ fs.FileFlusher  = (*handle)(nil)
	_ fs.FileFsyncer  = (*handle)(nil)
)

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f, e := h.fs, h.e
	f.mu.Lock()
	lf, errno := f.ensureLocalLocked(e)
	if errno != 0 {
		f.mu.Unlock()
		return nil, errno
	}
	end := off + int64(len(dest))
	if lf.complete || lf.dirty {
		end = min(end, lf.size)
	} else {
		end = min(end, max(e.size, lf.size))
	}
	if off >= end {
		f.mu.Unlock()
		return fuse.ReadResultData(nil), 0
	}
	if errno := f.waitLocked(lf, end); errno != 0 {
		f.mu.Unlock()
		return nil, errno
	}
	if lf.complete {
		end = min(end, lf.size)
	}
	fh := lf.f
	f.mu.Unlock()
	if fh == nil || off >= end {
		return fuse.ReadResultData(nil), 0
	}
	nr, err := fh.ReadAt(dest[:end-off], off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:nr]), 0
}

func (h *handle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f, e := h.fs, h.e
	f.mu.Lock()
	lf, errno := f.makeWritableLocked(e)
	if errno != 0 {
		f.mu.Unlock()
		return 0, errno
	}
	fh := lf.f
	f.mu.Unlock()
	nw, err := fh.WriteAt(data, off)
	f.mu.Lock()
	defer f.mu.Unlock()
	if nw > 0 {
		if end := off + int64(nw); end > lf.size {
			lf.size, lf.have = end, end
		}
		lf.dirty = true
		e.mtime = time.Now()
	}
	if err != nil {
		return uint32(nw), syscall.EIO
	}
	return uint32(nw), 0
}

func (h *handle) Flush(ctx context.Context) syscall.Errno { return 0 }

// Fsync uploads now and waits, so `sync`-style callers get real durability.
func (h *handle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	f, e := h.fs, h.e
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitUploadLocked(e)
	if e.local == nil || !e.local.dirty {
		return 0
	}
	f.stopTimerLocked(e)
	f.startUploadLocked(e)
	f.waitUploadLocked(e)
	if e.local != nil && e.local.dirty {
		return syscall.EIO
	}
	return 0
}

func (h *handle) Release(ctx context.Context) syscall.Errno {
	f, e := h.fs, h.e
	f.mu.Lock()
	defer f.mu.Unlock()
	e.openCount--
	if h.writing {
		e.writers--
	}
	if e.writers == 0 && e.local != nil && e.local.dirty && !e.removed {
		f.scheduleUploadLocked(e)
		return 0
	}
	f.maybeReleaseLocked(e)
	return 0
}
