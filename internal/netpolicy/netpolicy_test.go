// SPDX-License-Identifier: MIT

package netpolicy

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from golden (run with -update to refresh):\n%s", name, got)
	}
}

func guest(t *testing.T, iface string, slot int) Guest {
	t.Helper()
	n, err := Subnet(netip.MustParsePrefix("10.200.0.0/16"), slot)
	if err != nil {
		t.Fatal(err)
	}
	return Guest{Iface: iface, Gateway: n.Gateway, Addr: n.Guest}
}

func TestRulesetGolden(t *testing.T) {
	golden(t, "empty.nft", Ruleset(nil))
	golden(t, "one.nft", Ruleset([]Guest{guest(t, "ertap0", 0)}))
	// Order of input must not matter.
	two := Ruleset([]Guest{guest(t, "erbr1", 1), guest(t, "ertap0", 0)})
	if two != Ruleset([]Guest{guest(t, "ertap0", 0), guest(t, "erbr1", 1)}) {
		t.Fatal("ruleset depends on guest order")
	}
	golden(t, "two.nft", two)
}

func TestRulesetPolicyInvariants(t *testing.T) {
	rs := Ruleset([]Guest{guest(t, "ertap0", 0)})
	for _, want := range []string{
		"169.254.0.0/16", "100.64.0.0/10", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		`iifname "ertap0" drop`, "dport 53 accept", "masquerade", `iifname "ertap*" meta nfproto ipv6 drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	// The drop for the host must come after the DNS accepts and the guest
	// table must never use a drop policy (it would break other tables' traffic).
	if strings.Contains(rs, "policy drop") {
		t.Error("base chains must keep policy accept")
	}
	if strings.Index(rs, "dport 53 accept") > strings.Index(rs, `iifname "ertap0" drop`) {
		t.Error("DNS accept must precede the catch-all drop")
	}
}

// S1: nothing a guest sends may be accepted by a conntrack rule before the
// per-guest checks: a forged "reply" from guest A to guest B's flow would
// otherwise be accepted as established traffic.
func TestForwardChainChecksGuestTrafficBeforeEstablished(t *testing.T) {
	rs := Ruleset([]Guest{guest(t, "ertap0", 0), guest(t, "erbr1", 1)})
	fwd := rs[strings.Index(rs, "chain forward"):strings.Index(rs, "chain postrouting")]
	lines := strings.Split(fwd, "\n")
	idx := func(sub string) int {
		for i, l := range lines {
			if strings.Contains(l, sub) {
				return i
			}
		}
		t.Fatalf("forward chain lacks %q:\n%s", sub, fwd)
		return -1
	}
	est := idx("ct state established,related accept")
	for _, must := range []string{
		`iifname "ertap*" meta nfproto ipv6 drop`, `iifname "erbr*" meta nfproto ipv6 drop`,
		`iifname "ertap0" ip saddr != 10.200.0.2 drop`, `iifname "ertap0" ip daddr @blocked_v4 drop`,
		`iifname "erbr1" ip saddr != 10.200.0.6 drop`, `iifname "erbr1" ip daddr @blocked_v4 drop`,
	} {
		if idx(must) > est {
			t.Errorf("%q must come before the established accept", must)
		}
	}
	// And the established accept itself never applies to packets that enter
	// through a sandbox interface.
	if !strings.Contains(lines[est], `iifname != { "ertap*", "erbr*" }`) {
		t.Errorf("established accept must exclude sandbox ingress: %s", lines[est])
	}
}

// S6: port 53 of a gateway address is closed to every interface but its guest's.
func TestInputChainClosesGatewayDNSToOtherInterfaces(t *testing.T) {
	rs := Ruleset([]Guest{guest(t, "ertap0", 0), guest(t, "ertap1", 1)})
	for _, want := range []string{
		`ip daddr 10.200.0.1 udp dport 53 iifname != "ertap0" drop`,
		`ip daddr 10.200.0.1 tcp dport 53 iifname != "ertap0" drop`,
		`ip daddr 10.200.0.5 udp dport 53 iifname != "ertap1" drop`,
		`ip daddr 10.200.0.5 tcp dport 53 iifname != "ertap1" drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("lacks %q", want)
		}
	}
}

// S10: documentation, test-net and benchmarking ranges are blocked too.
func TestBlockedSetCoversSpecialUseRanges(t *testing.T) {
	rs := Ruleset([]Guest{guest(t, "ertap0", 0)})
	for _, r := range []string{"192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"} {
		if !strings.Contains(rs, r) {
			t.Errorf("blocked set lacks %s", r)
		}
	}
}

func TestSubnet(t *testing.T) {
	base := netip.MustParsePrefix("10.200.0.0/16")
	n, err := Subnet(base, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n.Prefix.String() != "10.200.0.12/30" || n.Gateway.String() != "10.200.0.13" || n.Guest.String() != "10.200.0.14" {
		t.Fatalf("got %+v", n)
	}
	n, _ = Subnet(base, 64)
	if n.Gateway.String() != "10.200.1.1" {
		t.Fatalf("carry: %+v", n)
	}
	if _, err := Subnet(base, 1<<14); err == nil {
		t.Fatal("slot beyond the prefix accepted")
	}
	if _, err := Subnet(netip.MustParsePrefix("10.0.0.0/31"), 0); err == nil {
		t.Fatal("prefix longer than /30 accepted")
	}
	if _, err := Subnet(base, -1); err == nil {
		t.Fatal("negative slot accepted")
	}
}

func TestGuestValidate(t *testing.T) {
	g := guest(t, "ertap0", 0)
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "a b", `x"y`, "a;b", "waytoolonginterfacename", "x\ny"} {
		g.Iface = bad
		if g.Validate() == nil {
			t.Errorf("iface %q accepted", bad)
		}
	}
	g.Iface = "ok"
	g.Addr = netip.MustParseAddr("::1")
	if g.Validate() == nil {
		t.Error("IPv6 guest accepted")
	}
}

