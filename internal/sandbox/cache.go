// SPDX-License-Identifier: MIT

package sandbox

import (
	"net"

	"bnema.dev/ermite-runner/internal/cachesvc"
)

// ServeCache serves the runner's cache service (Config.Cache, when set) on a
// Unix socket at path, the host end of the cache channel of sandbox id. The
// guest relay opens one connection to it per TCP connection it accepts on its
// loopback: through the sandbox's own bind mount (container) or through
// Firecracker's vsock proxy (VM, which connects to <uds>_<port> when the guest
// connects to the host on that port). Nothing but that sandbox can reach the
// path, and the service accepts on it only tokens made for job (see
// cachesvc.Service.Serve). own, when not nil, sets the owner and mode of the
// socket before it is served. mbit caps the channel's traffic, all its
// connections and both directions together (0: unlimited); a VM passes its
// NetMbit, as the cache stream does not cross eth0, which carries that limit.
// With no cache service this does nothing.
func (c Config) ServeCache(id, path, job string, mbit int, own func(path string) error) error {
	if c.Cache == nil {
		return nil
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if own != nil {
		if err := own(path); err != nil {
			_ = ln.Close()
			return err
		}
	}
	return c.Cache.Serve(id, ln, job, cachesvc.WithBandwidth(mbit))
}

// StopCache ends the cache channel of sandbox id: it closes the listener (which
// removes the socket) and the open connections. It is idempotent.
func (c Config) StopCache(id string) {
	if c.Cache != nil {
		c.Cache.Stop(id)
	}
}
