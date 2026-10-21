// SPDX-License-Identifier: MIT
package registryproxy

import (
	"context"
	"errors"
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
		if strings.HasPrefix(k, "tags/") {
			tags++
			if e.size != tagCharge {
				t.Errorf("tag %s charged %d", k, e.size)
			}
		}
	}
	if sum != s.total || s.total+s.pending > s.max {
		t.Fatalf("accounting: sum=%d total=%d pending=%d max=%d", sum, s.total, s.pending, s.max)
	}
	files, err := os.ReadDir(filepath.Join(s.dir, "tags"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != tags {
		t.Fatalf("%d tag files, %d tag entries", len(files), tags)
	}
	return tags
}

// Accept is reduced to the known manifest types, so its variants map to
// a bounded set of keys.
func TestNormalizeAccept(t *testing.T) {
	const index, v2 = "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.v2+json"
	all := normalizeAccept("")
	for _, tc := range []struct{ in, want string }{
		{index + ";q=0.9, " + strings.ToUpper(v2) + " , " + index, v2 + "," + index},
		{" " + index + " ;q=0.1", index},
		{"text/html, */*", all},
		{"", all},
	} {
		if got := normalizeAccept(tc.in); got != tc.want {
			t.Errorf("normalizeAccept(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if strings.Count(all, ",") != len(manifestTypes)-1 {
		t.Fatalf("default key %q", all)
	}
}

// A job varying Accept on one tag creates at most one tag file per
// subset of the known types, and every tag file is charged.
func TestTagFilesBoundedByAccept(t *testing.T) {
	f := fake(t)
	p := proxyFor(t, f)
	for i := range 300 {
		accept := fmt.Sprintf("application/vnd.oci.image.index.v1+json;q=0.%d, x/junk-%d", i%10, i)
		switch i % 3 {
		case 1:
			accept = fmt.Sprintf("x/only-junk-%d", i)
		case 2:
			accept += ", application/vnd.docker.distribution.manifest.list.v2+json"
		}
		if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3", "Accept", accept); w.Code != http.StatusOK {
			t.Fatalf("manifest: %d", w.Code)
		}
	}
	if n := accounted(t, p.s); n == 0 || n > 3 {
		t.Fatalf("%d tag files for 300 Accept variants", n)
	}
}

// Tag files share the LRU of the store, so they cannot grow the store
// past Max.
func TestTagsChargedAndEvicted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const media = "application/vnd.oci.image.index.v1+json"
	dir := t.TempDir()
	max := int64(len(manifestBody)+len(media)) + 3*tagCharge
	s, err := openStore(dir, max, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.ensure(context.Background(), "manifests", sum(manifestBody), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, manifestBody)
		return media, err
	}, keepalive{})
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
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
	// Reopening charges the tag files again.
	s, err = openStore(dir, max, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if n := accounted(t, s); n != 3 {
		t.Fatalf("reopened with %d tags", n)
	}
}

// An entry that is open for serving is not evicted (its bytes would
// stay allocated while uncharged); it can be evicted once closed.
func TestPinnedBlobSurvivesEviction(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir, 8, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "aaaa")
	f, _, err := s.open("blobs", sum("aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	fill := func() error {
		g, _, _, err := s.ensure(context.Background(), "blobs", sum("bbbbbbbb"), func(_ context.Context, w io.Writer) (string, error) {
			_, err := io.WriteString(w, "bbbbbbbb")
			return "", err
		}, keepalive{})
		if err == nil {
			_ = g.Close()
		}
		return err
	}
	if err := fill(); !errors.Is(err, errTooLarge) {
		t.Fatalf("fill over a pinned entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sum("aaaa")[7:])); err != nil {
		t.Fatalf("pinned blob removed: %v", err)
	}
	accounted(t, s)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sum("aaaa")[7:])); !os.IsNotExist(err) {
		t.Fatal("unpinned blob not evicted")
	}
	accounted(t, s)
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

// A blob that a response is still sending is pinned: a fill that needs
// its room fails, and succeeds once the response has closed the file.
func TestEvictionWhileServing(t *testing.T) {
	a, b := strings.Repeat("a", 1024), strings.Repeat("b", 1024)
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
	p := proxyWith(t, srv, Config{Max: 1500})
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
	if _, err := os.Stat(filepath.Join(p.s.dir, "blobs", sum(a)[7:])); err != nil {
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
	if _, err := os.Stat(filepath.Join(p.s.dir, "blobs", sum(a)[7:])); !os.IsNotExist(err) {
		t.Fatal("released blob not evicted")
	}
	accounted(t, p.s)
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

// A blob body has no total deadline: a transfer that keeps moving may
// last far longer than RequestTimeout and BlobIdleTimeout.
func TestBlobTransferOutlivesRequestTimeout(t *testing.T) {
	body := strings.Repeat("layer", 4000)
	srv := upstreamWith(t, slowBlob(body, 20, 25*time.Millisecond))
	p := proxyWith(t, srv, Config{RequestTimeout: 100 * time.Millisecond, BlobIdleTimeout: 150 * time.Millisecond})
	start := time.Now()
	w := request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(body))
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("blob: %d", w.Code)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("transfer took %v, not longer than the timeouts", d)
	}
}

// A blob body that stops moving is cut after BlobIdleTimeout, and auth,
// manifests and HEAD keep their total RequestTimeout.
func TestStalledUpstreamTimesOut(t *testing.T) {
	body := strings.Repeat("layer", 4000)
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = io.WriteString(w, body[:100])
			w.(http.Flusher).Flush()
		}
		select { // a stalled body, or headers that never come
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	p := proxyWith(t, srv, Config{RequestTimeout: 100 * time.Millisecond, BlobIdleTimeout: 150 * time.Millisecond})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v2/library/alpine/blobs/" + sum(body)},
		{http.MethodHead, "/v2/library/alpine/blobs/" + sum(body)},
		{http.MethodGet, "/v2/library/alpine/manifests/3"},
	} {
		start := time.Now()
		if w := request(t, p, tc.method, tc.path); w.Code != http.StatusBadGateway {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s %s: cut after %v", tc.method, tc.path, d)
		}
	}
	if _, err := os.Stat(filepath.Join(p.s.dir, "blobs", sum(body)[7:])); !os.IsNotExist(err) {
		t.Fatal("partial blob stored")
	}
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
func waiters(s *store, key string) int {
	if fl, ok := s.flights[key]; ok {
		return fl.waiters
	}
	return -1
}

// The shared download does not belong to the client that started it.
// That client disconnects mid-download; a second client, joining within the
// grace period, still gets the blob and the blob is committed, from one
// upstream fetch.
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
	key := "blobs/" + sum(body)[7:]
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
	if _, err := os.Stat(filepath.Join(p.s.dir, "blobs", sum(body)[7:])); err != nil {
		t.Fatalf("blob not committed: %v", err)
	}
	if gets.Load() != 1 {
		t.Fatalf("%d upstream fetches", gets.Load())
	}
}

