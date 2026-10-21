// SPDX-License-Identifier: MIT

package registryproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reopening a store keeps its content, charges it again and clears the
// incomplete downloads in tmp/.
func TestStoreReopen(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, storeCfg(dir, 8*minCharge))
	put(t, s, "aaaa")
	putManifest(t, s, manifestBody, "application/vnd.oci.image.index.v1+json")
	if err := s.setTag("library/alpine", "3", normalizeAccept(""), sum(manifestBody)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "leftover"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	total := s.total
	s = mustOpen(t, storeCfg(dir, 8*minCharge))
	f, _, err := s.open(kindBlob, sum("aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := s.tag("library/alpine", "3", normalizeAccept("")); got != sum(manifestBody) {
		t.Fatalf("tag after reopen: %q", got)
	}
	if ds, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(ds) != 0 {
		t.Fatal("tmp not cleared")
	}
	if accounted(t, s); s.total != total {
		t.Fatalf("reopened total %d, was %d", s.total, total)
	}
}

// Every entry is charged at least minCharge, on commit and on reopen, so tiny
// blobs cannot make more than Max/minCharge files.
func TestEntriesChargedAtLeastMinCharge(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, storeCfg(dir, 3*minCharge))
	for i := range 10 {
		put(t, s, fmt.Sprint("tiny-", i))
	}
	accounted(t, s)
	if n := len(s.entries); n > 3 {
		t.Fatalf("%d entries under a cap of 3 charges", n)
	}
	s = mustOpen(t, storeCfg(dir, 3*minCharge))
	accounted(t, s)
	if s.total != int64(len(s.entries))*minCharge {
		t.Fatalf("reopened total %d for %d entries", s.total, len(s.entries))
	}
}

// A cap below one entry's minimum charge is refused; a blob larger than Max is
// refused and leaves nothing charged.
func TestStoreOversize(t *testing.T) {
	if _, err := openStore(storeCfg(t.TempDir(), minCharge-1)); err == nil {
		t.Fatal("cap below minCharge accepted")
	}
	s := mustOpen(t, storeCfg(t.TempDir(), minCharge))
	large := strings.Repeat("x", int(minCharge)+1)
	_, _, _, err := s.ensure(context.Background(), kindBlob, sum(large), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, large)
		return "", err
	}, keepalive{})
	if err != errTooLarge {
		t.Fatal(err)
	}
	if s.pending != 0 || s.total != 0 {
		t.Fatalf("accounting: %d %d", s.pending, s.total)
	}
}

// Eviction walks the LRU, evicts least recently used first, frees down to the
// low-water mark rather than the bare need, and skips pinned entries.
func TestEvictionLRUAndLowWater(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const n = 40
	c := storeCfg(t.TempDir(), n*minCharge) // low water: 5%, two entries
	c.now = func() time.Time { return now }
	s := mustOpen(t, c)
	// 40 blobs fill the store. Freeing only the need would evict one blob; the
	// low-water mark evicts two.
	blob := func(i int) string { return fmt.Sprintf("%03d", i) + strings.Repeat("x", 47) }
	for i := range n {
		now = now.Add(time.Second)
		put(t, s, blob(i))
	}
	// Touch 0 and 1, and keep 2 open: the least recently used are then 3, 4...
	for _, i := range []int{0, 1} {
		now = now.Add(time.Second)
		f, _, err := s.open(kindBlob, sum(blob(i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	pinned, _, err := s.open(kindBlob, sum(blob(2)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pinned.Close() }()
	s.mu.Lock()
	s.lru.MoveToFront(s.entries[kindBlob.key(sum(blob(2))[7:])].elem) // oldest of all, but open
	s.mu.Unlock()
	now = now.Add(time.Second)
	put(t, s, "a") // over the cap: frees the low-water mark
	present := func(i int) bool {
		_, err := os.Stat(filepath.Join(s.dir, kindBlob.key(sum(blob(i))[7:])))
		return err == nil
	}
	for i := range n {
		want := i != 3 && i != 4 // 3 and 4 are the least recently used unpinned
		if present(i) != want {
			t.Errorf("blob %d present=%v", i, present(i))
		}
	}
	accounted(t, s)
	s.mu.Lock()
	free := s.max - s.total
	s.mu.Unlock()
	if free < min(lowWater, s.max/20)-minCharge {
		t.Fatalf("freed only to %d", free)
	}
}

// Eviction restarts from the front after a manifest removal that fails, and
// does not loop on it.
func TestEvictionAfterFailedManifestRemoval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := storeCfg(t.TempDir(), 1<<20)
	c.now = func() time.Time { return now }
	s := mustOpen(t, c)
	m := `{"schemaVersion":2}`
	putManifest(t, s, m, "application/vnd.oci.image.index.v1+json")
	now = now.Add(time.Second)
	if err := s.setTag("library/alpine", "3", "", sum(m)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	put(t, s, "blob")
	// The manifest's .type cannot be removed: replace it with a non-empty
	// directory. Its tag goes first all the same.
	typ := filepath.Join(s.dir, kindManifest.key(sum(m)[7:])+".type")
	if err := os.Remove(typ); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(typ, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.evictLocked(s.max) // ask for everything
	_, manifestKept := s.entries[kindManifest.key(sum(m)[7:])]
	_, blobKept := s.entries[kindBlob.key(sum("blob")[7:])]
	tags := len(s.tagsOf)
	s.mu.Unlock()
	if !manifestKept || blobKept || tags != 0 {
		t.Fatalf("manifest kept=%v blob kept=%v tags=%d", manifestKept, blobKept, tags)
	}
	accounted(t, s)
}

// The committed entry is pinned for its waiters, so an eviction between the
// commit and their open cannot remove it.
func TestCommittedEntryPinnedForWaiters(t *testing.T) {
	s := mustOpen(t, storeCfg(t.TempDir(), minCharge))
	key := kindBlob.key(sum("aaaa")[7:])
	s.hookCommitted = func(string) {
		// Ask for the whole store before the waiters are woken.
		s.mu.Lock()
		s.evictLocked(s.max)
		s.mu.Unlock()
	}
	f, _, _, err := s.ensure(context.Background(), kindBlob, sum("aaaa"), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, "aaaa")
		return "", err
	}, keepalive{})
	if err != nil {
		t.Fatalf("waiter lost its entry: %v", err)
	}
	s.mu.Lock()
	pins := s.entries[key].pins
	s.mu.Unlock()
	if pins != 1 {
		t.Fatalf("pins %d: the commit pin was not released to the reader's", pins)
	}
	_ = f.Close()
	s.hookCommitted = nil
	s.mu.Lock()
	s.evictLocked(s.max)
	_, kept := s.entries[key]
	s.mu.Unlock()
	if kept {
		t.Fatal("unpinned entry not evictable")
	}
}

// A blob that a response is still sending is pinned: a fill that needs its
// room fails, and succeeds once the response has closed the file.
func TestEvictionWhileServing(t *testing.T) {
	a, b := strings.Repeat("a", int(minCharge)), strings.Repeat("b", int(minCharge))
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, sum(a)):
			_, _ = io.WriteString(w, a)
		case strings.HasSuffix(r.URL.Path, sum(b)):
			_, _ = io.WriteString(w, b)
		default:
			http.NotFound(w, r)
		}
	})
	p := proxyWith(t, srv, Config{Max: minCharge * 3 / 2})
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(a)); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	bw := &blockingWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(bw, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/blobs/"+sum(a), nil))
	}()
	<-bw.started
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(b)); w.Code != http.StatusBadGateway {
		t.Fatalf("fill evicted a blob being served: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(p.s.dir, kindBlob.key(sum(a)[7:]))); err != nil {
		t.Fatalf("served blob removed: %v", err)
	}
	accounted(t, p.s)
	close(bw.release)
	<-done
	if bw.Code != http.StatusOK || bw.Body.String() != a {
		t.Fatalf("served: %d", bw.Code)
	}
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(b)); w.Code != http.StatusOK || w.Body.String() != b {
		t.Fatalf("after release: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(p.s.dir, kindBlob.key(sum(a)[7:]))); !os.IsNotExist(err) {
		t.Fatal("released blob not evicted")
	}
	accounted(t, p.s)
}

