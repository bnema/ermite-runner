// SPDX-License-Identifier: MIT
package registryproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
)

// stallingUpstream serves a blob's first bytes and then holds the body until
// the request is cancelled; it counts started and cancelled downloads.
type stallingUpstream struct {
	srv                *httptest.Server
	started, cancelled atomic.Int32
}

func newStallingUpstream(t *testing.T) *stallingUpstream {
	t.Helper()
	u := &stallingUpstream{}
	u.srv = upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		u.started.Add(1)
		w.Header().Set("Content-Length", "1000000")
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			u.cancelled.Add(1)
		case <-time.After(10 * time.Second):
		}
	})
	return u
}

func blobPath(i int) string { return "/v2/library/alpine/blobs/" + sum(fmt.Sprint("blob-", i)) }

// serveCancelled starts a request and cancels it once its download has begun.
func serveCancelled(t *testing.T, p *Proxy, u *stallingUpstream, path string, started int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	waitFor(t, p.s, "the download to start", func() bool { return u.started.Load() >= started })
	cancel()
	<-done
}

// MAJOR 1a: a guest that requests many digests and disconnects leaves no
// download behind: once a flight has no waiter for the grace period, it is
// cancelled upstream and its temporary file removed.
func TestAbandonedDownloadsCancelled(t *testing.T) {
	u := newStallingUpstream(t)
	const n = 20
	p := proxyWith(t, u.srv, Config{AbandonGrace: 20 * time.Millisecond, MaxDownloads: n})
	for i := range n {
		serveCancelled(t, p, u, blobPath(i), int32(i+1))
	}
	waitFor(t, p.s, "every flight to end", func() bool { return len(p.s.flights) == 0 && p.s.downloads == 0 && p.s.pending == 0 })
	for end := time.Now().Add(5 * time.Second); u.cancelled.Load() != n; {
		if time.Now().After(end) {
			t.Fatalf("upstream saw %d cancellations of %d", u.cancelled.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
	waitFor(t, p.s, "tmp/ to be empty", func() bool {
		ds, _ := os.ReadDir(filepath.Join(p.s.dir, "tmp"))
		return len(ds) == 0
	})
}

// MAJOR 1a: a request that joins within the grace period keeps the download.
func TestRejoinWithinGraceKeepsDownload(t *testing.T) {
	body := strings.Repeat("layer", 4000)
	release := make(chan struct{})
	var gets, cancelled atomic.Int32
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = io.WriteString(w, body[:10])
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, body[10:])
		case <-r.Context().Done():
			cancelled.Add(1)
		}
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	p := proxyWith(t, srv, Config{AbandonGrace: time.Hour})
	path := "/v2/library/alpine/blobs/" + sum(body)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	go func() {
		defer close(first)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	key := "blobs/" + sum(body)[7:]
	waitFor(t, p.s, "the first waiter", func() bool { return waiters(p.s, key) == 1 })
	cancel()
	<-first
	w := httptest.NewRecorder()
	second := make(chan struct{})
	go func() { defer close(second); p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil)) }()
	waitFor(t, p.s, "the second waiter", func() bool { return waiters(p.s, key) == 1 })
	once.Do(func() { close(release) })
	<-second
	if w.Code != http.StatusOK || w.Body.String() != body || gets.Load() != 1 || cancelled.Load() != 0 {
		t.Fatalf("code %d, %d fetches, %d cancelled", w.Code, gets.Load(), cancelled.Load())
	}
}

