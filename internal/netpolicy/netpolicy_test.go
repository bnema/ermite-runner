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