// Tag files share the LRU of the store, so they cannot grow the store past
// Max; reopening charges them again.
func TestTagsChargedAndEvicted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	c := storeCfg(dir, 4*minCharge) // the manifest and 3 tags
	c.now = func() time.Time { return now }
	s := mustOpen(t, c)
	putManifest(t, s, manifestBody, "application/vnd.oci.image.index.v1+json")
	for i := range 20 {
		now = now.Add(time.Second)
		if err := s.setTag("library/alpine", fmt.Sprint("t", i), "", sum(manifestBody)); err != nil {
			t.Fatalf("tag %d: %v", i, err)
		}
		if n := accounted(t, s); n > 3 {
			t.Fatalf("%d tags over a cap of 3", n)
		}
	}
	if s.tag("library/alpine", "t19", "") != sum(manifestBody) || s.tag("library/alpine", "t0", "") != "" {
		t.Fatal("tags not evicted least recently used first")
	}
	s = mustOpen(t, storeCfg(dir, 4*minCharge))
	if n := accounted(t, s); n != 3 {
		t.Fatalf("reopened with %d tags", n)
	}
}

// The tag index follows rewrites, and removing a manifest takes exactly its
// own tags along.
func TestTagIndex(t *testing.T) {
	s := mustOpen(t, storeCfg(t.TempDir(), 1<<20))
	manifests := []string{`{"schemaVersion":2,"a":1}`, `{"schemaVersion":2,"b":1}`}
	for _, m := range manifests {
		putManifest(t, s, m, "application/vnd.oci.image.index.v1+json")
	}
	for _, tag := range []string{"x", "y"} {
		if err := s.setTag("library/alpine", tag, "", sum(manifests[0])); err != nil {
			t.Fatal(err)
		}
	}
	// y moves to the second manifest.
	if err := s.setTag("library/alpine", "y", "", sum(manifests[1])); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if !s.removeLocked(kindManifest.key(sum(manifests[0])[7:])) {
		t.Fatal("remove failed")
	}
	n0, n1 := len(s.tagsOf[sum(manifests[0])]), len(s.tagsOf[sum(manifests[1])])
	s.mu.Unlock()
	if s.tag("library/alpine", "x", "") != "" || s.tag("library/alpine", "y", "") != sum(manifests[1]) || n0 != 0 || n1 != 1 {
		t.Fatalf("index: %d %d", n0, n1)
	}
	accounted(t, s)
}

// A .type file without its manifest is removed at startup.
func TestOrphanTypeRemoved(t *testing.T) {
	dir := t.TempDir()
	mustOpen(t, storeCfg(dir, 1<<20))
	orphan := filepath.Join(dir, kindManifest.key(sum("gone")[7:])+".type")
	if err := os.WriteFile(orphan, []byte("application/json"), 0600); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, storeCfg(dir, 1<<20))
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan .type kept")
	}
	accounted(t, s)
}
