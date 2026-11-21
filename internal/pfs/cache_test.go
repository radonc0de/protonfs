package pfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// put commits a file of n bytes as revUID and unpins it.
func put(t *testing.T, c *Cache, revUID string, n int) {
	t.Helper()
	p := c.PartPath(revUID)
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Commit(p, revUID, int64(n)); err != nil {
		t.Fatal(err)
	}
	c.Release(revUID)
	time.Sleep(2 * time.Millisecond) // distinct lastUsed
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c, err := OpenCache(t.TempDir(), 300)
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "a", 100)
	put(t, c, "b", 100)
	put(t, c, "c", 100)

	// Touch a, so b becomes the oldest.
	if c.Acquire("a") == "" {
		t.Fatal("a missing")
	}
	c.Release("a")
	time.Sleep(2 * time.Millisecond)

	put(t, c, "d", 100)
	if c.Acquire("b") != "" {
		t.Error("b should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if c.Acquire(k) == "" {
			t.Errorf("%s should still be cached", k)
		}
	}
	if got := c.Usage(); got != 300 {
		t.Errorf("usage = %d, want 300", got)
	}
}

func TestCacheNeverEvictsPinned(t *testing.T) {
	c, err := OpenCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "a", 100)
	c.Acquire("a") // pin
	put(t, c, "b", 100)
	if c.Acquire("a") == "" {
		t.Error("pinned a was evicted")
	}
}

func TestCacheReopenRecoversState(t *testing.T) {
	dir := t.TempDir()
	c, _ := OpenCache(dir, 1000)
	put(t, c, "a", 10)
	os.WriteFile(c.PartPath("partial"), []byte("x"), 0o600)
	os.WriteFile(c.NewDirtyPath(), []byte("unsaved"), 0o600)

	c2, err := OpenCache(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Acquire("a") == "" {
		t.Error("committed revision lost on reopen")
	}
	if _, err := os.Stat(c2.PartPath("partial")); !os.IsNotExist(err) {
		t.Error(".part file not cleaned up")
	}
	lf, _ := os.ReadDir(filepath.Join(dir, "lost+found"))
	if len(lf) != 1 {
		t.Errorf("lost+found has %d files, want 1", len(lf))
	}
}
