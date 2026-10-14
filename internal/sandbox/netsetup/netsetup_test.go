// SPDX-License-Identifier: MIT

package netsetup

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/netpolicy"
)

func guest() netpolicy.Guest {
	return netpolicy.Guest{Iface: "ertap0", Gateway: netip.MustParseAddr("127.0.0.1"), Addr: netip.MustParseAddr("10.200.0.2")}
}

func TestAttachDetach(t *testing.T) {
	f := &cmdrun.Fake{}
	n := New(f, 0, "")
	t.Cleanup(n.Close)
	ctx := context.Background()
	if err := n.Attach(ctx, guest()); err != nil {
		t.Skip("cannot bind loopback:", err)
	}
	if _, ok := n.DNS.Addr(guest().Gateway); !ok {
		t.Fatal("DNS forwarder not listening on the gateway")
	}
	if err := n.Detach(ctx, "ertap0", guest().Gateway); err != nil {
		t.Fatal(err)
	}
	if _, ok := n.DNS.Addr(guest().Gateway); ok {
		t.Fatal("DNS forwarder still listening after Detach")
	}
	if len(f.Calls) != 2 || f.Calls[1].Stdin != netpolicy.Ruleset(nil) {
		t.Fatalf("calls: %v", f.Lines())
	}
	if err := n.Detach(ctx, "ertap0", guest().Gateway); err != nil {
		t.Fatalf("Detach must be idempotent: %v", err)
	}
}

func TestAttachRollsBackPolicyWhenDNSFails(t *testing.T) {
	f := &cmdrun.Fake{}
	n := New(f, 0, "")
	t.Cleanup(n.Close)
	ctx := context.Background()
	if err := n.Attach(ctx, guest()); err != nil {
		t.Skip(err)
	}
	before := len(f.Calls)
	if err := n.Attach(ctx, guest()); err == nil { // already listening
		t.Fatal("second Attach on the same gateway must fail")
	}
	if len(f.Calls) != before+2 {
		t.Fatalf("expected add then rollback remove, got %v", f.Lines())
	}
}

func TestAttachPolicyError(t *testing.T) {
	boom := errors.New("nft failed")
	n := New(&cmdrun.Fake{Handle: func(cmdrun.Call) ([]byte, error) { return nil, boom }}, 0, "")
	if err := n.Attach(context.Background(), guest()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, ok := n.DNS.Addr(guest().Gateway); ok {
		t.Fatal("DNS must not start when the policy failed")
	}
}

func TestEnableForwarding(t *testing.T) {
	n := New(&cmdrun.Fake{}, 0, "")
	n.ForwardFile = filepath.Join(t.TempDir(), "ip_forward")
	_ = os.WriteFile(n.ForwardFile, []byte("0\n"), 0o644)
	if err := n.EnableForwarding(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(n.ForwardFile); strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("not enabled: %q", b)
	}
	n.ForwardFile = filepath.Join(t.TempDir(), "missing", "ip_forward")
	if err := n.EnableForwarding(); err == nil || !strings.Contains(err.Error(), "ip_forward") {
		t.Fatalf("want a clear error, got %v", err)
	}
}

// With a cache service, Attach opens its port in the policy of that guest and
// serves it on the gateway; Detach closes both.
func TestAttachDetachCache(t *testing.T) {
	probe := listenLoopback(t)
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()

	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	f := &cmdrun.Fake{}
	n := New(f, 0, "")
	n.Cache, n.CachePort = svc, port
	t.Cleanup(n.Close)
	ctx := context.Background()
	if err := n.Attach(ctx, guest()); err != nil {
		t.Fatalf("Attach: %v", err) // the port was free a moment ago: not an environmental skip
	}
	if rs := f.Calls[0].Stdin; !strings.Contains(rs, "ip daddr 127.0.0.1 tcp dport "+itoa(port)+" iifname != \"ertap0\" drop") ||
		!strings.Contains(rs, "iifname \"ertap0\" ip saddr 10.200.0.2 ip daddr 127.0.0.1 tcp dport "+itoa(port)+" accept") {
		t.Fatalf("the policy lacks the cache port:\n%s", rs)
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	url := "http://127.0.0.1:" + itoa(port) + cachesvc.APIPath + "cache?keys=k&version=v"
	resp, err := cli.Get(url)
	if err != nil {
		t.Fatalf("the cache does not answer on the gateway: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden { // no token
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := n.Detach(ctx, "ertap0", guest().Gateway); err != nil {
		t.Fatal(err)
	}
	if resp, err := cli.Get(url); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the cache still answers after Detach")
	}
}

// A failure to serve the cache rolls back the policy and the DNS listener.
func TestAttachRollsBackWhenCacheFails(t *testing.T) {
	busy := listenLoopback(t)
	defer func() { _ = busy.Close() }()
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	f := &cmdrun.Fake{}
	n := New(f, 0, "")
	n.Cache, n.CachePort = svc, uint16(busy.Addr().(*net.TCPAddr).Port)
	t.Cleanup(n.Close)
	if err := n.Attach(context.Background(), guest()); err == nil {
		t.Fatal("Attach must fail when the cache port is taken")
	}
	if _, ok := n.DNS.Addr(guest().Gateway); ok {
		t.Fatal("DNS still listening after the rollback")
	}
	if len(f.Calls) != 2 || f.Calls[1].Stdin != netpolicy.Ruleset(nil) {
		t.Fatalf("calls: %v", f.Lines())
	}
}

// listenLoopback binds a free loopback port. Only a sandbox that forbids
// sockets altogether (EPERM, EACCES, EAFNOSUPPORT) skips the test; any other
// error fails it.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAFNOSUPPORT) {
			t.Skip("loopback sockets are not allowed here:", err)
		}
		t.Fatal(err)
	}
	return ln
}

func itoa(p uint16) string { return strconv.Itoa(int(p)) }
