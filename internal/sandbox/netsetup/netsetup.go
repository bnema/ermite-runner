// SPDX-License-Identifier: MIT

// Package netsetup is the host-side network plumbing shared by both sandbox
// drivers: the nftables policy (package netpolicy), the DNS forwarder (package
// dnsfwd) and IPv4 forwarding. Each driver creates its own interface (a TAP or
// a Docker bridge) and then Attaches it here.
package netsetup

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/dnsfwd"
	"bnema.dev/ermite-runner/internal/netpolicy"
)

// Plumbing is the host network surface a driver needs. Net implements it; tests
// of the drivers use a fake because binding gateway addresses and loading nft
// rules needs privileges.
type Plumbing interface {
	EnableForwarding() error
	Attach(ctx context.Context, g netpolicy.Guest) error
	Detach(ctx context.Context, iface string, gateway netip.Addr) error
	Reset(ctx context.Context) error
	Close()
}

var _ Plumbing = (*Net)(nil)

// Net is the host network state of one driver.
type Net struct {
	Policy *netpolicy.Manager
	DNS    *dnsfwd.Forwarder
	// Cache, when not nil, is the runner's cache service: it listens on the
	// gateway of every guest, on CachePort, and netpolicy opens that port to
	// that guest only.
	Cache     *cachesvc.Service
	CachePort uint16
	// CacheFreeBind lets the cache listener bind before the gateway address
	// exists (podman, like dnsfwd.Forwarder.FreeBind).
	CacheFreeBind bool
	// ForwardFile is the ip_forward sysctl; overridden in tests.
	ForwardFile string
}

// New builds a Net: nft runs through run, DNS listens on dnsPort and forwards
// to the resolvers of resolvConf ("" = /etc/resolv.conf).
func New(run cmdrun.Runner, dnsPort int, resolvConf string) *Net {
	r := dnsfwd.NewResolvers(resolvConf)
	return &Net{
		Policy:      netpolicy.NewManager(run, "nft"),
		DNS:         dnsfwd.New(r.Addrs, dnsPort),
		ForwardFile: "/proc/sys/net/ipv4/ip_forward",
	}
}

// EnableForwarding turns on IPv4 forwarding (NAT needs it). It is a no-op when
// already on, and an error that names the fix when it cannot be turned on.
func (n *Net) EnableForwarding() error {
	b, err := os.ReadFile(n.ForwardFile)
	if err == nil && strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	if err := os.WriteFile(n.ForwardFile, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding (%s): %w; set net.ipv4.ip_forward=1", n.ForwardFile, err)
	}
	return nil
}

// Attach installs the firewall policy of g and starts the DNS forwarder on its
// gateway address, which must already be configured on the host interface.
// On error nothing stays installed.
func (n *Net) Attach(ctx context.Context, g netpolicy.Guest) error {
	if n.Cache != nil {
		g.CachePort = n.CachePort
	}
	if err := n.Policy.Add(ctx, g); err != nil {
		return err
	}
	if err := n.DNS.Listen(ctx, g.Gateway); err != nil {
		return errors.Join(err, n.Policy.Remove(context.WithoutCancel(ctx), g.Iface))
	}
	if n.Cache != nil {
		if err := n.Cache.Listen(ctx, g.Gateway, n.CachePort, n.CacheFreeBind); err != nil {
			n.DNS.Close(g.Gateway)
			return errors.Join(err, n.Policy.Remove(context.WithoutCancel(ctx), g.Iface))
		}
	}
	return nil
}

// Detach undoes Attach. It is idempotent and keeps going after an error.
func (n *Net) Detach(ctx context.Context, iface string, gateway netip.Addr) error {
	n.DNS.Close(gateway)
	if n.Cache != nil {
		n.Cache.Close(gateway)
	}
	return n.Policy.Remove(ctx, iface)
}

// Reset removes the runner's nftables table: used at startup to reap a previous
// run. Only the table is touched, never other tables.
func (n *Net) Reset(ctx context.Context) error { return n.Policy.Reset(ctx) }

// Close stops every DNS listener. Cache listeners end with Detach; the cache
// service itself belongs to whoever made it.
func (n *Net) Close() {
	n.DNS.CloseAll()
}
