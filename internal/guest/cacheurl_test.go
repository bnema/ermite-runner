// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestCacheURL(t *testing.T) {
	for _, tc := range []struct {
		port uint32
		want string
	}{
		// The toolkit appends "_apis/artifactcache/…" to this: the slash is needed.
		{9089, "http://127.0.0.1:9089/"},
		{0, ""},
		{70000, ""},
	} {
		if got := cacheURL(tc.port); got != tc.want {
			t.Errorf("cacheURL(%d) = %q, want %q", tc.port, got, tc.want)
		}
	}
}

// freePort returns a loopback port nobody listens on.
func freePort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return uint32(ln.Addr().(*net.TCPAddr).Port)
}

// A job with a cache service gets the relay on its loopback port, and the URL
// and token for the actions; the relay reaches the cache channel of the host,
// the sibling of the executor target, and ends with the job.
func TestStartCacheRelay(t *testing.T) {
	dir := t.TempDir()
	execSock := filepath.Join(dir, "exec.sock")
	ln, err := net.Listen("unix", filepath.Join(dir, "cache.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "cache says "+r.Header.Get("Authorization"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	port := freePort(t)
	spec := &execpb.JobSpec{CachePort: port, CacheToken: "tok", CacheMode: "read"}
	got := startCacheRelay(context.Background(), Target{Network: "unix", Addr: execSock}, spec)
	if got.relay == nil || got.token != "tok" || got.mode != "read" || got.url != cacheURL(port) {
		t.Fatalf("relay %+v", got)
	}

	req, _ := http.NewRequest("GET", got.url+"_apis/artifactcache/cache", nil)
	req.Header.Set("Authorization", "Bearer tok")
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "cache says Bearer tok" {
		t.Fatalf("got %q", body)
	}

	got.close()
	if c, err := net.DialTimeout("tcp", got.relay.Addr().String(), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the relay outlives the job")
	}
}

// No cache service, no relay and no variables; a port that is taken leaves the
// job without a cache instead of failing it.
func TestStartCacheRelayWithoutServiceOrPort(t *testing.T) {
	target := Target{Network: "unix", Addr: filepath.Join(t.TempDir(), "exec.sock")}
	for _, spec := range []*execpb.JobSpec{{}, {CachePort: 9089}, {CacheToken: "tok"}} {
		if got := startCacheRelay(context.Background(), target, spec); got != (cacheRelay{}) {
			t.Errorf("%+v: relay %+v", spec, got)
		}
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	defer func() { _ = busy.Close() }()
	spec := &execpb.JobSpec{CachePort: uint32(busy.Addr().(*net.TCPAddr).Port), CacheToken: "tok"}
	if got := startCacheRelay(context.Background(), target, spec); got != (cacheRelay{}) {
		t.Errorf("taken port: relay %+v", got)
	}
}
