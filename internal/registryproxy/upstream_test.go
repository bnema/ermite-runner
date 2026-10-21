// SPDX-License-Identifier: MIT

package registryproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A blob body has no total deadline: a transfer that keeps moving may last
// far longer than RequestTimeout and BlobIdleTimeout.
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
	if _, err := os.Stat(filepath.Join(p.s.dir, kindBlob.key(sum(body)[7:]))); !os.IsNotExist(err) {
		t.Fatal("partial blob stored")
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

// Blob requests, HEAD included, follow the CDN redirect without the bearer
// token.
func TestBlobRedirectStripsBearer(t *testing.T) {
	var cdnHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer sent to the CDN")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(blobBody)))
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, blobBody)
		}
	}))
	t.Cleanup(cdn.Close)
	srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer public" {
			t.Error("no bearer on the registry")
		}
		http.Redirect(w, r, cdn.URL+"/layer", http.StatusTemporaryRedirect)
	})
	p := proxyWith(t, srv, Config{})
	path := "/v2/library/alpine/blobs/" + sum(blobBody)
	if w := request(t, p, http.MethodHead, path); w.Code != http.StatusOK || w.Header().Get("Content-Length") != fmt.Sprint(len(blobBody)) {
		t.Fatalf("HEAD: %d %v", w.Code, w.Header())
	}
	if w := request(t, p, http.MethodGet, path); w.Code != http.StatusOK || w.Body.String() != blobBody {
		t.Fatalf("blob: %d", w.Code)
	}
	if cdnHits.Load() != 2 {
		t.Fatalf("CDN hits %d", cdnHits.Load())
	}
}

// A 401 or 403 is retried once with a fresh token; a second one is a 404, so
// that dockerd falls back to Docker Hub with the job's credentials.
func TestUnauthorizedRetriedOnce(t *testing.T) {
	for _, path := range []string{"/v2/library/alpine/manifests/3", "/v2/library/alpine/blobs/" + sum(blobBody)} {
		f := fake(t)
		p := proxyFor(t, f)
		f.status.Store(http.StatusUnauthorized)
		if w := request(t, p, http.MethodGet, path); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if f.tokens.Load() != 2 {
			t.Fatalf("%s: %d token requests, want a fresh one", path, f.tokens.Load())
		}
	}
}

// When upstream is down, fails or rate-limits (429), a known tag is served
// stale; an unknown one is an error.
func TestStaleTagWhenUpstreamUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int32 // 0: the server is closed
	}{{"rate limited", http.StatusTooManyRequests}, {"failing", http.StatusServiceUnavailable}, {"down", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			f := fake(t)
			p := proxyFor(t, f)
			if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3"); w.Code != http.StatusOK {
				t.Fatal(w.Code)
			}
			if tc.status == 0 {
				f.server.Close()
			}
			f.status.Store(tc.status)
			if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3"); w.Code != http.StatusOK || w.Body.String() != manifestBody {
				t.Fatalf("stale: %d %s", w.Code, w.Body)
			}
			if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/unknown"); w.Code != http.StatusBadGateway {
				t.Fatalf("unknown: %d", w.Code)
			}
		})
	}
}
