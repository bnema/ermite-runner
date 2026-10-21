// SPDX-License-Identifier: MIT

package registryproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Accept is reduced to the known manifest types, so its variants map to
// a bounded set of keys.
func TestNormalizeAccept(t *testing.T) {
	const index, v2 = "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.v2+json"
	all := normalizeAccept("")
	if strings.Count(all, ",") != len(manifestTypes)-1 {
		t.Fatalf("default key %q", all)
	}
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
}

// The Accept variants a job sends for one tag reach upstream with every known
// type they name, and make at most one tag file per subset of the known types.
// dockerd's legacy client sends one Accept line per type; the 8 KiB bound
// applies to the joined lines.
func TestAcceptTagFiles(t *testing.T) {
	types := []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}
	for _, tc := range []struct {
		name     string
		accepts  func(i int) []string // the Accept lines of request i
		requests int
		maxTags  int
		code     int
	}{
		{"multi-line", func(int) []string { return types }, 2, 1, http.StatusOK},
		{"variants", func(i int) []string {
			switch i % 3 {
			case 1:
				return []string{fmt.Sprintf("x/only-junk-%d", i)}
			case 2:
				return []string{fmt.Sprintf("%s;q=0.%d, x/junk-%d, %s", types[0], i%10, i, types[1])}
			}
			return []string{fmt.Sprintf("%s;q=0.%d, x/junk-%d", types[0], i%10, i)}
		}, 300, 3, http.StatusOK},
		{"oversized", func(int) []string {
			lines := make([]string, 200)
			for i := range lines {
				lines[i] = strings.Repeat("x", 50)
			}
			return lines
		}, 1, 0, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			srv := upstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, strings.Join(r.Header.Values("Accept"), ","))
				mu.Unlock()
				w.Header().Set("Content-Type", types[0])
				w.Header().Set("Docker-Content-Digest", sum(manifestBody))
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, manifestBody)
				}
			})
			p := proxyWith(t, srv, Config{})
			for i := range tc.requests {
				r := httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/3", nil)
				lines := tc.accepts(i)
				for _, line := range lines {
					r.Header.Add("Accept", line)
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if w.Code != tc.code {
					t.Fatalf("request %d: %d", i, w.Code)
				}
				if tc.code != http.StatusOK {
					continue
				}
				mu.Lock()
				got := seen[len(seen)-1]
				mu.Unlock()
				for _, typ := range types {
					if strings.Contains(strings.Join(lines, ","), typ) && !strings.Contains(got, typ) {
						t.Errorf("upstream Accept %q lacks %s", got, typ)
					}
				}
			}
			if n := accounted(t, p.s); n > tc.maxTags || (tc.maxTags > 0 && n == 0) {
				t.Fatalf("%d tag files for %d requests", n, tc.requests)
			}
		})
	}
}
