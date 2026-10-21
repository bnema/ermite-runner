// SPDX-License-Identifier: MIT

package registryproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// At most MaxDownloads cold blobs download at once; one more waits for
// SlotWait and then gets 503 (dockerd retries or pulls from Docker Hub), a
// joiner of a running download takes no slot, and a slot frees once a
// download ends.
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
	// Joining a running download takes no slot: the joiner is a waiter of the
	// flight, and the number of downloads does not move.
	key := kindBlob.key(sum("blob-0")[7:])
	go p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, blobPath(0), nil).WithContext(ctx))
	waitFor(t, p.s, "the joiner", func() bool { return waiters(p.s, key) == 2 })
	p.s.mu.Lock()
	downloads := p.s.downloads
	p.s.mu.Unlock()
	if downloads != limit {
		t.Fatalf("%d downloads with a joiner, want %d", downloads, limit)
	}
	cancel()
	wg.Wait()
	waitFor(t, p.s, "the slots to free", func() bool { return p.s.downloads == 0 })
	if u.started.Load() != limit {
		t.Fatalf("%d downloads started", u.started.Load())
	}
}

// A sandbox that saturates its quota does not hold back another sandbox: its
// download starts at once while the global cap still has room.
func TestQuotaIsolatesChannels(t *testing.T) {
	u := newStallingUpstream(t)
	p := proxyWith(t, u.srv, Config{SlotWait: time.Hour, AbandonGrace: 10 * time.Millisecond})
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
	// The quotas are taken before the downloads reach upstream: wait for the
	// 4 of A and B there, then check under the store lock that A's fourth
	// took no slot.
	waitFor(t, p.s, "4 downloads upstream", func() bool { return u.started.Load() == 4 })
	p.s.mu.Lock()
	downloads := p.s.downloads
	p.s.mu.Unlock()
	if downloads != 4 {
		t.Fatalf("%d downloads, want A's 3 and B's 1", downloads)
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
	waitKeptAlive(t, w) // waiting for a slot
	select {
	case code := <-second:
		t.Fatalf("did not wait for a slot: %d", code)
	default:
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
	if u.started.Load() != 1 {
		t.Fatalf("%d downloads started", u.started.Load())
	}
}

// A request that disconnects while it waits for a slot leaves at once, with no
// slot and no 503.
func TestQuotaWaiterDisconnects(t *testing.T) {
	u := newStallingUpstream(t)
	const wait = time.Hour
	p := proxyWith(t, u.srv, Config{SlotWait: wait, AbandonGrace: 10 * time.Millisecond, KeepAliveInterval: 5 * time.Millisecond})
	q := NewQuota(1)
	ctx, cancel := context.WithCancel(WithQuota(context.Background(), q))
	defer cancel()
	serveAsync(p, ctx, blobPath(0), nil)
	waitFor(t, p.s, "the quota to fill", func() bool { return q.used.Load() == 1 })
	wctx, wcancel := context.WithCancel(WithQuota(context.Background(), q))
	w := &keepAliveRecorder{ResponseRecorder: httptest.NewRecorder()}
	waiting := serveAsync(p, wctx, blobPath(1), w)
	waitKeptAlive(t, w) // really waiting for a slot
	start := time.Now()
	wcancel()
	code := <-waiting
	if d := time.Since(start); d > time.Second {
		t.Fatalf("left after %v", d)
	}
	if code == http.StatusServiceUnavailable {
		t.Fatal("a disconnected waiter got 503")
	}
	if u.started.Load() != 1 || q.used.Load() != 1 {
		t.Fatalf("%d downloads started, quota %d", u.started.Load(), q.used.Load())
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
	key := kindBlob.key(sum("blob-0")[7:])
	waitFor(t, p.s, "B to join", func() bool { return waiters(p.s, key) == 2 })
	if b.used.Load() != 0 {
		t.Fatal("a joiner took quota")
	}
	// B's quota is still free for a download of its own.
	serveAsync(p, ctxB, blobPath(1), nil)
	waitFor(t, p.s, "B's own download", func() bool { return b.used.Load() == 1 })
}
