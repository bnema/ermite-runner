// SPDX-License-Identifier: MIT
package host

import (
	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-runner/internal/cachesvc"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeForRegistryOnly(t *testing.T) {
	svc, err := cachesvc.New(cachesvc.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.CloseAll() }()
	j := &jobRun{job: &ermite.Job{JobId: "j1", TimeoutMinutes: 5}}
	port, tok, mode, results := j.runtimeFor(&Runner{Cache: svc, Registry: true})
	if port != cachesvc.DefaultPort || tok == "" || mode != "" || results {
		t.Fatalf("%d %q %q %v", port, tok, mode, results)
	}
	job, scopes, _, err := svc.RuntimeToken(tok)
	if err != nil || job != "j1" || strings.Contains(scopes, "Actions.Results") {
		t.Fatalf("%q %q %v", job, scopes, err)
	}
	req := httptest.NewRequest(http.MethodPost, cachesvc.APIPath+"caches", strings.NewReader(`{"key":"k","version":"v","cacheSize":1}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	svc.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unscoped token allowed cache: %d", w.Code)
	}
	if p, _, _, _ := j.runtimeFor(&Runner{Cache: svc}); p != 0 {
		t.Fatal("proxy off still supplies channel")
	}
	if p, _, _, _ := j.runtimeFor(&Runner{Registry: true}); p != 0 {
		t.Fatal("cache off still supplies channel")
	}
}
