// SPDX-License-Identifier: MIT
package registryproxy

import (
	"context"
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
)

// serveAsync serves path with ctx on its own goroutine, into w (a new
// recorder when nil); the status code comes on the returned channel.
func serveAsync(p *Proxy, ctx context.Context, path string, w *keepAliveRecorder) <-chan int {
	if w == nil {
		w = &keepAliveRecorder{ResponseRecorder: httptest.NewRecorder()}
	}
	code := make(chan int, 1)
	go func() {
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		code <- w.Code
	}()
	return code
}

// A sandbox that saturates its quota does not hold back another sandbox: its
// download starts at once while the global cap still has room.
func TestQuotaIsolatesChannels(t *testing.T) {
	u := newStallingUpstream(t)
	p := proxyWith(t, u.srv, Config{MaxDownloads: 16, SlotWait: time.Hour, AbandonGrace: 10 * time.Millisecond})
	a, b := NewQuota(3), NewQuota(3)
	ctxA, cancelA := context.WithCancel(WithQuota(context.Background(), a))
	defer cancelA()
	for i := range 3 {
		serveAsync(p, ctxA, blobPath(i), nil)
	}
	waitFor(t, p.s, "A's quota to fill", func() bool { return a.used.Load() == 3 })
	// A's fourth waits for its own quota and starts nothing.
	serveAsync(p, ctxA, blobPath(3), nil)
	ctxB, cancelB := context.WithCancel(WithQuota(context.Background(), b))
	defer cancelB()
	start := time.Now()
	serveAsync(p, ctxB, blobPath(10), nil)
	waitFor(t, p.s, "B's download to start", func() bool { return b.used.Load() == 1 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("B started after %v", d)
	}
	if got := u.started.Load(); got != 4 {
		t.Fatalf("%d downloads started, want A's 3 and B's 1", got)
	}
	cancelA()
	cancelB()
	waitFor(t, p.s, "every slot to free", func() bool { return p.s.downloads == 0 })
	if a.used.Load() != 0 || b.used.Load() != 0 {
		t.Fatalf("quota not released: %d %d", a.used.Load(), b.used.Load())
	}
}

// A request over its quota waits, kept alive, and starts its download as soon
// as one of its channel's ends, with no 503.
func TestQuotaWaiterGetsFreedSlot(t *testing.T) {
	body := strings.Repeat("layer", 100)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, sum(body)) {
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p := proxyWith(t, srv, Config{SlotWait: time.Hour, KeepAliveInterval: 5 * time.Millisecond})
	q := NewQuota(1)
	ctx := WithQuota(context.Background(), q)
	first := serveAsync(p, ctx, blobPath(0), nil)
	waitFor(t, p.s, "the quota to fill", func() bool { return q.used.Load() == 1 })
	w := &keepAliveRecorder{ResponseRecorder: httptest.NewRecorder()}
	second := serveAsync(p, ctx, "/v2/library/alpine/blobs/"+sum(body), w)
	for end := time.Now().Add(5 * time.Second); w.calls.Load() == 0; { // waiting for a slot, kept alive
		select {
		case code := <-second:
			t.Fatalf("did not wait for a slot: %d", code)
		default:
		}
		if time.Now().After(end) {
			t.Fatal("not kept alive while waiting for a slot")
		}
		time.Sleep(time.Millisecond)
	}
	unblock() // the first download ends (digest mismatch), freeing the slot
	if code := <-first; code != http.StatusBadGateway {
		t.Fatalf("first: %d", code)
	}
	if code := <-second; code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("second: %d", code)
	}
}

// After SlotWait without a slot, the request gets 503.
func TestQuotaWaitTimesOut(t *testing.T) {
	u := newStallingUpstream(t)
	const wait = 50 * time.Millisecond
	p := proxyWith(t, u.srv, Config{SlotWait: wait, AbandonGrace: 10 * time.Millisecond})
	q := NewQuota(1)
	ctx, cancel := context.WithCancel(WithQuota(context.Background(), q))
	defer cancel()
	serveAsync(p, ctx, blobPath(0), nil)
	waitFor(t, p.s, "the quota to fill", func() bool { return q.used.Load() == 1 })
	start := time.Now()
	code := <-serveAsync(p, ctx, blobPath(1), nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("over the quota: %d", code)
	}
	if d := time.Since(start); d < wait {
		t.Fatalf("503 after %v", d)
	}
	// A request that disconnects while waiting leaves at once, with no slot.
	cctx, ccancel := context.WithCancel(WithQuota(context.Background(), q))
	waiting := serveAsync(p, cctx, blobPath(2), nil)
	ccancel()
	<-waiting
	if u.started.Load() != 1 {
		t.Fatalf("%d downloads started", u.started.Load())
	}
}