type keepAliveRecorder struct {
	*httptest.ResponseRecorder
	calls atomic.Int32
}

func (k *keepAliveRecorder) KeepAlive() { k.calls.Add(1) }

// A client waiting for a progressing download is kept alive, so
// the cache channel's progress deadline follows the download.
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

// Auth and manifest requests never follow a redirect.
func TestMetadataRedirectsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = io.WriteString(w, `{"token":"public"}`)
	}))
	t.Cleanup(other.Close)
	for _, redirectAuth := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" && !redirectAuth {
				_, _ = io.WriteString(w, `{"token":"public"}`)
				return
			}
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		p := proxyWith(t, srv, Config{})
		for _, ref := range []string{"3", sum(manifestBody)} {
			if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/"+ref); w.Code != http.StatusBadGateway {
				t.Errorf("auth redirect %v, ref %s: %d", redirectAuth, ref, w.Code)
			}
		}
		srv.Close()
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("%d redirects followed", elsewhere.Load())
	}
}

// Blob requests follow the CDN redirect, without the bearer token.
func TestBlobRedirectStripsBearer(t *testing.T) {
	var cdnHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer sent to the CDN")
		}
		_, _ = io.WriteString(w, blobBody)
	}))
	t.Cleanup(cdn.Close)
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer public" {
			t.Error("no bearer on the registry")
		}
		http.Redirect(w, r, cdn.URL+"/layer", http.StatusTemporaryRedirect)
	})
	p := proxyWith(t, srv, Config{})
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(blobBody)); w.Code != http.StatusOK || w.Body.String() != blobBody {
		t.Fatalf("blob: %d", w.Code)
	}
	if cdnHits.Load() != 1 {
		t.Fatalf("CDN hits %d", cdnHits.Load())
	}
}
