// SPDX-License-Identifier: MIT

package cachesvc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The registry is served under RegistryPath on the cache handler and the
// channel, never by the results service; without one, those paths are 404.
func TestRegistryRoute(t *testing.T) {
	for _, on := range []bool{false, true} {
		cfg := Config{}
		if on {
			cfg.Registry = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "registry") })
		}
		r := newRigWith(t, cfg)
		results := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("registry fell through to results")
			w.WriteHeader(http.StatusInternalServerError)
		})
		for _, h := range []http.Handler{r.svc.Handler(), r.svc.channelHandler("job", results)} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, RegistryPath, nil))
			if on {
				if w.Code != http.StatusOK || w.Body.String() != "registry" {
					t.Fatalf("%d %s", w.Code, w.Body)
				}
			} else if w.Code != http.StatusNotFound {
				t.Fatal(w.Code)
			}
		}
	}
}
