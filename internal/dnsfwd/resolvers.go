// SPDX-License-Identifier: MIT

package dnsfwd

import (
	"bufio"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Resolvers reads the upstream nameservers from a resolv.conf-format file and
// re-reads it when it changes. Entries such as the systemd-resolved stub
// (127.0.0.53) are kept on purpose: the forwarder runs on the host, where the
// stub is reachable, and that stub is the only resolver that knows the host's
// split-DNS configuration.
type Resolvers struct {
	path string

	mu    sync.Mutex
	mtime time.Time
	size  int64
	addrs []string
}

// NewResolvers returns a Resolvers reading path (default /etc/resolv.conf).
func NewResolvers(path string) *Resolvers {
	if path == "" {
		path = "/etc/resolv.conf"
	}
	return &Resolvers{path: path}
}

// Addrs returns the upstream "host:53" addresses, in file order. When the file
// cannot be read the last known list is returned.
func (r *Resolvers) Addrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := os.Stat(r.path)
	if err != nil {
		return r.addrs
	}
	if st.ModTime().Equal(r.mtime) && st.Size() == r.size && r.addrs != nil {
		return r.addrs
	}
	f, err := os.Open(r.path)
	if err != nil {
		return r.addrs
	}
	defer func() { _ = f.Close() }()
	r.addrs = ParseResolvConf(f)
	r.mtime, r.size = st.ModTime(), st.Size()
	return r.addrs
}

// ParseResolvConf extracts "nameserver" entries as "ip:53" strings. Comments,
// unknown directives and invalid addresses are ignored; duplicates removed.
// IPv6 addresses with a zone (fe80::1%eth0) are kept with their zone.
func ParseResolvConf(f interface{ Read([]byte) (int, error) }) []string {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(fields[1])
		if err != nil || a.IsUnspecified() {
			continue
		}
		s := net.JoinHostPort(a.String(), "53")
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
