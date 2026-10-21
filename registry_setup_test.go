// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/actionscache"
	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// The three size caps share one parser: a zero size in any unit turns the
// registry proxy and the action archive cache off, and is refused for the
// cache API, which ERMITE_RUNNER_CACHE=0 turns off. Each error names its
// variable.
func TestCacheSizeConfiguration(t *testing.T) {
	t.Setenv(envCache, "1")
	for _, tc := range []struct {
		registry, actions, cache string
		registryOn, archivesOn   bool
	}{
		{"", "", "1M", true, true},
		{"1M", "0", "2M", true, false},
		{"0", "1M", "1M", false, true},
		{"0G", "0GiB", "1M", false, false},
		{" 0M ", "", "1M", false, true},
		{"00", "", "1M", false, true},
		{"0GiB", "", "1M", false, true},
		{"10M", "", "1M", true, true},
	} {
		t.Setenv(envRegistryMax, tc.registry)
		t.Setenv(actionscache.EnvMax, tc.actions)
		t.Setenv(cachesvc.EnvMax, tc.cache)
		cfg := sandbox.Config{StateDir: t.TempDir()}
		svc, archives, err := setupCache(context.Background(), &cfg)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if svc.RegistryEnabled() != tc.registryOn || (archives != nil) != tc.archivesOn {
			t.Fatalf("%+v: registry=%v archives=%v", tc, svc.RegistryEnabled(), archives != nil)
		}
		_ = svc.CloseAll()
	}
	t.Setenv(envRegistryMax, "")
	t.Setenv(actionscache.EnvMax, "")
	t.Setenv(cachesvc.EnvMax, "")
	for _, tc := range []struct{ env, value string }{
		{envRegistryMax, "invalid"}, {envRegistryMax, "G"}, {envRegistryMax, "-1G"},
		{actionscache.EnvMax, "x"},
		{cachesvc.EnvMax, "0"}, {cachesvc.EnvMax, "0G"}, {cachesvc.EnvMax, "-1"},
	} {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			cfg := sandbox.Config{StateDir: t.TempDir()}
			svc, _, err := setupCache(context.Background(), &cfg)
			if err == nil {
				_ = svc.CloseAll()
				t.Fatal("accepted")
			}
			if !strings.HasPrefix(err.Error(), tc.env+": ") {
				t.Fatalf("error does not name %s: %v", tc.env, err)
			}
		})
	}
	t.Setenv(envCache, "0")
	t.Setenv(envRegistryMax, "invalid")
	cfg := sandbox.Config{StateDir: t.TempDir()}
	svc, _, err := setupCache(context.Background(), &cfg)
	if err != nil || svc != nil {
		t.Fatalf("cache off: %v %v", svc, err)
	}
}
