// SPDX-License-Identifier: MIT
package guest

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestRunDepsStartsRegistryOnlyRelayAndClosesIt(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	defer func() { _ = guest.Close() }()
	go func() { _, _ = io.Copy(io.Discard, host) }()
	c := execstream.NewConn(guest)

	var spec *execpb.JobSpec
	relayLive := false
	env := depsEnv{
		relay: func(_ context.Context, s *execpb.JobSpec) cacheRelay {
			spec = s
			relayLive = true
			return cacheRelay{}
		},
		run: func(context.Context, *execpb.DepsRequest, io.Writer) (map[string][]byte, error) {
			if !relayLive {
				t.Error("deps ran before the relay started")
			}
			return nil, nil
		},
	}
	if code := runDeps(context.Background(), c, &execpb.DepsRequest{RegistryPort: 5000, Token: "t"}, env); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if spec == nil || spec.CachePort != 5000 || spec.CacheToken != "" || spec.CacheMode != "" || spec.Results {
		t.Fatalf("spec %+v", spec)
	}
}

func TestRegistryOnlyRelayIsClosedByDeps(t *testing.T) {
	// A registry-only spec yields a relay that close() releases.
	port := freePort(t)
	r := startCacheRelay(context.Background(), Target{Network: "unix", Addr: "/nonexistent"}, &execpb.JobSpec{CachePort: port}, noBridge)
	if r.relay == nil || r.token != "" || r.bridge != nil {
		t.Fatalf("relay %+v", r)
	}
	r.close()
	if c, err := net.Dial("tcp", net.JoinHostPort(CacheListenHost, strconv.FormatUint(uint64(port), 10))); err == nil {
		_ = c.Close()
		t.Fatal("relay still listening")
	}
}
