// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestDefaultDockerdMirror(t *testing.T) {
	args := strings.Join(defaultDockerdArgs(), " ")
	if !strings.Contains(args, "--registry-mirror http://127.0.0.1:9089") {
		t.Fatal(args)
	}
}

// A registry-only job (a port, no token, no mode, no results) gets the
// loopback relay for dockerd and no runtime environment for its steps.
func TestRegistryOnlyRelayHasNoRuntimeEnvironment(t *testing.T) {
	target := serveCache(t, t.TempDir())
	got := startCacheRelay(context.Background(), target, &execpb.JobSpec{CachePort: freePort(t)}, noBridge)
	defer got.close()
	if got.relay == nil {
		t.Fatal("missing loopback relay")
	}
	if got.token != "" {
		t.Fatalf("token %q for a registry-only job", got.token)
	}
	if got.url != "" || got.containerURL != "" || got.mode != "" || got.results || got.bridge != nil {
		t.Fatalf("runtime environment leaked: %+v", got)
	}
}