// MAJOR 1b: at most MaxDownloads cold blobs download at once; one more waits
// for SlotWait and then gets 503 (dockerd retries or pulls from Docker Hub),
// and a slot frees once a download ends.
func TestColdDownloadCap(t *testing.T) {
	u := newStallingUpstream(t)
	const limit = 3
	const wait = 50 * time.Millisecond
	p := proxyWith(t, u.srv, Config{AbandonGrace: 10 * time.Millisecond, MaxDownloads: limit, SlotWait: wait})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := range limit {
		wg.Go(func() {
			p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, blobPath(i), nil).WithContext(ctx))
		})
	}
	waitFor(t, p.s, "the cap to fill", func() bool { return p.s.downloads == limit })
	start := time.Now()
	w := request(t, p, http.MethodGet, blobPath(limit))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the cap: %d", w.Code)
	}
	if d := time.Since(start); d < wait {
		t.Fatalf("503 after %v, before the slot wait", d)
	}
	// Joining a running download takes no slot.
	joined := make(chan int, 1)
	go func() {
		jctx, jcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer jcancel()
		jw := httptest.NewRecorder()
		p.ServeHTTP(jw, httptest.NewRequest(http.MethodGet, blobPath(0), nil).WithContext(jctx))
		joined <- jw.Code
	}()
	if code := <-joined; code == http.StatusServiceUnavailable {
		t.Fatal("a joiner was refused a slot")
	}
	cancel()
	wg.Wait()
	waitFor(t, p.s, "the slots to free", func() bool { return p.s.downloads == 0 })
	if u.started.Load() != limit {
		t.Fatalf("%d downloads started", u.started.Load())
	}
}

// MAJOR 2: dockerd's legacy client sends one Accept line per type. All of them
// reach upstream, and they make one tag file.
func TestMultiLineAccept(t *testing.T) {
	types := []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}
	var mu sync.Mutex
	var seen []string
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, strings.Join(r.Header.Values("Accept"), ","))
		mu.Unlock()
		w.Header().Set("Content-Type", types[0])
		w.Header().Set("Docker-Content-Digest", sum(manifestBody))
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, manifestBody)
		}
	})
	p := proxyWith(t, srv, Config{})
	for range 2 {
		r := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/3", nil)
		for _, typ := range types {
			r.Header.Add("Accept", typ)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
	}
	mu.Lock()
	for _, got := range seen {
		for _, typ := range types {
			if !strings.Contains(got, typ) {
				t.Errorf("upstream Accept %q lacks %s", got, typ)
			}
		}
	}
	mu.Unlock()
	if n := accounted(t, p.s); n != 1 {
		t.Fatalf("%d tag files", n)
	}
	// The 8 KiB bound applies to the joined lines.
	r := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/3", nil)
	for range 200 {
		r.Header.Add("Accept", strings.Repeat("x", 50))
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("oversized Accept: %d", w.Code)
	}
}

// MINOR: a rate-limited upstream (429) is unavailable: a known tag is served
// stale; an unknown one is an error.
func TestRateLimitedServesStaleTag(t *testing.T) {
	f := fake(t)
	p := proxyFor(t, f)
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3"); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	f.status.Store(http.StatusTooManyRequests)
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3"); w.Code != http.StatusOK || w.Body.String() != manifestBody {
		t.Fatalf("stale on 429: %d", w.Code)
	}
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/other"); w.Code != http.StatusBadGateway {
		t.Fatalf("unknown on 429: %d", w.Code)
	}
}

