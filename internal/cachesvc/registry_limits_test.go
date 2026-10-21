// SPDX-License-Identifier: MIT

package cachesvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/registryproxy"
)

// The registry requests of one channel in progress at once are capped: one
// over the cap gets 503 at once, the cache API of that channel still answers,
// and another channel has its own slots.
func TestRegistryPerChannelCap(t *testing.T) {
	const limit = 2
	var inside atomic.Int32
	release := make(chan struct{})
	svc, err := New(Config{Dir: t.TempDir(), RegistryPerChannel: limit, Registry: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inside.Add(1)
		<-release
		_, _ = io.WriteString(w, "ok")
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	a, b := channel(t, svc, "sb-a", "job-a"), channel(t, svc, "sb-b", "job-b")
	get := func(c *http.Client) int {
		resp, err := c.Get("http://cache" + RegistryPath + "library/alpine/blobs/x")
		if err != nil {
			t.Error(err)
			return 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	var wg sync.WaitGroup
	codes := make(chan int, limit)
	for range limit {
		wg.Go(func() { codes <- get(a) })
	}
	waitUntil(t, func() bool { return inside.Load() == limit })
	if code := get(a); code != http.StatusServiceUnavailable {
		t.Fatalf("over the cap: %d", code)
	}
	if code := lookupStatus(t, a, ""); code != http.StatusForbidden {
		t.Fatalf("cache API behind the registry cap: %d", code)
	}
	other := make(chan int, 1)
	go func() { other <- get(b) }()
	waitUntil(t, func() bool { return inside.Load() == limit+1 })
	unblock()
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("within the cap: %d", code)
		}
	}
	if code := <-other; code != http.StatusOK {
		t.Fatalf("other channel: %d", code)
	}
	if code := get(a); code != http.StatusOK {
		t.Fatalf("slots not released: %d", code)
	}
}

// stallingHub fakes Docker Hub for a real registryproxy: a manifest, and blobs
// whose download stalls until the test ends. It counts the blob downloads
// started.
func stallingHub(t *testing.T) (*httptest.Server, *atomic.Int32, string) {
	t.Helper()
	const manifest = `{"schemaVersion":2}`
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))
	var started atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"token":"public"}`)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = io.WriteString(w, manifest)
		default:
			started.Add(1)
			w.Header().Set("Content-Length", "1000000")
			_, _ = io.WriteString(w, "partial")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-done:
			}
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) }) // before srv.Close, which waits for the handlers
	return srv, &started, digest
}

// Each channel has its own quota of cold downloads: a channel that has
// started RegistryDownloadsPerChannel of them waits for its own to end, while
// another channel starts its download at once.
func TestRegistryQuotaPerChannel(t *testing.T) {
	hub, started, _ := stallingHub(t)
	proxy, err := registryproxy.New(registryproxy.Config{Dir: t.TempDir(), Max: 1 << 30, Upstream: hub.URL, AuthURL: hub.URL + "/token",
		SlotWait: time.Hour, AbandonGrace: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Dir: t.TempDir(), Registry: proxy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	pull := func(c *http.Client, i int) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://cache%slibrary/alpine/blobs/sha256:%x", RegistryPath, sha256.Sum256(fmt.Appendf(nil, "blob-%d", i))), nil)
		wg.Go(func() {
			if resp, err := c.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		})
	}
	a, b := channel(t, svc, "sb-a", "job-a"), channel(t, svc, "sb-b", "job-b")
	for i := range RegistryDownloadsPerChannel + 1 {
		pull(a, i)
	}
	waitUntil(t, func() bool { return started.Load() == RegistryDownloadsPerChannel })
	time.Sleep(50 * time.Millisecond) // the one over A's quota would have started by now
	if n := started.Load(); n != RegistryDownloadsPerChannel {
		t.Fatalf("channel A started %d downloads, its quota is %d", n, RegistryDownloadsPerChannel)
	}
	pull(b, 100)
	waitUntil(t, func() bool { return started.Load() == RegistryDownloadsPerChannel+1 })
}

// The registry lines of a channel carry its job.
func TestRegistryLogsJob(t *testing.T) {
	hub, _, digest := stallingHub(t)
	var logs bytes.Buffer
	var mu sync.Mutex
	proxy, err := registryproxy.New(registryproxy.Config{Dir: t.TempDir(), Max: 1 << 20, Upstream: hub.URL, AuthURL: hub.URL + "/token",
		Log: zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return logs.Write(p)
		})})})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Dir: t.TempDir(), Registry: proxy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	resp, err := channel(t, svc, "sb-a", "job-a").Get("http://cache" + RegistryPath + "library/alpine/manifests/" + digest)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), `"job_id":"job-a"`) || !strings.Contains(logs.String(), `"message":"registry manifest"`) {
		t.Fatalf("logs: %s", logs.String())
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
