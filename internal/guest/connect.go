// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// Target is where the executor connects back to: "unix:/path" (container) or
// "vsock:PORT" (vm; the host is CID 2).
type Target struct {
	Network string // "unix" or "vsock"
	Addr    string // socket path, or the vsock port
}

// ParseTarget parses "unix:/path" or "vsock:port".
func ParseTarget(s string) (Target, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return Target{}, fmt.Errorf("connect target %q: want unix:PATH or vsock:PORT", s)
	}
	switch kind {
	case "unix":
		return Target{Network: "unix", Addr: rest}, nil
	case "vsock":
		if p, err := strconv.ParseUint(rest, 10, 32); err != nil || p == 0 {
			return Target{}, fmt.Errorf("connect target %q: bad vsock port", s)
		}
		return Target{Network: "vsock", Addr: rest}, nil
	}
	return Target{}, fmt.Errorf("connect target %q: unknown kind %q", s, kind)
}

func (t Target) String() string { return t.Network + ":" + t.Addr }

// CacheTarget is where the cache streams of the sandbox connect: the host
// channel of the executor (t) has a sibling for the cache, on the same
// sandbox-private path. A VM uses its own vsock port (sandbox.CacheVsockPort);
// a container uses sandbox.CacheSocket next to the executor socket.
func (t Target) CacheTarget() Target {
	if t.Network == "vsock" {
		return Target{Network: "vsock", Addr: strconv.Itoa(sandbox.CacheVsockPort)}
	}
	return Target{Network: "unix", Addr: filepath.Join(filepath.Dir(t.Addr), sandbox.CacheSocket)}
}

// DialStream opens one new connection to the target, without retrying: the
// cache relay opens one per accepted TCP connection and a failure closes that
// connection (the client retries).
func (t Target) DialStream(ctx context.Context) (io.ReadWriteCloser, error) {
	if t.Network == "unix" {
		var d net.Dialer
		return d.DialContext(ctx, "unix", t.Addr)
	}
	c, err := dialVsock(t.Addr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Dial connects to the host, retrying until ctx ends or timeout elapses: the
// host listens before the sandbox starts, but a vsock device may take a moment.
func (t Target) Dial(ctx context.Context, timeout time.Duration) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		var c io.ReadWriteCloser
		var err error
		if t.Network == "unix" {
			var d net.Dialer
			c, err = d.DialContext(ctx, "unix", t.Addr)
		} else {
			var vc *fdConn
			if vc, err = dialVsock(t.Addr); err == nil {
				c = vc
			}
		}
		if err == nil {
			return c, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to %s: %w (last error: %v)", t, ctx.Err(), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// vsockHostCID is the well-known CID of the host.
const vsockHostCID = 2

func dialVsock(port string) (*fdConn, error) {
	p, _ := strconv.ParseUint(port, 10, 32)
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	// A blocking connect is fine: a Firecracker vsock either connects to the
	// host listener at once or fails.
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: vsockHostCID, Port: uint32(p)}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return newFDConn(fd)
}
