// SPDX-License-Identifier: MIT

package cachesvc

import (
	"net"
	"sync"
	"time"
)

// bandwidth is the aggregate rate limit of one channel: a token bucket over
// time, shared by every connection of the sandbox and by both directions, and
// applied to the bytes read from the guest (request bodies) and written to it
// (replies, archives).
// The cache stream of a micro-VM does not cross its network interface, so the
// interface's rate limit does not see it; this one stands in for it.
//
// A transfer is slowed, never cut: each Read or Write moves at most chunk bytes
// and then sleeps for what those bytes cost, so a throttled transfer keeps
// making progress in small steps (the progress deadlines of the handlers and the
// relay's idle cut of the guest see activity). Sleeping ends when the connection
// is closed.
type bandwidth struct {
	rate  float64 // bytes per second
	chunk int     // largest Read or Write step, in bytes

	mu   sync.Mutex
	free time.Time // when the bucket is empty: the cost of the bytes taken so far
}

const maxChunk = 32 << 10

// newBandwidth returns a limiter of bytesPerSec, or nil (no limit) when it is
// not positive.
func newBandwidth(bytesPerSec int64) *bandwidth {
	if bytesPerSec <= 0 {
		return nil
	}
	chunk := int(min(max(bytesPerSec/10, 512), maxChunk))
	// No burst: idle time earns no credit, so a steady transfer stays at the
	// rate and an idle channel does not bank one.
	return &bandwidth{rate: float64(bytesPerSec), chunk: chunk}
}

// mbitToBytes converts the Mbit/s of the sandbox configuration (same unit and
// meaning as the VM's eth0 limit) to bytes per second; 0 or less is no limit.
func mbitToBytes(mbit int) int64 {
	if mbit <= 0 {
		return 0
	}
	return int64(mbit) * 1_000_000 / 8
}

// reserve takes n bytes of the bucket and returns how long to wait before
// moving them.
func (b *bandwidth) reserve(n int) time.Duration {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.free.Before(now) {
		b.free = now
	}
	b.free = b.free.Add(time.Duration(float64(n) / b.rate * float64(time.Second)))
	return b.free.Sub(now)
}

// wait pays for n bytes: it returns when they are due, or with net.ErrClosed
// as soon as done is closed.
func (b *bandwidth) wait(done <-chan struct{}, n int) error {
	d := b.reserve(n)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-done:
		return net.ErrClosed
	}
}

// throttleListener hands out connections that pay into b.
type throttleListener struct {
	net.Listener
	b *bandwidth
}

func (l throttleListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &throttledConn{Conn: c, b: l.b, done: make(chan struct{})}, nil
}

// throttledConn moves its bytes in steps of at most b.chunk and pays for each
// step once it has moved, after the Read or Write itself: the deadlines of the
// handlers, set before a call, then cover the transfer and not the wait, and a
// step costs at most chunk/rate (about 0.1 s) per connection ahead in line. It
// does not implement ReadFrom/WriteTo, so nothing bypasses the steps (no
// sendfile).
type throttledConn struct {
	net.Conn
	b    *bandwidth
	once sync.Once
	done chan struct{}
}

func (c *throttledConn) Read(p []byte) (int, error) {
	if len(p) > c.b.chunk {
		p = p[:c.b.chunk]
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		if werr := c.b.wait(c.done, n); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

func (c *throttledConn) Write(p []byte) (int, error) {
	var sent int
	for len(p) > 0 {
		step := p
		if len(step) > c.b.chunk {
			step = step[:c.b.chunk]
		}
		n, err := c.Conn.Write(step)
		sent += n
		if err != nil {
			return sent, err
		}
		if err := c.b.wait(c.done, n); err != nil {
			return sent, err
		}
		p = p[len(step):]
	}
	return sent, nil
}

func (c *throttledConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// ServeOption configures a channel (Serve).
type ServeOption func(*serveConfig)

type serveConfig struct {
	bytesPerSec int64
}

// WithBandwidth limits the channel to mbit Mbit/s, summed over all its
// connections and both directions. Zero or less means no limit, as for the VM
// interface.
//
// The cache is the one stream of a micro-VM that does not cross its network
// interface, so this is its share of the sandbox's NetMbit.
func WithBandwidth(mbit int) ServeOption {
	return func(c *serveConfig) { c.bytesPerSec = mbitToBytes(mbit) }
}

// withRate is WithBandwidth in bytes per second (tests use small rates).
func withRate(bytesPerSec int64) ServeOption {
	return func(c *serveConfig) { c.bytesPerSec = bytesPerSec }
}
