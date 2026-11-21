package pfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cache is an on-disk LRU of file contents.
//
//	rev-<hash>      complete, clean content of one file revision (evictable)
//	rev-<hash>.part download in progress
//	dirty-<random>  locally modified content not yet uploaded (never evicted)
//	lost+found/     unsaved data recovered after a crash (left for the user)
//
// Clean files are keyed by revision UID, so they stay valid across remounts.
// A revision is removed when the cache exceeds Max (least recently used
// first), when it hasn't been used for MaxAge, or when it is Forgotten
// because the file was deleted or replaced. Pinned revisions (open files)
// are never removed while pinned.
type Cache struct {
	Dir    string
	Max    int64
	MaxAge time.Duration // 0: no age limit

	mu    sync.Mutex
	items map[string]*cacheItem // key: file name (rev-<hash>)
	total int64
}

type cacheItem struct {
	size     int64
	lastUsed time.Time
	refs     int
	doomed   bool // delete as soon as it is unpinned
}

// LostFound is the directory, inside a cache dir, holding unsaved data
// recovered after a crash.
const LostFound = "lost+found"

func OpenCache(dir string, max int64, maxAge time.Duration) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &Cache{Dir: dir, Max: max, MaxAge: maxAge, items: map[string]*cacheItem{}}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	lf := filepath.Join(dir, LostFound)
	for _, de := range ents {
		name := de.Name()
		p := filepath.Join(dir, name)
		switch {
		case strings.HasSuffix(name, ".part"):
			os.Remove(p)
		case strings.HasPrefix(name, "dirty-"):
			// Unsaved data from a crash: keep it where the user can find it.
			os.MkdirAll(lf, 0o700)
			os.Rename(p, filepath.Join(lf, name))
		case strings.HasPrefix(name, "rev-"):
			if info, err := de.Info(); err == nil {
				c.items[name] = &cacheItem{size: info.Size(), lastUsed: info.ModTime()}
				c.total += info.Size()
			}
		}
	}
	if n, size := DirFiles(lf); n > 0 {
		log.Printf("WARNING: %d file(s) (%d bytes) of unsaved changes from an earlier crash are in %s; "+
			"protonfs can't tell which Drive files they belong to. Recover what you need, then delete them.", n, size, lf)
	}
	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()
	if maxAge > 0 {
		go c.sweepLoop()
	}
	return c, nil
}

// DirFiles counts the regular files directly in dir and their total size.
func DirFiles(dir string) (n int, size int64) {
	ents, _ := os.ReadDir(dir)
	for _, de := range ents {
		if info, err := de.Info(); err == nil && info.Mode().IsRegular() {
			n++
			size += info.Size()
		}
	}
	return n, size
}

func (c *Cache) sweepLoop() {
	every := c.MaxAge / 24
	if every < time.Minute {
		every = time.Minute
	}
	if every > time.Hour {
		every = time.Hour
	}
	for range time.Tick(every) {
		c.mu.Lock()
		c.evictLocked()
		c.mu.Unlock()
	}
}

func revKey(revUID string) string {
	h := sha256.Sum256([]byte(revUID))
	return "rev-" + hex.EncodeToString(h[:16])
}

func (c *Cache) path(key string) string { return filepath.Join(c.Dir, key) }

// Acquire returns the path of a complete cached revision and pins it, or "".
func (c *Cache) Acquire(revUID string) string {
	if revUID == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := revKey(revUID)
	it := c.items[key]
	if it == nil {
		return ""
	}
	it.refs++
	it.doomed = false // wanted again after all
	it.lastUsed = time.Now()
	p := c.path(key)
	os.Chtimes(p, it.lastUsed, it.lastUsed)
	return p
}

// Release unpins a revision acquired with Acquire or Commit.
func (c *Cache) Release(revUID string) {
	if revUID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := revKey(revUID)
	if it := c.items[key]; it != nil && it.refs > 0 {
		it.refs--
		it.lastUsed = time.Now()
		if it.refs == 0 && it.doomed {
			c.removeLocked(key)
		}
	}
	c.evictLocked()
}

// PartPath is where a download of revUID is written before Commit.
func (c *Cache) PartPath(revUID string) string { return c.path(revKey(revUID)) + ".part" }

// NewDirtyPath returns a fresh path for locally modified content.
func (c *Cache) NewDirtyPath() string {
	b := make([]byte, 12)
	rand.Read(b)
	return c.path("dirty-" + hex.EncodeToString(b))
}

// Commit moves a finished file (a .part or uploaded dirty file) into the cache
// as revUID, pinned. Returns the new path.
func (c *Cache) Commit(from, revUID string, size int64) (string, error) {
	key := revKey(revUID)
	to := c.path(key)
	if err := os.Rename(from, to); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.items[key]; old != nil {
		c.total -= old.size
	}
	c.items[key] = &cacheItem{size: size, lastUsed: time.Now(), refs: 1}
	c.total += size
	c.evictLocked()
	return to, nil
}

// Detach removes a revision from the index without deleting the file, moving
// it to a dirty path (used when a cached file is about to be modified).
func (c *Cache) Detach(revUID string) (string, error) {
	key := revKey(revUID)
	to := c.NewDirtyPath()
	if err := os.Rename(c.path(key), to); err != nil {
		return "", err
	}
	c.mu.Lock()
	if it := c.items[key]; it != nil {
		c.total -= it.size
		delete(c.items, key)
	}
	c.mu.Unlock()
	return to, nil
}

// Forget drops a revision that is no longer needed (its file was deleted or
// replaced by a newer revision). If it is pinned, it is dropped on release.
func (c *Cache) Forget(revUID string) {
	if revUID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := revKey(revUID)
	if it := c.items[key]; it != nil {
		if it.refs > 0 {
			it.doomed = true
		} else {
			c.removeLocked(key)
		}
	}
}

func (c *Cache) removeLocked(key string) {
	it := c.items[key]
	os.Remove(c.path(key))
	c.total -= it.size
	delete(c.items, key)
}

func (c *Cache) evictLocked() {
	if c.MaxAge > 0 {
		cutoff := time.Now().Add(-c.MaxAge)
		for k, it := range c.items {
			if it.refs == 0 && it.lastUsed.Before(cutoff) {
				c.removeLocked(k)
			}
		}
	}
	if c.total <= c.Max {
		return
	}
	keys := make([]string, 0, len(c.items))
	for k := range c.items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return c.items[keys[i]].lastUsed.Before(c.items[keys[j]].lastUsed) })
	for _, k := range keys {
		if c.total <= c.Max {
			return
		}
		if c.items[k].refs > 0 {
			continue
		}
		c.removeLocked(k)
	}
}

// Usage reports bytes held by clean cached revisions.
func (c *Cache) Usage() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
