// SPDX-License-Identifier: MIT

package host

import (
	"testing"

	"bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/registryproxy"
)

// A job with neither a cache scope nor the results service gets the channel
// for the registry proxy alone: its port and no token. Without the proxy, or
// without the cache, it gets no channel.
func TestRuntimeForRegistryOnly(t *testing.T) {
	proxy, err := registryproxy.New(registryproxy.Config{Dir: t.TempDir(), Max: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	withRegistry, err := cachesvc.New(cachesvc.Config{Dir: t.TempDir(), Registry: proxy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = withRegistry.CloseAll() }()
	without, err := cachesvc.New(cachesvc.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = without.CloseAll() }()
	// No repository in the contexts: the job has no cache scope.
	j := &jobRun{job: &ermite.Job{JobId: "j1", TimeoutMinutes: 5}}
	port, tok, mode, results := j.runtimeFor(&Runner{Cache: withRegistry})
	if port != cachesvc.DefaultPort || tok != "" || mode != "" || results {
		t.Fatalf("registry only: %d %q %q %v", port, tok, mode, results)
	}
	if p, _, _, _ := j.runtimeFor(&Runner{Cache: without}); p != 0 {
		t.Fatal("proxy off still supplies a channel")
	}
	if p, _, _, _ := j.runtimeFor(&Runner{}); p != 0 {
		t.Fatal("cache off still supplies a channel")
	}
	// A job with a cache scope still gets its token, proxy or not.
	scoped := &jobRun{job: &ermite.Job{JobId: "j1", Ref: "refs/heads/main", TimeoutMinutes: 5}, contexts: `{"github":{"repository":"o/r"}}`}
	if _, tok, mode, _ := scoped.runtimeFor(&Runner{Cache: withRegistry}); tok == "" || mode != "write" {
		t.Fatalf("scoped job: %q %q", tok, mode)
	}
}