// Joining a download in progress takes none of the joiner's quota.
func TestQuotaJoinersExempt(t *testing.T) {
	u := newStallingUpstream(t)
	p := proxyWith(t, u.srv, Config{SlotWait: 20 * time.Millisecond, AbandonGrace: 10 * time.Millisecond})
	a, b := NewQuota(1), NewQuota(1)
	ctxA, cancelA := context.WithCancel(WithQuota(context.Background(), a))
	defer cancelA()
	serveAsync(p, ctxA, blobPath(0), nil)
	waitFor(t, p.s, "A's download", func() bool { return a.used.Load() == 1 })
	ctxB, cancelB := context.WithCancel(WithQuota(context.Background(), b))
	defer cancelB()
	serveAsync(p, ctxB, blobPath(0), nil) // joins A's
	key := "blobs/" + sum("blob-0")[7:]
	waitFor(t, p.s, "B to join", func() bool { return waiters(p.s, key) == 2 })
	if b.used.Load() != 0 {
		t.Fatal("a joiner took quota")
	}
	// B's quota is still free for a download of its own.
	serveAsync(p, ctxB, blobPath(1), nil)
	waitFor(t, p.s, "B's own download", func() bool { return b.used.Load() == 1 })
}

// A panic in a download fails its flight and frees its slots; it does not
// leave the waiters hanging.
func TestPanickingFetchFailsFlight(t *testing.T) {
	s, err := openStore(t.TempDir(), 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.maxDownloads = 1
	q := NewQuota(1)
	ctx, cancel := context.WithTimeout(WithQuota(context.Background(), q), 5*time.Second)
	defer cancel()
	_, _, _, err = s.ensure(ctx, "blobs", sum("x"), func(context.Context, io.Writer) (string, error) {
		panic("boom")
	}, keepalive{})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downloads != 0 || len(s.flights) != 0 || q.used.Load() != 0 || s.pending != 0 {
		t.Fatalf("leaked: downloads=%d flights=%d quota=%d pending=%d", s.downloads, len(s.flights), q.used.Load(), s.pending)
	}
	if ds, _ := os.ReadDir(filepath.Join(s.dir, "tmp")); len(ds) != 0 {
		t.Fatal("temp file kept")
	}
}

// Eviction restarts from the front after a manifest removal that fails, and
// does not loop on it.
func TestEvictionAfterFailedManifestRemoval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const media = "application/vnd.oci.image.index.v1+json"
	s, err := openStore(t.TempDir(), 1<<20, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	m := `{"schemaVersion":2}`
	f, _, _, err := s.ensure(context.Background(), "manifests", sum(m), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, m)
		return media, err
	}, keepalive{})
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	now = now.Add(time.Second)
	if err := s.setTag("library/alpine", "3", "", sum(m)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	put(t, s, "blob")
	// The manifest's .type cannot be removed: replace it with a non-empty
	// directory. Its tag goes first all the same.
	typ := filepath.Join(s.dir, "manifests", sum(m)[7:]+".type")
	if err := os.Remove(typ); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(typ, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.evictLocked(s.max) // ask for everything
	_, manifestKept := s.entries["manifests/"+sum(m)[7:]]
	_, blobKept := s.entries["blobs/"+sum("blob")[7:]]
	tags := len(s.tagsOf)
	s.mu.Unlock()
	if !manifestKept || blobKept || tags != 0 {
		t.Fatalf("manifest kept=%v blob kept=%v tags=%d", manifestKept, blobKept, tags)
	}
	accounted(t, s)
}

// unpinLocked of an unpinned entry is reported (and fails the test run through
// TestMain's hook); here the hook is replaced to observe it.
func TestUnpinBugReported(t *testing.T) {
	s, err := openStore(t.TempDir(), 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var bugs atomic.Int32
	s.hookBug = func(string) { bugs.Add(1) }
	put(t, s, "aaaa")
	s.mu.Lock()
	s.unpinLocked("blobs/" + sum("aaaa")[7:])
	pins := s.entries["blobs/"+sum("aaaa")[7:]].pins
	s.mu.Unlock()
	if bugs.Load() != 1 || pins != 0 {
		t.Fatalf("bugs %d pins %d", bugs.Load(), pins)
	}
}
