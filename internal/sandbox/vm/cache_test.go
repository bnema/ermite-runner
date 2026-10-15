// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

func startVM(t *testing.T, d *Driver, job string) *vmSandbox {
	t.Helper()
	sb, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: job}); err != nil {
		t.Fatal(err)
	}
	return sb.(*vmSandbox)
}

// Each VM serves the cache on its own host-side vsock socket, <uds>_<cache
// port> in the VM's own directory (the path Firecracker connects to when the
// guest connects to the host on the cache port), for its own job only. Stopping
// one VM closes its channel and leaves the other alone.
func TestCacheVsockSocketPerVM(t *testing.T) {
	e := newEnv(t, "")
	svc := sandboxtest.NewCache(t)
	e.cfg.Cache = svc
	d := e.driver(t)
	a, b := startVM(t, d, "job-a"), startVM(t, d, "job-b")
	tokA, tokB := sandboxtest.CacheToken(svc, "job-a"), sandboxtest.CacheToken(svc, "job-b")

	if a.cacheSocket() == b.cacheSocket() {
		t.Fatal("two VMs share a cache socket")
	}
	for _, sb := range []*vmSandbox{a, b} {
		if !strings.HasSuffix(sb.cacheSocket(), "_9089") || !strings.HasPrefix(sb.cacheSocket(), sb.lay.root+"/") {
			t.Fatalf("cache socket %q is not <vm dir>/v.sock_9089", sb.cacheSocket())
		}
	}
	for _, tc := range []struct {
		sb   *vmSandbox
		tok  string
		want int
	}{
		{a, tokA, http.StatusNoContent},
		{b, tokB, http.StatusNoContent},
		{a, tokB, http.StatusForbidden},
		{b, tokA, http.StatusForbidden},
	} {
		got, err := sandboxtest.CacheLookup(tc.sb.cacheSocket(), tc.tok)
		if err != nil || got != tc.want {
			t.Errorf("%s: status %d err %v, want %d", tc.sb.ID(), got, err, tc.want)
		}
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("unix", a.cacheSocket()); err == nil {
		_ = c.Close()
		t.Fatal("A's cache socket answers after Stop")
	}
	if got, err := sandboxtest.CacheLookup(b.cacheSocket(), tokB); err != nil || got != http.StatusNoContent {
		t.Fatalf("B after A's Stop: %d %v", got, err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// A boot that fails after the channel was opened closes it.
func TestCacheChannelClosedWhenBootFails(t *testing.T) {
	e := newEnv(t, "hang") // the VMM never lets the executor connect
	svc := sandboxtest.NewCache(t)
	e.cfg.Cache = svc
	d, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	sb, _ := d.New()
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"}); err == nil || !strings.Contains(err.Error(), "did not connect") {
		t.Fatalf("expected a boot timeout, got %v", err)
	}
	_ = sb.Stop(context.Background())
	ln, err := net.Listen("unix", t.TempDir()+"/s")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Serve(sb.ID(), ln, "j"); err != nil {
		t.Fatalf("the channel of the failed boot is still registered: %v", err)
	}
}

// With no cache service the VM has no cache socket.
func TestNoCacheSocketWithoutService(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb := startVM(t, d, "j")
	if _, err := os.Stat(sb.cacheSocket()); err == nil {
		t.Fatal("a cache socket exists with no cache service")
	}
	_ = sb.Stop(context.Background())
}

// The socket path limit covers the cache socket too.
func TestSocketPathsIncludeTheCacheSocket(t *testing.T) {
	var found bool
	for _, p := range socketPaths("/var/lib/ermite", "firecracker", true) {
		found = found || strings.HasSuffix(p, "_9089")
	}
	if !found {
		t.Fatal("the cache socket is not checked against the sun_path limit")
	}
}
