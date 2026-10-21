// SPDX-License-Identifier: MIT
package cachesvc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/registryproxy"
)

const registryProgress = 150 * time.Millisecond

// registryRig serves a fake registry behind the service's real HTTP server, so
// that the connection deadlines apply.
func registryRig(t *testing.T, h http.HandlerFunc) *rig {
	t.Helper()
	return newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, ProgressTimeout: registryProgress, Registry: h})
}

// A registry download that keeps moving may last longer than the
// progress period set when the request started.
func TestRegistryProgressingTransferSucceeds(t *testing.T) {
	const chunks = 8
	r := registryRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(chunks*1024))
		for range chunks {
			if _, err := w.Write([]byte(strings.Repeat("x", 1024))); err != nil {
				return
			}
			_ = http.NewResponseController(w).Flush()
			time.Sleep(registryProgress / 3)
		}
	})
	start := time.Now()
	resp, err := http.Get(r.srv.URL + "/v2/library/alpine/blobs/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || len(b) != chunks*1024 {
		t.Fatalf("read %d bytes: %v", len(b), err)
	}
	if d := time.Since(start); d < 2*registryProgress {
		t.Fatalf("transfer took %v, not longer than the progress period", d)
	}
}

// The proxy keeps a client alive while it waits for a shared upstream
// download, before the first byte; the read deadline moves too, so the request
// context stays alive.
func TestRegistryKeepAliveWhileWaiting(t *testing.T) {
	r := registryRig(t, func(w http.ResponseWriter, req *http.Request) {
		k, ok := w.(interface{ KeepAlive() })
		if !ok {
			t.Error("no KeepAlive on the registry writer")
			return
		}
		for range 6 {
			time.Sleep(registryProgress / 3)
			k.KeepAlive()
			if req.Context().Err() != nil {
				t.Error("request context ended while kept alive")
				return
			}
		}
		_, _ = io.WriteString(w, "verified")
	})
	resp, err := http.Get(r.srv.URL + "/v2/library/alpine/blobs/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if b, err := io.ReadAll(resp.Body); err != nil || string(b) != "verified" {
		t.Fatalf("%q %v", b, err)
	}
}

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
		resp, err := c.Get("http://cache/v2/library/alpine/blobs/x")
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

// Each channel hands the registry its own download quota, the same one for
// all of its requests.
func TestRegistryQuotaPerChannel(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]map[*registryproxy.Quota]bool{}
	svc, err := New(Config{Dir: t.TempDir(), Registry: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := registryproxy.QuotaOf(r.Context())
		if q == nil {
			t.Error("no quota on the registry request")
		}
		mu.Lock()
		ch := r.Header.Get("X-Channel")
		if seen[ch] == nil {
			seen[ch] = map[*registryproxy.Quota]bool{}
		}
		seen[ch][q] = true
		mu.Unlock()
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	for _, ch := range []string{"a", "b"} {
		c := channel(t, svc, "sb-"+ch, "job-"+ch)
		for range 3 {
			req, _ := http.NewRequest(http.MethodGet, "http://cache/v2/library/alpine/blobs/x", nil)
			req.Header.Set("X-Channel", ch)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen["a"]) != 1 || len(seen["b"]) != 1 {
		t.Fatalf("quotas per channel: a=%d b=%d", len(seen["a"]), len(seen["b"]))
	}
	for qa := range seen["a"] {
		if seen["b"][qa] {
			t.Fatal("two channels share a quota")
		}
	}
}

// A client that stops reading is cut after one progress period.
func TestRegistryStalledReaderIsCut(t *testing.T) {
	result := make(chan error, 1)
	r := registryRig(t, func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := w.Write(chunk); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	if _, err := fmt.Fprint(conn, "GET /v2/library/alpine/blobs/x HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	// Read nothing more.
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a stalled reader was not cut")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler still blocked")
	}
}
