// SPDX-License-Identifier: MIT

package actionscache

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
)

func newCache(t *testing.T, max int64) (*Cache, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	c, err := New(Config{Dir: t.TempDir(), Max: max, Now: func() time.Time { return now }, Log: zerowrap.Logger{}})
	if err != nil {
		t.Fatal(err)
	}
	return c, &now
}

func keyOf(t *testing.T, n int) string {
	t.Helper()
	k, ok := Key("https://github.com", "o/r"+strings.Repeat("x", n), strings.Repeat("a", 40))
	if !ok {
		t.Fatal("key")
	}
	return k
}

func body(s string) Fetch {
	return func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, s); return err }
}

func read(t *testing.T, c *Cache, key string) string {
	t.Helper()
	f, err := c.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	return string(b)
}

func TestHitMiss(t *testing.T) {
	c, _ := newCache(t, 1<<20)
	k := keyOf(t, 0)
	var fetches int
	f := func(ctx context.Context, w io.Writer) error { fetches++; return body("archive")(ctx, w) }
	if hit, err := c.Ensure(context.Background(), k, f); err != nil || hit {
		t.Fatalf("first: hit=%v err=%v", hit, err)
	}
	if hit, err := c.Ensure(context.Background(), k, f); err != nil || !hit {
		t.Fatalf("second: hit=%v err=%v", hit, err)
	}
	if fetches != 1 || read(t, c, k) != "archive" {
		t.Fatalf("fetches=%d", fetches)
	}
	// A new process finds the entry.
	c2, err := New(Config{Dir: c.dir, Max: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if hit, _ := c2.Ensure(context.Background(), k, f); !hit || fetches != 1 {
		t.Fatalf("restart: hit=%v fetches=%d", hit, fetches)
	}
}

func TestFailedFetchLeavesNothing(t *testing.T) {
	c, _ := newCache(t, 1<<20)
	k := keyOf(t, 0)
	boom := errors.New("429")
	_, err := c.Ensure(context.Background(), k, func(_ context.Context, w io.Writer) error { _, _ = io.WriteString(w, "part"); return boom })
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := c.Open(k); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed download must not be an entry")
	}
	des, _ := os.ReadDir(c.dir)
	if len(des) != 0 {
		t.Fatalf("leftovers: %v", des)
	}
}

func TestConcurrentSingleFlight(t *testing.T) {
	c, _ := newCache(t, 1<<20)
	k := keyOf(t, 0)
	var fetches atomic.Int32
	release := make(chan struct{})
	f := func(ctx context.Context, w io.Writer) error {
		fetches.Add(1)
		<-release
		return body("once")(ctx, w)
	}
	var wg sync.WaitGroup
	var hits atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hit, err := c.Ensure(context.Background(), k, f)
			if err != nil {
				t.Error(err)
			}
			if hit {
				hits.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("%d downloads for one key", fetches.Load())
	}
	if hits.Load() != 7 {
		t.Errorf("%d hits, want 7 (all but the downloader)", hits.Load())
	}
}

func TestCorruptEntryIsDroppedAndRefetched(t *testing.T) {
	c, _ := newCache(t, 1<<20)
	k := keyOf(t, 0)
	if _, err := c.Ensure(context.Background(), k, body("good bytes")); err != nil {
		t.Fatal(err)
	}
	// Same size, other content: only the hash can tell.
	if err := os.WriteFile(c.archivePath(k), []byte("evil bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a corrupt entry was served: %v", err)
	}
	if c.Size() != 0 {
		t.Errorf("size %d after the drop", c.Size())
	}
	hit, err := c.Ensure(context.Background(), k, body("good bytes"))
	if err != nil || hit {
		t.Fatalf("hit=%v err=%v", hit, err)
	}
	if read(t, c, k) != "good bytes" {
		t.Fatal("not refetched")
	}
	// Truncation is caught too.
	if err := os.WriteFile(c.archivePath(k), []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(k); !errors.Is(err, ErrNotFound) {
		t.Fatal("a truncated entry was served")
	}
}

func TestLRUEviction(t *testing.T) {
	c, now := newCache(t, 25)
	ctx := context.Background()
	step := func() { *now = now.Add(time.Hour + PinFor) } // past every pin
	a, b, d := keyOf(t, 0), keyOf(t, 1), keyOf(t, 2)
	for _, k := range []string{a, b} {
		if _, err := c.Ensure(ctx, k, body(strings.Repeat("x", 10))); err != nil {
			t.Fatal(err)
		}
		step()
	}
	// Use a again: b is now the least recently used.
	if hit, _ := c.Ensure(ctx, a, body("unused")); !hit {
		t.Fatal("a should hit")
	}
	step()
	if _, err := c.Ensure(ctx, d, body(strings.Repeat("y", 10))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(b); !errors.Is(err, ErrNotFound) {
		t.Error("the least recently used entry survived")
	}
	for _, k := range []string{a, d} {
		if _, err := c.Open(k); err != nil {
			t.Errorf("%s evicted: %v", k[:6], err)
		}
	}
	if c.Size() > 25 {
		t.Errorf("size %d over the cap", c.Size())
	}
}

// A pinned entry is not evicted, even to make room: the newcomer is the one
// that is not cached, so the cap holds.
func TestPinnedEntryIsNotEvicted(t *testing.T) {
	c, _ := newCache(t, 15)
	ctx := context.Background()
	a, b := keyOf(t, 0), keyOf(t, 1)
	if _, err := c.Ensure(ctx, a, body(strings.Repeat("x", 10))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(ctx, b, body(strings.Repeat("y", 10))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Open(a); err != nil {
		t.Fatalf("a pinned entry was evicted: %v", err)
	}
	if c.Size() > 15 {
		t.Fatalf("size %d", c.Size())
	}
}

// diskUse is the bytes of every file in the cache directory, temporary files
// included.
func diskUse(t *testing.T, c *Cache) int64 {
	t.Helper()
	var n int64
	des, _ := os.ReadDir(c.dir)
	for _, de := range des {
		if fi, err := de.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// Many downloads in flight at once, each below the cap, are charged as they
// are written: the directory never holds more than the cap, temporary files
// included, and the accounting returns to the bytes of the entries.
func TestConcurrentDownloadsStayUnderTheCap(t *testing.T) {
	const max = 100
	c, _ := newCache(t, max)
	var over atomic.Int32
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				// The charge covers the entries and the downloads in progress. (The
				// directory cannot be sampled atomically: a listing taken while an
				// entry is evicted and a download grows would count both.)
				c.mu.Lock()
				n := c.total + c.pending
				c.mu.Unlock()
				if n > max {
					over.Add(1)
				}
			}
		}
	}()
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := keyOf(t, i)
			_, _ = c.Ensure(context.Background(), k, func(_ context.Context, w io.Writer) error {
				for range 6 { // 6 x 10 bytes, in pieces
					if _, err := w.Write([]byte(strings.Repeat("z", 10))); err != nil {
						return err
					}
					time.Sleep(time.Millisecond)
				}
				return nil
			})
		}()
	}
	wg.Wait()
	close(stop)
	if over.Load() != 0 {
		t.Fatal("entries and downloads in flight went over the cap")
	}
	var onDisk int64
	des, _ := os.ReadDir(c.dir)
	for _, de := range des {
		if strings.HasPrefix(de.Name(), tmpPrefix) {
			t.Errorf("temporary file left: %s", de.Name())
		}
		if strings.HasSuffix(de.Name(), archiveExt) {
			fi, _ := de.Info()
			onDisk += fi.Size()
		}
	}
	if onDisk > max {
		t.Fatalf("%d bytes on disk", onDisk)
	}
	c.mu.Lock()
	pending, total := c.pending, c.total
	c.mu.Unlock()
	if pending != 0 || total > max {
		t.Fatalf("pending=%d total=%d", pending, total)
	}
	var sum int64
	for _, e := range c.entries {
		sum += e.size
	}
	if sum != total {
		t.Fatalf("entries hold %d, accounted %d", sum, total)
	}
}

// The bytes of a download in progress are counted, and a failed or refused one
// leaves neither a file nor a charge.
func TestTemporaryFilesAreCountedAndCleaned(t *testing.T) {
	c, _ := newCache(t, 50)
	seen := int64(-1)
	_, err := c.Ensure(context.Background(), keyOf(t, 0), func(_ context.Context, w io.Writer) error {
		_, _ = io.WriteString(w, strings.Repeat("a", 30))
		c.mu.Lock()
		seen = c.pending
		c.mu.Unlock()
		return errors.New("connection reset")
	})
	if err == nil || seen != 30 {
		t.Fatalf("err=%v pending during=%d", err, seen)
	}
	// Refused midway for size: the part already written is released too.
	_, err = c.Ensure(context.Background(), keyOf(t, 1), body(strings.Repeat("b", 60)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if c.pending != 0 || c.Size() != 0 || diskUse(t, c) != 0 {
		t.Fatalf("pending=%d size=%d disk=%d", c.pending, c.Size(), diskUse(t, c))
	}
	// The size is what was written, whatever the server announced.
	if _, err := c.Ensure(context.Background(), keyOf(t, 2), body(strings.Repeat("c", 50))); err != nil {
		t.Fatal(err)
	}
	if c.Size() != 50 {
		t.Fatalf("size %d", c.Size())
	}
}

func TestTooLarge(t *testing.T) {
	c, _ := newCache(t, 10)
	_, err := c.Ensure(context.Background(), keyOf(t, 0), body(strings.Repeat("x", 11)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if c.Size() != 0 || diskUse(t, c) != 0 {
		t.Fatalf("an oversize archive left %d bytes on disk", diskUse(t, c))
	}
}

func TestStartDropsHalfWrittenFiles(t *testing.T) {
	c, _ := newCache(t, 100)
	k := keyOf(t, 0)
	_ = os.WriteFile(c.archivePath(k), []byte("no sum"), 0o600)
	_ = os.WriteFile(c.dir+"/tmp-123", []byte("x"), 0o600)
	_ = os.WriteFile(c.sumPath(keyOf(t, 1)), []byte("sum without archive"), 0o600)
	if _, err := New(Config{Dir: c.dir, Max: 100}); err != nil {
		t.Fatal(err)
	}
	if des, _ := os.ReadDir(c.dir); len(des) != 0 {
		t.Fatalf("leftovers: %v", des)
	}
}

// The key is made of the forge, the repository and the commit, never a ref: a
// tag or a branch name is not a sha and is refused, and two commits or two
// forges never share an entry.
func TestKeyNeverUsesARef(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, bad := range []string{"", "v4", "main", "refs/tags/v4", strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 41)} {
		if _, ok := Key("https://github.com", "o/r", bad); ok {
			t.Errorf("%q made a key", bad)
		}
	}
	k1, _ := Key("https://github.com", "o/r", sha)
	k2, _ := Key("https://github.com", "o/r", strings.Repeat("b", 40))
	k3, _ := Key("https://ermite.example", "o/r", sha)
	k4, _ := Key("https://github.com", "O/R", strings.ToUpper(sha))
	if k1 == k2 || k1 == k3 {
		t.Error("collision")
	}
	if k1 != k4 {
		t.Error("the case of the repository or the sha must not split an entry")
	}
	if !ValidKey(k1) {
		t.Error("shape")
	}
}

func TestParseMax(t *testing.T) {
	for in, want := range map[string]int64{"": DefaultMax, "0": 0, "500M": 500 << 20, "2G": 2 << 30, "2GiB": 2 << 30, "1024": 1024} {
		if got, err := ParseMax(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"-1", "x", "G"} {
		if _, err := ParseMax(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
