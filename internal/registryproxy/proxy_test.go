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
	"testing"
)

// A pulled manifest and blob are served from the store the second time: a tag
// is revalidated by HEAD, a digest is not fetched again.
func TestPullCached(t *testing.T) {
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
}

// HEAD of a blob answers its length, cold or stored, with no body; a stored
// blob serves ranges.
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
	if _, err := os.Stat(filepath.Join(p.s.dir, kindBlob.key(sum(blobBody)[7:]))); !os.IsNotExist(err) {
		t.Fatal("corrupt blob stored")
	}
	if ds, _ := os.ReadDir(filepath.Join(p.s.dir, "tmp")); len(ds) != 0 {
		t.Fatal("partial temp retained")
	}
}

// Only GET and HEAD of valid names, tags and digests under /v2/ are served.
func TestRouteValidation(t *testing.T) {
	p := proxyFor(t, fake(t))
	for _, path := range []string{"/v2/../library/alpine/manifests/3", "/v2/Library/alpine/manifests/3", "/v2/library/alpine/manifests/.bad", "/v2/library/alpine/manifests/sha256:bad", "/v2/library/alpine/blobs/sha256:bad", "/v2/library/alpine/tags/list", "/v3/library/alpine/manifests/3"} {
		if w := request(t, p, http.MethodGet, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := request(t, p, http.MethodPut, "/v2/"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
	if w := request(t, p, http.MethodGet, "/v2/"); w.Code != http.StatusOK || w.Body.String() != "{}" {
		t.Fatalf("/v2/: %d %s", w.Code, w.Body)
	}
}

// A manifest whose bytes do not match its digest, or over the size limit, is
// a 502 and is not stored.
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
		if _, _, err := p.s.open(kindManifest, sum(manifestBody)); err == nil {
			t.Fatal("invalid manifest stored")
		}
	}
}
