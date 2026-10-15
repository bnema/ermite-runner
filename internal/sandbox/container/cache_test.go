// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

func cacheSock(d *Driver, sb sandbox.Sandbox) string {
	return filepath.Join(d.sockDir(sb.ID()), sandbox.CacheSocket)
}

// Each container gets its own cache socket in its private run directory (the
// one directory bind-mounted into it), served for its own job only: a token of
// the other container's job gets a 403 on it, and nothing listens on a network.
// Stopping a container removes its channel and leaves the other one alone.
func TestCacheSocketPerContainer(t *testing.T) {
	e := newCEnv(t)
	svc := sandboxtest.NewCache(t)
	e.cfg.Cache = svc
	d := e.driver(t)
	start := func(job string) sandbox.Sandbox {
		t.Helper()
		sb, err := d.New()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: job}); err != nil {
			t.Fatal(err)
		}
		return sb
	}
	a, b := start("job-a"), start("job-b")
	tokA, tokB := sandboxtest.CacheToken(svc, "job-a"), sandboxtest.CacheToken(svc, "job-b")

	if cacheSock(d, a) == cacheSock(d, b) {
		t.Fatal("two containers share a cache socket")
	}
	// The socket is in the directory that is mounted as /run/ermite, next to
	// the executor socket, and in no other container's.
	e.eng.mu.Lock()
	mounts := map[string]bool{}
	for _, c := range e.eng.containers {
		for _, bm := range c.Binds {
			if bm.Target == sandbox.ContainerRunDir {
				mounts[bm.Source] = true
			}
		}
	}
	e.eng.mu.Unlock()
	for _, sb := range []sandbox.Sandbox{a, b} {
		if !mounts[filepath.Dir(cacheSock(d, sb))] {
			t.Fatalf("%s: the cache socket is not in a directory mounted in a container", sb.ID())
		}
		if st, err := os.Stat(cacheSock(d, sb)); err != nil || st.Mode().Perm() != 0o666 {
			t.Fatalf("%s: cache socket %v %v", sb.ID(), st, err)
		}
	}

	for _, tc := range []struct {
		sb   sandbox.Sandbox
		tok  string
		want int
	}{
		{a, tokA, http.StatusNoContent},
		{b, tokB, http.StatusNoContent},
		{a, tokB, http.StatusForbidden},
		{b, tokA, http.StatusForbidden},
	} {
		got, err := sandboxtest.CacheLookup(cacheSock(d, tc.sb), tc.tok)
		if err != nil || got != tc.want {
			t.Errorf("%s: status %d err %v, want %d", tc.sb.ID(), got, err, tc.want)
		}
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("unix", cacheSock(d, a)); err == nil {
		_ = c.Close()
		t.Fatal("A's cache socket answers after Stop")
	}
	if got, err := sandboxtest.CacheLookup(cacheSock(d, b), tokB); err != nil || got != http.StatusNoContent {
		t.Fatalf("B after A's Stop: %d %v", got, err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// A failed start does not leave the channel behind.
func TestCacheSocketClosedWhenStartFails(t *testing.T) {
	e := newCEnv(t)
	svc := sandboxtest.NewCache(t)
	e.cfg.Cache = svc
	e.eng.failStart = os.ErrInvalid
	d := e.driver(t)
	sb, _ := d.New()
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"}); err == nil {
		t.Fatal("expected a start error")
	}
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The same sandbox id could be served again: the service forgot it.
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Serve(sb.ID(), ln, "j"); err != nil {
		t.Fatalf("the channel of the failed start is still registered: %v", err)
	}
}

// The sun_path limit covers the cache socket, which is one byte longer than the
// executor socket: a directory that fits the executor socket exactly is refused
// when the cache is on, and still accepted when it is not.
func TestSockPathLimitCoversTheCacheSocket(t *testing.T) {
	pad := func(n int) string { return "/" + strings.Repeat("d", n-1) }
	// dir + "/exec.sock" is exactly maxSockPath bytes.
	exact := pad(maxSockPath - len("/"+sandbox.ContainerSocket))
	if len(filepath.Join(exact, sandbox.ContainerSocket)) != maxSockPath {
		t.Fatalf("setup: %d", len(filepath.Join(exact, sandbox.ContainerSocket)))
	}
	if err := checkSockPaths(exact, false); err != nil {
		t.Fatalf("%d-byte executor socket without cache: %v", maxSockPath, err)
	}
	err := checkSockPaths(exact, true)
	if err == nil || !strings.Contains(err.Error(), "cache socket path") || !strings.Contains(err.Error(), sandbox.EnvStateDir) {
		t.Fatalf("%d-byte executor socket with cache: %v", maxSockPath, err)
	}
	// One byte shorter: both fit.
	if err := checkSockPaths(exact[:len(exact)-1], true); err != nil {
		t.Fatalf("both sockets fit: %v", err)
	}
	// One byte longer: the executor socket is refused first.
	if err := checkSockPaths(exact+"d", true); err == nil || !strings.Contains(err.Error(), "executor socket path") {
		t.Fatalf("executor socket over the limit: %v", err)
	}
}

// At Start, a state directory that fits the executor socket but not the cache
// socket fails with the actionable error and leaves nothing behind.
func TestStartRefusesACacheSocketPathOverTheLimit(t *testing.T) {
	e := newCEnv(t)
	e.cfg.Cache = sandboxtest.NewCache(t)
	pid, err := sandbox.NewID()
	if err != nil {
		t.Fatal(err)
	}
	// Grow the state directory so that exec.sock is exactly at the limit (ids
	// have a fixed length).
	probe := &Driver{cfg: e.cfg}
	grow := maxSockPath - len(filepath.Join(probe.sockDir(pid), sandbox.ContainerSocket))
	if grow < 2 {
		t.Skipf("state directory already %d bytes over", -grow)
	}
	e.cfg.StateDir = e.cfg.StateDir + "/" + strings.Repeat("d", grow-1)
	d := e.driver(t)
	sb, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(filepath.Join(d.sockDir(sb.ID()), sandbox.ContainerSocket)); got != maxSockPath {
		t.Fatalf("setup: executor socket path is %d bytes, want %d", got, maxSockPath)
	}
	_, err = sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err == nil || !strings.Contains(err.Error(), "cache socket path") || !strings.Contains(err.Error(), "limit of a Unix socket") {
		t.Fatalf("got %v", err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// Without a cache service there is no cache socket.
func TestNoCacheSocketWithoutService(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheSock(d, sb)); err == nil {
		t.Fatal("a cache socket exists with no cache service")
	}
	_ = sb.Stop(context.Background())
}
