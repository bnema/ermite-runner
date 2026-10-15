// SPDX-License-Identifier: MIT

// Package netpolicy generates and applies the nftables policy of job sandboxes.
//
// All rules live in one dedicated table, "inet ermite_runner", that is replaced
// atomically (one `nft -f -` transaction) whenever a sandbox is added or
// removed, and deleted when none is left. Nothing outside that table is touched.
//
// Policy for every guest interface (a TAP in a micro-VM, a bridge in a
// container):
//   - to the host itself (input hook): only DNS (udp/tcp 53) to the gateway
//     address of that guest, where the runner's DNS forwarder listens; every
//     other packet is dropped, and port 53 of a gateway is closed to every
//     other interface;
//   - optionally (Guest.ForgePort, the opt-in guest forge access, staging
//     only): TCP from the guest's source address to its own gateway address on
//     that one port. The host's own listener on that port receives the traffic;
//     nothing is proxied, and every other host port stays closed;
//     the GitHub Actions cache has no rule here: it is not on the network (the
//     guest reaches it on its own loopback, through the host channel);
//   - IPv6 from the guest is dropped (the guest also disables IPv6);
//   - forwarded traffic must carry the guest's own source address;
//   - forwarded traffic to RFC 1918, CGNAT 100.64.0.0/10 (VPN overlays),
//     link-local 169.254.0.0/16 (cloud metadata 169.254.169.254), loopback,
//     "this network", multicast and reserved ranges is dropped;
//   - traffic towards a guest interface that does not belong to an established
//     connection is dropped, so guests cannot reach each other;
//   - everything else is NATed (masquerade) to the internet.
package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

// Table is the nftables table owned by the runner.
const Table = "ermite_runner"

// IfacePrefixes are the interface name prefixes of sandboxes; wildcard rules
// are generated for them. TAP devices of micro-VMs, bridges of containers.
var IfacePrefixes = []string{"ertap", "erbr"}

// blockedV4 are the destinations a guest may never reach.
var blockedV4 = []string{
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // RFC 1918
	"100.64.0.0/10",   // RFC 6598 shared address space (CGNAT, VPN overlays)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, incl. the 169.254.169.254 metadata service
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, incl. limited broadcast
}

// prefixSet renders IfacePrefixes as an nft anonymous set of interface wildcards.
func prefixSet() string {
	q := make([]string, len(IfacePrefixes))
	for i, p := range IfacePrefixes {
		q[i] = `"` + p + `*"`
	}
	return "{ " + strings.Join(q, ", ") + " }"
}

// Guest is one sandbox network attachment.
type Guest struct {
	Iface   string     // host-side interface: TAP or bridge name
	Gateway netip.Addr // host address on Iface; the DNS forwarder listens here
	Addr    netip.Addr // the guest's only allowed source address
	// ForgePort, when not 0, opens TCP from Addr to Gateway on that one port
	// (guest forge access). The accept is in the input chain, scoped to this
	// guest's interface, source and gateway, so it neither reaches other
	// addresses of the host nor other guests.
	ForgePort uint16
}

// Validate checks the fields used in generated rules.
func (g Guest) Validate() error {
	if !validIface(g.Iface) {
		return fmt.Errorf("netpolicy: invalid interface name %q", g.Iface)
	}
	if !g.Gateway.Is4() || !g.Addr.Is4() {
		return errors.New("netpolicy: guest and gateway must be IPv4 addresses")
	}
	return nil
}

