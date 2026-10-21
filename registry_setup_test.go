// SPDX-License-Identifier: MIT
package main

import (
	"bnema.dev/ermite-runner/internal/sandbox"
	"context"
	"testing"
)

func TestRegistryConfiguration(t *testing.T) {
	t.Setenv(envCache, "1")
	t.Setenv("ERMITE_RUNNER_ACTIONS_CACHE_MAX", "0")
	for _, tc := range []struct {
		registry, cache string
		on              bool
	}{{"", "1M", true}, {"1M", "2M", true}, {"0", "1M", false}, {"0G", "1M", false}, {" 0M ", "1M", false}, {"00", "1M", false}, {"0GiB", "1M", false}, {"10M", "1M", true}} {
		t.Setenv(envRegistryMax, tc.registry)
		t.Setenv("ERMITE_RUNNER_CACHE_MAX", tc.cache)
		cfg := sandbox.Config{StateDir: t.TempDir()}
		svc, _, err := setupCache(context.Background(), &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if svc.RegistryEnabled() != tc.on {
			t.Fatalf("%+v: registry=%v", tc, svc.RegistryEnabled())
		}
		_ = svc.CloseAll()
	}
	for _, bad := range []string{"invalid", "G", "-1G"} {
		t.Setenv(envRegistryMax, bad)
		cfg := sandbox.Config{StateDir: t.TempDir()}
		if _, _, err := setupCache(context.Background(), &cfg); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	t.Setenv(envCache, "0")
	t.Setenv(envRegistryMax, "invalid")
	cfg := sandbox.Config{StateDir: t.TempDir()}
	svc, _, err := setupCache(context.Background(), &cfg)
	if err != nil || svc != nil {
		t.Fatalf("cache off: %v %v", svc, err)
	}
}
