// SPDX-License-Identifier: MIT

package registryproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
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
)

// TestMain fails the run if any store hit an accounting bug (unpinLocked of
// an unpinned entry), whichever test caused it.
func TestMain(m *testing.M) {
	var bugs atomic.Int32
	defaultBugHook = func(msg string) {
		bugs.Add(1)
		fmt.Fprintln(os.Stderr, "registry cache bug:", msg)
	}
	code := m.Run()
	if n := bugs.Load(); n > 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d registry cache accounting bugs\n", n)
		code = 1
	}
	os.Exit(code)
}

const manifestBody = `{"schemaVersion":2}`
const blobBody = "a layer of bytes"

func sum(b string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(b))) }

// storeCfg is a complete configuration for a test that opens a store directly.
func storeCfg(dir string, max int64) storeConfig {
	return storeConfig{dir: dir, max: max, now: time.Now, grace: time.Second, maxDownloads: 16, slotWait: time.Second}
}

func mustOpen(t *testing.T, c storeConfig) *store {
	t.Helper()
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// put stores b as a blob.
func put(t *testing.T, s *store, b string) {
	t.Helper()
	f, _, _, err := s.ensure(context.Background(), kindBlob, sum(b), func(_ context.Context, w io.Writer) (string, error) { _, err := io.WriteString(w, b); return "", err }, keepalive{})
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// putManifest stores m as a manifest of media type media.
func putManifest(t *testing.T, s *store, m, media string) {
	t.Helper()
	f, _, _, err := s.ensure(context.Background(), kindManifest, sum(m), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, m)
		return media, err
	}, keepalive{})
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// accounted checks that the store's total is exactly the sum of its entries and
// within Max, and returns the number of tag files on disk.
func accounted(t *testing.T, s *store) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	tags := 0
	for k, e := range s.entries {
		sum += e.size
		if e.size < minCharge {
			t.Errorf("%s charged %d, under the minimum", k, e.size)
		}
		if e.kind == kindTag {
			tags++
			if e.size != minCharge {
				t.Errorf("tag %s charged %d", k, e.size)
			}
		}
	}
	if sum != s.total || s.total+s.pending > s.max {
		t.Fatalf("accounting: sum=%d total=%d pending=%d max=%d", sum, s.total, s.pending, s.max)
	}
	files, err := os.ReadDir(filepath.Join(s.dir, kindTag.dir()))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != tags {
		t.Fatalf("%d tag files, %d tag entries", len(files), tags)
	}
	return tags
}

// waitFor polls cond (under the store lock) until it holds.
func waitFor(t *testing.T, s *store, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; {
		s.mu.Lock()
		ok := cond()
		s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waiters is the number of requests waiting for the flight of key (-1: none).
// The caller holds the store lock.
func waiters(s *store, key string) int {
	if fl, ok := s.flights[key]; ok {
		return fl.waiters
	}
	return -1
}

// upstreamWith fakes Docker Hub: /token answers an anonymous token and h serves
// everything else.
func upstreamWith(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"token":"public","expires_in":300}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func proxyWith(t *testing.T, srv *httptest.Server, c Config) *Proxy {
	t.Helper()
	if c.Dir == "" {
		c.Dir = t.TempDir()
	}
	if c.Max == 0 {
		c.Max = 8 << 20
	}
	c.Upstream, c.AuthURL = srv.URL, srv.URL+"/token"
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeRegistry is a Docker Hub with one manifest and one blob, served from a
// CDN behind a redirect. It counts the requests it gets.
type fakeRegistry struct {
	server                     *httptest.Server
	heads, gets, blobs, tokens atomic.Int32
	status                     atomic.Int32
	corrupt                    atomic.Bool
}

func fake(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer leaked to CDN")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(blobBody)))
		if r.Method != http.MethodHead {
			b := blobBody
			if f.corrupt.Load() {
				b = strings.Repeat("x", len(b))
			}
			_, _ = io.WriteString(w, b)
		}
	}))
	t.Cleanup(cdn.Close)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			f.tokens.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("guest credentials forwarded to auth")
			}
			if r.URL.Query().Get("scope") != "repository:library/alpine:pull" {
				t.Error("wrong scope")
			}
			_, _ = io.WriteString(w, `{"token":"public","expires_in":300}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer public" {
			t.Errorf("wrong bearer %q", r.Header.Get("Authorization"))
		}
		if s := f.status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", sum(manifestBody))
			w.Header().Set("Content-Length", fmt.Sprint(len(manifestBody)))
			if r.Method == http.MethodHead {
				f.heads.Add(1)
			} else {
				f.gets.Add(1)
				_, _ = io.WriteString(w, manifestBody)
			}
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			if r.Method == http.MethodGet {
				f.blobs.Add(1)
			}
			http.Redirect(w, r, cdn.URL+"/layer", http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func proxyFor(t *testing.T, f *fakeRegistry) *Proxy {
	t.Helper()
	p, err := New(Config{Dir: t.TempDir(), Max: 8 << 20, Upstream: f.server.URL, AuthURL: f.server.URL + "/token"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// request serves one request with guest credentials that must never reach
// upstream; headers are name, value pairs.
func request(t *testing.T, p http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer guest-secret")
	for i := 0; i < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

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

type keepAliveRecorder struct {
	*httptest.ResponseRecorder
	calls atomic.Int32
}

func (k *keepAliveRecorder) KeepAlive() { k.calls.Add(1) }

// waitKeptAlive waits until w has been kept alive at least once.
func waitKeptAlive(t *testing.T, w *keepAliveRecorder) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); w.calls.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("request never kept alive")
		}
	}
}

// blockingWriter holds a response in its first Write until release is closed.
type blockingWriter struct {
	*httptest.ResponseRecorder
	started, release chan struct{}
	once             sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.ResponseRecorder.Write(p)
}

// slowBlob streams body in chunks, one every gap.
func slowBlob(body string, chunks int, gap time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		size := len(body) / chunks
		for i := 0; i < len(body); i += size {
			_, _ = io.WriteString(w, body[i:min(i+size, len(body))])
			w.(http.Flusher).Flush()
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			}
		}
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
