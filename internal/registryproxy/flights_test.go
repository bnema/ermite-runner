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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
)

// Concurrent requests for one digest share a single fetch.
func TestSingleFlight(t *testing.T) {
	s := mustOpen(t, storeCfg(t.TempDir(), 1<<20))
	var calls atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			f, _, _, err := s.ensure(context.Background(), kindBlob, sum("content"), func(_ context.Context, w io.Writer) (string, error) {
				calls.Add(1)
				_, err := io.WriteString(w, "content")
				return "", err
			}, keepalive{})
			if err != nil {
				t.Error(err)
				return
			}
			_ = f.Close()
		})
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetches %d", calls.Load())
	}
}

// A guest that requests many digests and disconnects leaves no download
// behind: once a flight has no waiter for the grace period, it is cancelled
// upstream and its temporary file removed.
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

// A request that joins within the grace period keeps the download.
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
	key := kindBlob.key(sum(body)[7:])
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

// The shared download does not belong to the client that started it. That
// client disconnects mid-download; a second client, joining within the grace
// period, still gets the blob and the blob is committed, from one upstream
// fetch.
func TestBlobFlightSurvivesInitiatorDisconnect(t *testing.T) {
	body := strings.Repeat("layer", 4000)
	half, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var gets atomic.Int32
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = io.WriteString(w, body[:len(body)/2])
		w.(http.Flusher).Flush()
		close(half)
		<-release
		_, _ = io.WriteString(w, body[len(body)/2:])
	})
	t.Cleanup(unblock) // before srv.Close, which waits for the handler
	p := proxyWith(t, srv, Config{})
	path := "/v2/library/alpine/blobs/" + sum(body)

	ctx, cancel := context.WithCancel(context.Background())
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(first, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	<-half
	cancel()
	<-done
	if strings.Contains(first.Body.String(), "layer") {
		t.Fatal("unverified bytes sent to the initiator")
	}
	key := kindBlob.key(sum(body)[7:])
	p.s.mu.Lock()
	left := waiters(p.s, key)
	p.s.mu.Unlock()
	if left != 0 {
		t.Fatalf("waiters after the disconnect: %d", left)
	}

	second := httptest.NewRecorder()
	done = make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(second, httptest.NewRequest(http.MethodGet, path, nil))
	}()
	waitFor(t, p.s, "the second client to join", func() bool { return waiters(p.s, key) == 1 })
	unblock()
	<-done
	if second.Code != http.StatusOK || second.Body.String() != body {
		t.Fatalf("second client: %d", second.Code)
	}
	if _, err := os.Stat(filepath.Join(p.s.dir, key)); err != nil {
		t.Fatalf("blob not committed: %v", err)
	}
	if gets.Load() != 1 {
		t.Fatalf("%d upstream fetches", gets.Load())
	}
}

// A client waiting for a progressing download is kept alive, so the cache
// channel's progress deadline follows the download.
func TestWaiterKeepAlive(t *testing.T) {
	body := strings.Repeat("layer", 4000)
	srv := upstreamWith(t, slowBlob(body, 10, 30*time.Millisecond))
	p := proxyWith(t, srv, Config{KeepAliveInterval: 10 * time.Millisecond})
	w := &keepAliveRecorder{ResponseRecorder: httptest.NewRecorder()}
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/blobs/"+sum(body), nil))
	if w.Code != http.StatusOK || w.calls.Load() == 0 {
		t.Fatalf("code %d, %d keepalives", w.Code, w.calls.Load())
	}
}

// A panic in a download fails its flight and frees its slots; it does not
// leave the waiters hanging.
func TestPanickingFetchFailsFlight(t *testing.T) {
	c := storeCfg(t.TempDir(), 1<<20)
	c.maxDownloads = 1
	s := mustOpen(t, c)
	q := NewQuota(1)
	ctx, cancel := context.WithTimeout(WithQuota(context.Background(), q), 5*time.Second)
	defer cancel()
	_, _, _, err := s.ensure(ctx, kindBlob, sum("x"), func(context.Context, io.Writer) (string, error) {
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

// A request that joins a manifest flight logs hit=false, with its job.
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
	for i := range 2 {
		wg.Go(func() {
			ctx := WithJob(context.Background(), fmt.Sprint("job-", i))
			p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/3", nil).WithContext(ctx))
		})
	}
	key := kindManifest.key(sum(manifestBody)[7:])
	waitFor(t, p.s, "two waiters", func() bool { return waiters(p.s, key) == 2 })
	close(release)
	wg.Wait()
	out := logs.String()
	if strings.Count(out, `"hit":false`) != 2 || strings.Contains(out, `"hit":true`) {
		t.Fatalf("logs: %s", out)
	}
	if !strings.Contains(out, `"job_id":"job-0"`) || !strings.Contains(out, `"job_id":"job-1"`) {
		t.Fatalf("no job_id on the registry lines: %s", out)
	}
}