func validIface(s string) bool {
	if s == "" || len(s) > 15 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// Net is the /30 given to one sandbox.
type Net struct {
	Prefix  netip.Prefix // the /30
	Gateway netip.Addr   // host side (.1)
	Guest   netip.Addr   // guest side (.2)
}

// Subnet returns the slot-th /30 of base (an IPv4 prefix of at most /30).
func Subnet(base netip.Prefix, slot int) (Net, error) {
	if !base.Addr().Is4() || base.Bits() > 30 || slot < 0 {
		return Net{}, fmt.Errorf("netpolicy: bad base %s or slot %d", base, slot)
	}
	if slot >= 1<<(30-base.Bits()) {
		return Net{}, fmt.Errorf("netpolicy: slot %d does not fit in %s", slot, base)
	}
	b := base.Masked().Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(slot) * 4
	addr := func(off uint32) netip.Addr {
		x := v + off
		return netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
	}
	return Net{Prefix: netip.PrefixFrom(addr(0), 30), Gateway: addr(1), Guest: addr(2)}, nil
}

// Ruleset returns the nft script that atomically replaces the runner table with
// the policy for guests. With no guest it only deletes the table. The output is
// deterministic (guests sorted by interface).
func Ruleset(guests []Guest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\n", Table, Table)
	if len(guests) == 0 {
		return b.String()
	}
	gs := append([]Guest(nil), guests...)
	sort.Slice(gs, func(i, j int) bool { return gs[i].Iface < gs[j].Iface })

	fmt.Fprintf(&b, "table inet %s {\n", Table)
	b.WriteString("\tset blocked_v4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { ")
	b.WriteString(strings.Join(blockedV4, ", "))
	b.WriteString(" }\n\t}\n")

	b.WriteString("\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n")
	for _, p := range IfacePrefixes {
		fmt.Fprintf(&b, "\t\tiifname \"%s*\" meta nfproto ipv6 drop\n", p)
	}
	// The DNS forwarder of a guest answers that guest only: port 53 of a gateway
	// address is dropped from every other interface (other guests, the LAN).
	for _, g := range gs {
		for _, proto := range []string{"udp", "tcp"} {
			fmt.Fprintf(&b, "\t\tip daddr %s %s dport 53 iifname != \"%s\" drop\n", g.Gateway, proto, g.Iface)
		}
	}
	for _, g := range gs {
		for _, proto := range []string{"udp", "tcp"} {
			fmt.Fprintf(&b, "\t\tiifname \"%s\" ip saddr %s ip daddr %s %s dport 53 accept\n", g.Iface, g.Addr, g.Gateway, proto)
		}
		if g.ForgePort != 0 {
			// New connections only: the replies leave through the output hook,
			// which this table does not touch.
			fmt.Fprintf(&b, "\t\tiifname \"%s\" ip saddr %s ip daddr %s tcp dport %d accept\n", g.Iface, g.Addr, g.Gateway, g.ForgePort)
		}
		fmt.Fprintf(&b, "\t\tiifname \"%s\" drop\n", g.Iface)
	}
	// An interface with our prefix that no live guest owns (a leftover) is
	// dropped as well.
	for _, p := range IfacePrefixes {
		fmt.Fprintf(&b, "\t\tiifname \"%s*\" drop\n", p)
	}
	b.WriteString("\t}\n")

	// Forward chain. Order matters: everything a guest sends is checked (family,
	// source address, destination ranges) BEFORE any "established" accept, or a
	// guest could forge the source of another guest's flow and have it accepted
	// as a reply.
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n")
	for _, p := range IfacePrefixes {
		fmt.Fprintf(&b, "\t\tiifname \"%s*\" meta nfproto ipv6 drop\n", p)
	}
	for _, g := range gs {
		fmt.Fprintf(&b, "\t\tiifname \"%s\" ip saddr != %s drop\n", g.Iface, g.Addr)
		fmt.Fprintf(&b, "\t\tiifname \"%s\" ip daddr @blocked_v4 drop\n", g.Iface)
	}
	// Replies from outside to a guest; never for packets that come from a guest.
	b.WriteString("\t\tiifname != " + prefixSet() + " oifname " + prefixSet() + " ct state established,related accept\n")
	for _, p := range IfacePrefixes {
		fmt.Fprintf(&b, "\t\toifname \"%s*\" drop\n", p)
	}
	for _, g := range gs {
		fmt.Fprintf(&b, "\t\tiifname \"%s\" accept\n", g.Iface)
	}
	for _, p := range IfacePrefixes {
		fmt.Fprintf(&b, "\t\tiifname \"%s*\" drop\n", p)
	}
	b.WriteString("\t}\n")

	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	for _, g := range gs {
		fmt.Fprintf(&b, "\t\tip saddr %s oifname != \"%s\" masquerade\n", g.Addr, g.Iface)
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// Manager applies the policy for the set of live guests.
type Manager struct {
	run    cmdrun.Runner
	nft    string
	mu     sync.Mutex
	guests map[string]Guest
}

// NewManager returns a Manager that runs nft (path or name) through run.
func NewManager(run cmdrun.Runner, nft string) *Manager {
	if nft == "" {
		nft = "nft"
	}
	return &Manager{run: run, nft: nft, guests: map[string]Guest{}}
}

// Add installs (or replaces) the policy of g.
func (m *Manager) Add(ctx context.Context, g Guest) error {
	if err := g.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, had := m.guests[g.Iface]
	m.guests[g.Iface] = g
	if err := m.applyLocked(ctx); err != nil {
		if had {
			m.guests[g.Iface] = prev
		} else {
			delete(m.guests, g.Iface)
		}
		return err
	}
	return nil
}

// Remove drops the policy of iface; the table disappears with the last guest.
// Removing an unknown interface is not an error.
func (m *Manager) Remove(ctx context.Context, iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.guests[iface]; !ok {
		return nil
	}
	delete(m.guests, iface)
	return m.applyLocked(ctx)
}

// Reset deletes the runner table whatever its content: used at start to reap
// the table of a previous run. Safe when the table does not exist.
func (m *Manager) Reset(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.guests = map[string]Guest{}
	return m.applyLocked(ctx)
}

func (m *Manager) applyLocked(ctx context.Context) error {
	gs := make([]Guest, 0, len(m.guests))
	for _, g := range m.guests {
		gs = append(gs, g)
	}
	// `table X` creates the table if missing so that the following delete cannot
	// fail; the whole script is one transaction.
	if _, err := m.run.Run(ctx, strings.NewReader(Ruleset(gs)), m.nft, "-f", "-"); err != nil {
		return fmt.Errorf("netpolicy: apply ruleset: %w", err)
	}
	return nil
}
