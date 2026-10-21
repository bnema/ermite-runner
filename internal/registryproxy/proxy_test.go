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
)

const manifestBody = `{"schemaVersion":2}`
const blobBody = "a layer of bytes"

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
func TestPullAndOfflineTag(t *testing.T) {
	f := fake(t)
	p := proxyFor(t, f)
	for range 2 {
		w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3")
		if w.Code != http.StatusOK || w.Body.String() != manifestBody {
			t.Fatalf("manifest: %d %s", w.Code, w.Body)
		}
		w = request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(blobBody))
		if w.Code != http.StatusOK || w.Body.String() != blobBody {
			t.Fatalf("blob: %d %s", w.Code, w.Body)
		}
	}
	if f.heads.Load() != 2 || f.gets.Load() != 1 || f.blobs.Load() != 1 || f.tokens.Load() != 1 {
		t.Fatalf("counts HEAD=%d GET=%d blobs=%d tokens=%d", f.heads.Load(), f.gets.Load(), f.blobs.Load(), f.tokens.Load())
	}
	for _, down := range []int32{http.StatusServiceUnavailable, 0} {
		f.status.Store(down)
		if down == 0 {
			f.server.Close()
		}
		if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3"); w.Code != http.StatusOK || w.Body.String() != manifestBody {
			t.Fatalf("stale: %d %s", w.Code, w.Body)
		}
		if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/unknown"); w.Code != http.StatusBadGateway {
			t.Fatalf("unknown: %d", w.Code)
		}
	}
}
func TestBlobHeadAndRange(t *testing.T) {
	f := fake(t)
	p := proxyFor(t, f)
	path := "/v2/library/alpine/blobs/" + sum(blobBody)
	for _, cached := range []bool{false, true} {
		if cached {
			if w := request(t, p, http.MethodGet, path); w.Code != http.StatusOK {
				t.Fatal(w.Code)
			}
		}
		w := request(t, p, http.MethodHead, path)
		if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != fmt.Sprint(len(blobBody)) {
			t.Fatalf("HEAD %d %v %q", w.Code, w.Header(), w.Body.String())
		}
	}
	w := request(t, p, http.MethodGet, path, "Range", "bytes=2-5")
	if w.Code != http.StatusPartialContent || w.Body.String() != blobBody[2:6] {
		t.Fatalf("range: %d %s", w.Code, w.Body)
	}
}

// A blob is verified before any byte of it is sent: a corrupt one is a 502
// with no body, and is not stored.
func TestCorruptBlobRejected(t *testing.T) {
	f := fake(t)
	f.corrupt.Store(true)
	p := proxyFor(t, f)
	w := request(t, p, http.MethodGet, "/v2/library/alpine/blobs/"+sum(blobBody))
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "xxxx") {
		t.Fatalf("corrupt blob: %d %q", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(p.s.dir, "blobs", sum(blobBody)[7:])); !os.IsNotExist(err) {
		t.Fatal("corrupt blob stored")
	}
	ds, _ := os.ReadDir(filepath.Join(p.s.dir, "tmp"))
	if len(ds) != 0 {
		t.Fatal("partial temp retained")
	}
}
func TestDeniedAndValidation(t *testing.T) {
	f := fake(t)
	p := proxyFor(t, f)
	f.status.Store(http.StatusUnauthorized)
	if w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/3"); w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
	if f.tokens.Load() != 2 {
		t.Fatal("no fresh token retry")
	}
	for _, path := range []string{"/v2/../library/alpine/manifests/3", "/v2/Library/alpine/manifests/3", "/v2/library/alpine/manifests/.bad", "/v2/library/alpine/manifests/sha256:bad", "/v2/library/alpine/blobs/sha256:bad", "/v2/library/alpine/tags/list"} {
		if w := request(t, p, http.MethodGet, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := request(t, p, http.MethodPut, "/v2/"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}
func TestManifestLimitsAndDigest(t *testing.T) {
	for _, body := range []string{"tampered", strings.Repeat("x", manifestLimit+1)} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = io.WriteString(w, `{"token":"public"}`)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
		p, err := New(Config{Dir: t.TempDir(), Max: 8 << 20, Upstream: up.URL, AuthURL: up.URL + "/token"})
		if err != nil {
			t.Fatal(err)
		}
		w := request(t, p, http.MethodGet, "/v2/library/alpine/manifests/"+sum(manifestBody))
		up.Close()
		if w.Code != http.StatusBadGateway {
			t.Fatal(w.Code)
		}
		if _, _, err := p.s.open("manifests", sum(manifestBody)); err == nil {
			t.Fatal("invalid manifest stored")
		}
	}
}