// MINOR: eviction walks the LRU, evicts least recently used first, frees down
// to the low-water mark rather than the bare need, and skips pinned entries.
func TestEvictionLRUAndLowWater(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const max = 2000 // low water: 5% = 100 bytes
	s, err := openStore(t.TempDir(), max, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// 40 blobs of 50 bytes fill the store. Freeing only the need (1 byte) would
	// evict one blob; the low-water mark (100 bytes) evicts two.
	const n = 40
	blob := func(i int) string { return fmt.Sprintf("%03d", i) + strings.Repeat("x", 47) }
	for i := range n {
		now = now.Add(time.Second)
		put(t, s, blob(i))
	}
	// Touch 0 and 1, and keep 2 open: the least recently used are then 3, 4...
	for _, i := range []int{0, 1} {
		now = now.Add(time.Second)
		f, _, err := s.open("blobs", sum(blob(i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	pinned, _, err := s.open("blobs", sum(blob(2)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pinned.Close() }()
	s.mu.Lock()
	s.lru.MoveToFront(s.entries["blobs/"+sum(blob(2))[7:]].elem) // oldest of all, but open
	s.mu.Unlock()
	now = now.Add(time.Second)
	put(t, s, "a") // 1 byte over the cap: frees max(1, 100) bytes
	present := func(i int) bool {
		_, err := os.Stat(filepath.Join(s.dir, "blobs", sum(blob(i))[7:]))
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
	if free < min(lowWater, int64(max)/20)-1 {
		t.Fatalf("freed only to %d", free)
	}
}

// MINOR: the tag index follows rewrites, and removing a manifest takes exactly
// its own tags along.
func TestTagIndex(t *testing.T) {
	s, err := openStore(t.TempDir(), 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	manifests := []string{`{"schemaVersion":2,"a":1}`, `{"schemaVersion":2,"b":1}`}
	for _, m := range manifests {
		f, _, _, err := s.ensure(context.Background(), "manifests", sum(m), func(_ context.Context, w io.Writer) (string, error) {
			_, err := io.WriteString(w, m)
			return "application/vnd.oci.image.index.v1+json", err
		}, keepalive{})
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
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
	if !s.removeLocked("manifests/" + sum(manifests[0])[7:]) {
		t.Fatal("remove failed")
	}
	n0, n1 := len(s.tagsOf[sum(manifests[0])]), len(s.tagsOf[sum(manifests[1])])
	s.mu.Unlock()
	if s.tag("library/alpine", "x", "") != "" || s.tag("library/alpine", "y", "") != sum(manifests[1]) || n0 != 0 || n1 != 1 {
		t.Fatalf("index: %d %d", n0, n1)
	}
	accounted(t, s)
}

// NIT: the committed entry is pinned for its waiters, so an eviction between
// the commit and their open cannot remove it.
func TestCommittedEntryPinnedForWaiters(t *testing.T) {
	s, err := openStore(t.TempDir(), 8, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.hookCommitted = func(string) {
		// Ask for the whole store before the waiters are woken.
		s.mu.Lock()
		s.evictLocked(s.max)
		s.mu.Unlock()
	}
	f, _, _, err := s.ensure(context.Background(), "blobs", sum("aaaa"), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, "aaaa")
		return "", err
	}, keepalive{})
	if err != nil {
		t.Fatalf("waiter lost its entry: %v", err)
	}
	s.mu.Lock()
	pins := s.entries["blobs/"+sum("aaaa")[7:]].pins
	s.mu.Unlock()
	if pins != 1 {
		t.Fatalf("pins %d: the commit pin was not released to the reader's", pins)
	}
	_ = f.Close()
	s.hookCommitted = nil
	s.mu.Lock()
	s.evictLocked(s.max)
	_, kept := s.entries["blobs/"+sum("aaaa")[7:]]
	s.mu.Unlock()
	if kept {
		t.Fatal("unpinned entry not evictable")
	}
}

// NIT: a request that joins a manifest flight logs hit=false.
func TestJoinerLogsMiss(t *testing.T) {
	release := make(chan struct{})
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", sum(manifestBody))
		if r.Method == http.MethodGet {
			<-release
			_, _ = io.WriteString(w, manifestBody)
		}
	})
	var logs syncBuffer
	p := proxyWith(t, srv, Config{Log: zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &logs})})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3") })
	}
	key := "manifests/" + sum(manifestBody)[7:]
	waitFor(t, p.s, "two waiters", func() bool { return waiters(p.s, key) == 2 })
	close(release)
	wg.Wait()
	if out := logs.String(); strings.Count(out, `"hit":false`) != 2 || strings.Contains(out, `"hit":true`) {
		t.Fatalf("logs: %s", out)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// NIT: a .type file without its manifest is removed at startup.
func TestOrphanTypeRemoved(t *testing.T) {
	dir := t.TempDir()
	if _, err := openStore(dir, 1<<20, time.Now); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "manifests", sum("gone")[7:]+".type")
	if err := os.WriteFile(orphan, []byte("application/json"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(dir, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan .type kept")
	}
	accounted(t, s)
}
