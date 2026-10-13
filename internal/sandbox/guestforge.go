// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EnvGuestForge is ERMITE_RUNNER_GUEST_FORGE=1: sandboxes reach the forge the
// way the runner does, that is at the host and port of ERMITE_RUNNER_URL, on
// the host. For staging hosts where the forge is not reachable from the
// internet side of the NAT (see the README). Off by default.
const EnvGuestForge = "ERMITE_RUNNER_GUEST_FORGE"

// GuestForge is the one forge endpoint that sandboxes may reach on the host.
// It is derived from the server URL by ParseGuestForge.
type GuestForge struct {
	// Host is the lower-case DNS name of the forge, as in the URL. The DNS
	// forwarder answers it with the gateway address of each sandbox.
	Host string
	// Port is the TCP port: the only host port a sandbox may reach.
	Port uint16
	// Origin is scheme://host[:port] of the URL: the jobs' server URLs are
	// rewritten to it.
	Origin string
}

// Warning is the line logged at start and for each job.
func (g *GuestForge) Warning() string {
	return "guest forge access: sandboxes may reach " + g.Origin + " on the host"
}

// DNSName is the overridden name, "" when the access is off. Safe on nil.
func (g *GuestForge) DNSName() string {
	if g == nil {
		return ""
	}
	return g.Host
}

// TCPPort is the allowed port, 0 when the access is off. Safe on nil.
func (g *GuestForge) TCPPort() uint16 {
	if g == nil {
		return 0
	}
	return g.Port
}

// ParseGuestForge derives the endpoint from the server URL. The URL must have
// a DNS host name (an IP literal cannot be redirected by the DNS forwarder),
// no path, query or credentials, and the port defaults from the scheme (443,
// 80). A plain http URL is accepted only with insecureHTTP (ERMITE_RUNNER_
// INSECURE_HTTP=1): the job tokens then cross the host in clear, and that must
// be asked for explicitly even for localhost.
func ParseGuestForge(raw string, insecureHTTP bool) (*GuestForge, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s: the server URL %q is not a valid URL", EnvGuestForge, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s: the server URL must be a bare origin (scheme://host[:port]): the jobs' URLs are rewritten to the origin alone", EnvGuestForge)
	}
	var def uint16
	switch u.Scheme {
	case "https":
		def = 443
	case "http":
		def = 80
		if !insecureHTTP {
			return nil, fmt.Errorf("%s: the server URL is plain http: set ERMITE_RUNNER_INSECURE_HTTP=1 to accept that sandboxes then send their tokens in clear", EnvGuestForge)
		}
	default:
		return nil, fmt.Errorf("%s: the server URL must be http or https", EnvGuestForge)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if err := CheckForgeHostname(host); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvGuestForge, err)
	}
	port := def
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s: port %q is not in 1-65535", EnvGuestForge, p)
		}
		port = uint16(n)
	}
	origin := u.Scheme + "://" + host
	if u.Port() != "" {
		origin += ":" + u.Port()
	}
	return &GuestForge{Host: host, Port: port, Origin: origin}, nil
}

// CheckForgeHostname accepts one DNS host name: dot separated labels of ASCII
// letters, digits and hyphens (not at either end), at most 63 bytes each and
// 253 in all. IP literals, wildcards, underscores, IDNs and empty labels are
// refused.
func CheckForgeHostname(h string) error {
	if h == "" || len(h) > 253 {
		return errors.New("the host name is empty or longer than 253 bytes")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("%q is an IP address: a host name is required (sandboxes are pointed at the forge by DNS)", h)
	}
	labels := strings.Split(h, ".")
	allNumeric := true
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("%q is not a valid host name", h)
		}
		numeric := true
		for _, r := range l {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
			if !ok {
				return fmt.Errorf("%q is not a valid host name (wildcards, underscores and non-ASCII are refused)", h)
			}
			if r < '0' || r > '9' {
				numeric = false
			}
		}
		allNumeric = allNumeric && numeric
	}
	if allNumeric {
		// "10.1" or "2130706433": legacy spellings of an address.
		return fmt.Errorf("%q looks like an IP address: a host name is required", h)
	}
	return nil
}

// ResolveGuestForge is ParseGuestForge plus a check that the host name
// resolves on the host (lookup is net.DefaultResolver.LookupHost in
// production): the runner reaches the forge by that name, so a name that
// resolves to nothing is a configuration error to report at start.
func ResolveGuestForge(ctx context.Context, raw string, insecureHTTP bool, lookup func(ctx context.Context, host string) ([]string, error)) (*GuestForge, error) {
	g, err := ParseGuestForge(raw, insecureHTTP)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := lookup(ctx, g.Host)
	if err != nil {
		return nil, fmt.Errorf("%s: %s does not resolve on this host: %w", EnvGuestForge, g.Host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s: %s resolves to no address on this host", EnvGuestForge, g.Host)
	}
	return g, nil
}
