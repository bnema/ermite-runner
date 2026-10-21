// SPDX-License-Identifier: MIT
package guest

import (
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"context"
	"strings"
	"testing"
)

func TestDefaultDockerdMirror(t *testing.T) {
	args := strings.Join(defaultDockerdArgs(), " ")
	if !strings.Contains(args, "--registry-mirror http://127.0.0.1:9089") {
		t.Fatal(args)
	}
}
func TestRegistryOnlyRelayHasNoRuntimeEnvironment(t *testing.T) {
	target := serveCache(t, t.TempDir())
	got := startCacheRelay(context.Background(), target, &execpb.JobSpec{CachePort: freePort(t), CacheToken: "unscoped"}, noBridge)
	defer got.close()
	if got.relay == nil {
		t.Fatal("missing loopback relay")
	}
	if got.url != "" || got.containerURL != "" || got.token != "" || got.mode != "" || got.results || got.bridge != nil {
		t.Fatalf("runtime environment leaked: %+v", got)
	}
}
