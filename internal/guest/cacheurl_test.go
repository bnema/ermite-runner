// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
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

// noBridge is a sandbox without a docker daemon.
func noBridge() (net.IP, error) { return nil, errors.New("no docker0") }

// serveCache serves the cache channel next to the executor target in dir; it
// answers with the Authorization header it got.
func serveCache(t *testing.T, dir string) Target {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "cache.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "cache says "+r.Header.Get("Authorization"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return Target{Network: "unix", Addr: filepath.Join(dir, "exec.sock")}
}

// get fetches url with the token and returns the body.
func get(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"_apis/artifactcache/cache", nil)
	req.Header.Set("Authorization", "Bearer tok")
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(body)
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
	got := startCacheRelay(context.Background(), Target{Network: "unix", Addr: execSock}, spec, noBridge)
	if got.relay == nil || got.token != "tok" || got.mode != "read" || got.url != cacheURL(port) ||
		got.bridge != nil || got.containerURL != "" || got.results || got.cacheOff {
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
		if got := startCacheRelay(context.Background(), target, spec, noBridge); got != (cacheRelay{}) {
			t.Errorf("%+v: relay %+v", spec, got)
		}
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	defer func() { _ = busy.Close() }()
	spec := &execpb.JobSpec{CachePort: uint32(busy.Addr().(*net.TCPAddr).Port), CacheToken: "tok"}
	if got := startCacheRelay(context.Background(), target, spec, noBridge); got != (cacheRelay{}) {
		t.Errorf("taken port: relay %+v", got)
	}
}

// The relay also listens on the address of the docker bridge, on the same
// port, for the steps that run in a container; their URL has that address.
// 127.0.0.2 stands for docker0 (a loopback address that is bindable on Linux).
func TestStartCacheRelayOnBridge(t *testing.T) {
	target := serveCache(t, t.TempDir())
	port := freePort(t)
	probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		t.Skipf("127.0.0.2 is not bindable here: %v", err)
	}
	_ = probe.Close()
	bridge := func() (net.IP, error) { return net.ParseIP("127.0.0.2"), nil }
	got := startCacheRelay(context.Background(), target, &execpb.JobSpec{CachePort: port, CacheToken: "tok", CacheMode: "write"}, bridge)
	defer got.close()
	want := "http://127.0.0.2:" + strconv.FormatUint(uint64(port), 10) + "/"
	if got.bridge == nil || got.containerURL != want || got.url != cacheURL(port) {
		t.Fatalf("relay %+v", got)
	}
	for _, u := range []string{got.url, got.containerURL} {
		if body := get(t, u); body != "cache says Bearer tok" {
			t.Errorf("%s: got %q", u, body)
		}
	}
	got.close()
	if c, err := net.DialTimeout("tcp", got.bridge.Addr().String(), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the bridge relay outlives the job")
	}
}

// A bridge address that cannot be listened on leaves the loopback relay, and
// no URL for the containers.
func TestStartCacheRelayBridgeFails(t *testing.T) {
	target := serveCache(t, t.TempDir())
	port := freePort(t)
	// 192.0.2.1 (TEST-NET-1) is not an address of this machine.
	bridge := func() (net.IP, error) { return net.ParseIP("192.0.2.1"), nil }
	got := startCacheRelay(context.Background(), target, &execpb.JobSpec{CachePort: port, CacheToken: "tok"}, bridge)
	defer got.close()
	if got.relay == nil || got.bridge != nil || got.containerURL != "" {
		t.Fatalf("relay %+v", got)
	}
}

// JobSpec.results: the channel serves the results service; without a cache
// mode it serves nothing else, and the cache variables are off.
func TestStartCacheRelayResults(t *testing.T) {
	target := serveCache(t, t.TempDir())
	for _, tc := range []struct {
		mode              string
		results, cacheOff bool
	}{{"write", true, false}, {"", true, true}, {"read", false, false}, {"", false, false}} {
		spec := &execpb.JobSpec{CachePort: freePort(t), CacheToken: "tok", CacheMode: tc.mode, Results: tc.results}
		got := startCacheRelay(context.Background(), target, spec, noBridge)
		got.close()
		if got.relay == nil || got.results != tc.results || got.cacheOff != tc.cacheOff {
			t.Errorf("%+v: relay %+v", tc, got)
		}
	}
}

// docker0IPv4 without the interface is an error, not an address.
func TestDocker0IPv4(t *testing.T) {
	ip, err := docker0IPv4()
	if _, ierr := net.InterfaceByName(BridgeInterface); ierr != nil {
		if err == nil {
			t.Errorf("no %s, got %v", BridgeInterface, ip)
		}
		return
	}
	if err == nil && ip.To4() == nil {
		t.Errorf("not IPv4: %v", ip)
	}
}
