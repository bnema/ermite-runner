// SPDX-License-Identifier: MIT

package netsetup

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// The host network surface of a guest is DNS and the firewall policy, nothing
// else: Attach opens no cache port, neither in the policy nor as a listener.
func TestAttachOpensNoCachePort(t *testing.T) {
	f := &cmdrun.Fake{}
	n := New(f, 0, "")
	t.Cleanup(n.Close)
	if err := n.Attach(context.Background(), guest()); err != nil {
		t.Skip("cannot bind loopback:", err)
	}
	rs := f.Calls[0].Stdin
	for _, port := range []string{"8089", "9089"} {
		if strings.Contains(rs, port) {
			t.Fatalf("the policy mentions the cache port %s:\n%s", port, rs)
		}
	}
	if !strings.Contains(rs, "dport 53 accept") {
		t.Fatalf("the policy lost DNS:\n%s", rs)
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