func TestManager(t *testing.T) {
	f := &cmdrun.Fake{}
	m := NewManager(f, "nft")
	ctx := context.Background()
	if err := m.Add(ctx, guest(t, "ertap0", 0)); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(ctx, guest(t, "ertap1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "ertap0"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("want 3 nft calls (unknown iface is a no-op), got %v", f.Lines())
	}
	last := f.Calls[2]
	if last.Name != "nft" || strings.Join(last.Args, " ") != "-f -" {
		t.Fatalf("bad command %v", last)
	}
	if strings.Contains(last.Stdin, "ertap0") || !strings.Contains(last.Stdin, "ertap1") {
		t.Fatalf("after removing ertap0 only ertap1 must remain:\n%s", last.Stdin)
	}
	if err := m.Remove(ctx, "ertap1"); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[3].Stdin; got != Ruleset(nil) {
		t.Fatalf("last removal must delete the table, got:\n%s", got)
	}
	if err := m.Reset(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRollsBackOnError(t *testing.T) {
	boom := errors.New("boom")
	f := &cmdrun.Fake{Handle: func(cmdrun.Call) ([]byte, error) { return nil, boom }}
	m := NewManager(f, "")
	if err := m.Add(context.Background(), guest(t, "ertap0", 0)); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	f.Handle = nil
	if err := m.Remove(context.Background(), "ertap0"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("a guest whose Add failed must not be tracked: %v", f.Lines())
	}
}

var _ io.Reader = strings.NewReader("")

func forgeGuest(t *testing.T, iface string, slot int, port uint16) Guest {
	t.Helper()
	g := guest(t, iface, slot)
	g.ForgePort = port
	return g
}

func TestRulesetGuestForgeGolden(t *testing.T) {
	golden(t, "guest_forge.nft", Ruleset([]Guest{forgeGuest(t, "ertap0", 0, 8088)}))
}

// Without ForgePort the ruleset has no TCP rule beyond DNS: the exception is
// opt-in (and one.nft, the golden file of that case, is unchanged).
func TestRulesetWithoutForgePortHasNoForgeRule(t *testing.T) {
	rs := Ruleset([]Guest{guest(t, "ertap0", 0)})
	if n, dns := strings.Count(rs, "tcp dport"), strings.Count(rs, "tcp dport 53 "); n != dns {
		t.Fatalf("%d tcp port rules, %d of them for DNS:\n%s", n, dns, rs)
	}
}

// The exception is one TCP port towards the guest's own gateway, from its own
// address and interface, placed before the guest's catch-all drop; a second
// guest gets its own, never the other's gateway; nothing else opens, and the
// forward chain (private ranges, source checks, sandbox-to-sandbox) is the
// same with and without it.
func TestGuestForgeRuleScopeAndOrder(t *testing.T) {
	gs := []Guest{forgeGuest(t, "ertap0", 0, 8088), forgeGuest(t, "erbr1", 1, 8088), guest(t, "ertap2", 2)}
	rs := Ruleset(gs)
	in := rs[strings.Index(rs, "chain input"):strings.Index(rs, "chain forward")]
	r0 := `iifname "ertap0" ip saddr 10.200.0.2 ip daddr 10.200.0.1 tcp dport 8088 accept`
	r1 := `iifname "erbr1" ip saddr 10.200.0.6 ip daddr 10.200.0.5 tcp dport 8088 accept`
	for _, want := range []string{r0, r1} {
		if strings.Count(in, want) != 1 {
			t.Errorf("want exactly one %q in the input chain:\n%s", want, in)
		}
	}
	if strings.Count(rs, "8088") != 2 {
		t.Errorf("the port appears %d times, want 2 (one per guest with the exception):\n%s", strings.Count(rs, "8088"), rs)
	}
	if strings.Index(in, r0) > strings.Index(in, `iifname "ertap0" drop`) || strings.Index(in, r1) > strings.Index(in, `iifname "erbr1" drop`) {
		t.Error("the forge accept must precede the guest's catch-all drop")
	}
	// The guest without the exception has none.
	if strings.Contains(in, `iifname "ertap2" ip saddr 10.200.0.10 ip daddr 10.200.0.9 tcp dport 8088`) {
		t.Error("ertap2 got a forge rule")
	}
	// The forward and postrouting chains do not depend on the exception.
	plain := Ruleset([]Guest{guest(t, "ertap0", 0), guest(t, "erbr1", 1), guest(t, "ertap2", 2)})
	tail := func(s string) string { return s[strings.Index(s, "chain forward"):] }
	if tail(rs) != tail(plain) {
		t.Error("the forward/postrouting chains changed with the exception")
	}
	// Every accept of the input chain that is not DNS is one of the two.
	for _, l := range strings.Split(in, "\n") {
		if strings.HasSuffix(l, "accept") && !strings.Contains(l, "dport 53 accept") && !strings.Contains(l, "tcp dport 8088 accept") {
			t.Errorf("unexpected accept: %s", l)
		}
	}
}
