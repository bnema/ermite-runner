// SPDX-License-Identifier: MIT

// Package dnsfwd is a small DNS forwarder for job sandboxes (stdlib only). It
// listens on the gateway address of each sandbox (UDP and TCP, port 53 in
// production) and relays queries unchanged to the host's resolvers; the network
// policy lets guests reach nothing else on the host. It does not parse queries
// beyond what framing needs, so it supports every record type and EDNS.
//
// Abuse limits, per listener (one listener per guest, so one guest cannot
// starve another): at most MaxInflight concurrent upstream exchanges (UDP
// queries beyond that are dropped, never queued), at most MaxTCPConns open TCP
// connections, an absolute lifetime of TCPLifetime per TCP connection (a client
// cannot hold a slot by trickling bytes), a deadline on every upstream
// exchange, and a cap on queries per TCP connection.
//
// One name can be overridden (Forwarder.Override, the opt-in guest forge
// access): queries for it are answered locally with the address of the
// listener, see override.go. Every other name is forwarded.
package dnsfwd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"
)

const (
	// TCPLifetime is the absolute lifetime of a TCP connection, set at accept.
	TCPLifetime = 30 * time.Second
	// UpstreamTimeout is the deadline of one upstream TCP attempt.
	UpstreamTimeout = 3 * time.Second
	// UDPAttemptTimeout is the deadline of one upstream UDP attempt. It is short
	// on purpose: glibc guests use "timeout:2 attempts:2", so the reply (or its
	// TC=1 / TCP fallback) must reach them well inside the first 2 s window.
	UDPAttemptTimeout = time.Second
	// MaxExchange caps one whole exchange over all upstreams and transports, so
	// a long upstream list cannot hold an in-flight slot for long.
	MaxExchange = 6 * time.Second
	// TCPIdleTimeout closes TCP client connections that stay silent.
	TCPIdleTimeout = 10 * time.Second

	// MaxInflight bounds concurrent upstream exchanges per listener.
	MaxInflight = 32
	// MaxTCPConns bounds open TCP connections per listener.
	MaxTCPConns = 16
	// TCPMaxQueries caps queries per TCP connection.
	TCPMaxQueries = 64

	maxUDP = 4096 // largest datagram relayed (EDNS0 sizes up to 4096)
	hdrLen = 12

	// plainUDPSize is the largest UDP reply for a client without EDNS (RFC 1035).
	plainUDPSize = 512
)

// Forwarder relays DNS queries. The zero value is not usable; use New.
type Forwarder struct {
	upstreams func() []string
	port      int
	// Lifetime overrides TCPLifetime; set before Listen (tests).
	Lifetime time.Duration
	// Upstream deadlines, set to the constants above by New; tests shorten
	// them before Listen.
	udpTimeout, tcpTimeout, maxExchange time.Duration
	// dial opens upstream connections; tests replace it with in-memory fakes.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// FreeBind binds with IP_FREEBIND, so that the gateway address may be
	// configured after the listener. Off by default: with Docker the address
	// exists when the network is created and a missing one is an error to
	// report at once, not a listener on an address nobody owns. Set before Listen.
	FreeBind bool
	// Override, when set, is a host name that every listener answers itself:
	// A with the address the listener is bound to, and NOERROR with no data for
	// every other type, whatever the host's resolvers say. Case-insensitive; a
	// trailing dot is ignored. Set before Listen.
	Override string

	mu        sync.Mutex
	listeners map[netip.Addr]*listener
}

type listener struct {
	addr   netip.Addr
	udp    net.PacketConn
	tcp    net.Listener
	cancel context.CancelFunc
	done   sync.WaitGroup
	// inflight and conns are the per-listener limits (counting semaphores).
	inflight chan struct{}
	conns    chan struct{}
	// Dropped counts UDP queries dropped because the listener was saturated.
	dropped atomic.Int64
}

// New returns a Forwarder. upstreams returns the "host:port" resolvers to try
// in order (see Resolvers.Addrs); port is the port to listen on (53, or another
// port in tests).
func New(upstreams func() []string, port int) *Forwarder {
	return &Forwarder{
		upstreams: upstreams,
		port:      port,
		Lifetime:  TCPLifetime,

		udpTimeout:  UDPAttemptTimeout,
		tcpTimeout:  UpstreamTimeout,
		maxExchange: MaxExchange,
		dial:        (&net.Dialer{}).DialContext,
		listeners:   map[netip.Addr]*listener{},
	}
}

// Listen starts serving on addr (UDP and TCP). It returns once both sockets are
// bound. Listening twice on the same address is an error.
func (f *Forwarder) Listen(ctx context.Context, addr netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.listeners[addr]; ok {
		return fmt.Errorf("dnsfwd: already listening on %s", addr)
	}
	hp := net.JoinHostPort(addr.String(), strconv.Itoa(f.port))
	var lc net.ListenConfig
	if f.FreeBind {
		// The gateway address may not exist yet (podman's netavark configures the
		// bridge address when the first container starts, after this listener).
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1) }); err != nil {
				return err
			}
			return serr
		}
	}
	var (
		pc  net.PacketConn
		tl  net.Listener
		err error
	)
	// With port 0 (tests) UDP and TCP must share the port the kernel picked for
	// UDP, which a TCP socket may already hold: pick again a few times.
	for attempt := 0; ; attempt++ {
		bind := hp
		if pc, err = lc.ListenPacket(ctx, "udp", bind); err != nil {
			return fmt.Errorf("dnsfwd: listen udp %s: %w", bind, err)
		}
		if f.port == 0 {
			bind = pc.LocalAddr().String()
		}
		if tl, err = lc.Listen(ctx, "tcp", bind); err == nil {
			break
		}
		_ = pc.Close()
		if f.port != 0 || attempt == 9 || !errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("dnsfwd: listen tcp %s: %w", bind, err)
		}
	}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &listener{addr: addr, udp: pc, tcp: tl, cancel: cancel, inflight: make(chan struct{}, MaxInflight), conns: make(chan struct{}, MaxTCPConns)}
	f.listeners[addr] = l
	log := zerowrap.FromCtx(ctx)
	l.done.Add(2)
	go func() { defer l.done.Done(); f.serveUDP(lctx, l, log) }()
	go func() { defer l.done.Done(); f.serveTCP(lctx, l, log) }()
	return nil
}

// Addr returns the bound UDP address for addr, useful when port is 0.
func (f *Forwarder) Addr(addr netip.Addr) (net.Addr, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.listeners[addr]
	if !ok {
		return nil, false
	}
	return l.udp.LocalAddr(), true
}

// Close stops listening on addr and waits for its goroutines. Unknown
// addresses are ignored.
func (f *Forwarder) Close(addr netip.Addr) {
	f.mu.Lock()
	l, ok := f.listeners[addr]
	delete(f.listeners, addr)
	f.mu.Unlock()
	if !ok {
		return
	}
	l.cancel()
	_ = l.udp.Close()
	_ = l.tcp.Close()
	l.done.Wait()
}

// CloseAll stops every listener.
func (f *Forwarder) CloseAll() {
	f.mu.Lock()
	addrs := make([]netip.Addr, 0, len(f.listeners))
	for a := range f.listeners {
		addrs = append(addrs, a)
	}
	f.mu.Unlock()
	for _, a := range addrs {
		f.Close(a)
	}
}

// Dropped returns how many UDP queries the listener on addr dropped because it
// was saturated.
func (f *Forwarder) Dropped(addr netip.Addr) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.listeners[addr]; ok {
		return l.dropped.Load()
	}
	return 0
}

func (f *Forwarder) serveUDP(ctx context.Context, l *listener, log zerowrap.Logger) {
	pc := l.udp
	buf := make([]byte, maxUDP)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				log.Warn().Err(err).Msg("dns forwarder: udp read")
			}
			return
		}
		if n < hdrLen {
			continue
		}
		select {
		case l.inflight <- struct{}{}:
		default:
			// Saturated: drop. The client retries; blocking here would let one
			// guest's flood stall the read loop.
			l.dropped.Add(1)
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			defer func() { <-l.inflight }()
			resp := f.exchange(ctx, l, q, false)
			// A reply larger than the client accepts would be dropped or cut
			// by its stack: send TC=1 so that it retries over TCP at once.
			if len(resp) > clientUDPSize(q) {
				resp = truncated(resp)
			}
			if len(resp) > 0 {
				_, _ = pc.WriteTo(resp, from)
			}
		}()
	}
}

func (f *Forwarder) serveTCP(ctx context.Context, l *listener, log zerowrap.Logger) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.tcp.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				log.Warn().Err(err).Msg("dns forwarder: tcp accept")
			}
			return
		}
		select {
		case l.conns <- struct{}{}:
		default:
			_ = c.Close() // too many open connections on this listener
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-l.conns }()
			defer func() { _ = c.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = c.Close() })
			defer stop()
			// Absolute deadline from accept: neither idle nor slow-loris
			// connections keep their slot past TCPLifetime.
			deadline := time.Now().Add(f.Lifetime)
			_ = c.SetDeadline(deadline)
			f.serveConn(ctx, l, c, deadline)
		}()
	}
}

func (f *Forwarder) serveConn(ctx context.Context, l *listener, c net.Conn, deadline time.Time) {
	for range TCPMaxQueries {
		// The idle timeout can only shorten the absolute deadline.
		dl := time.Now().Add(TCPIdleTimeout)
		if deadline.Before(dl) {
			dl = deadline
		}
		_ = c.SetReadDeadline(dl)
		var lb [2]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lb[:]))
		if n < hdrLen {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		select {
		case l.inflight <- struct{}{}:
		default:
			return // saturated: close, the client retries
		}
		resp := f.exchange(ctx, l, q, true)
		<-l.inflight
		if len(resp) == 0 || len(resp) > 0xffff {
			return
		}
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		copy(out[2:], resp)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// exchange relays q to the upstreams in order and returns the first answer,
// SERVFAIL when none answers, or nil when ctx is cancelled. A TCP client is
// served over TCP. A UDP client is served over UDP for at most udpTimeout; if
// that fails, times out or returns TC=1, the same upstream is retried over TCP
// (at most tcpTimeout) before the next upstream is tried, because some
// resolvers or network paths lose large plain-UDP answers. Each upstream gets
// its own budget, and the whole exchange is capped at maxExchange. The
// overridden name, if any, is answered without asking the upstreams.
func (f *Forwarder) exchange(ctx context.Context, l *listener, q []byte, overTCP bool) []byte {
	if resp := f.overrideAnswer(l.addr, q); resp != nil {
		return resp
	}
	cctx, cancel := context.WithTimeout(ctx, f.maxExchange)
	defer cancel()
	valid := func(resp []byte, err error) bool {
		return err == nil && len(resp) >= hdrLen && resp[0] == q[0] && resp[1] == q[1]
	}
	for _, up := range f.upstreams() {
		if cctx.Err() != nil {
			break
		}
		if overTCP {
			if resp, err := f.exchangeTCP(cctx, up, q, f.tcpTimeout); valid(resp, err) {
				return resp
			}
			continue
		}
		resp, err := f.exchangeUDP(cctx, up, q, f.udpTimeout)
		var tc []byte
		if valid(resp, err) {
			if resp[2]&0x02 == 0 {
				return resp
			}
			tc = resp
		}
		if full, err := f.exchangeTCP(cctx, up, q, f.tcpTimeout); valid(full, err) {
			return full
		}
		if tc != nil {
			return tc // the client retries over TCP itself
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return servfail(q)
}

// clientUDPSize returns the largest UDP reply the client of q accepts: 512
// without EDNS, else its advertised OPT size (at least 512, at most maxUDP).
func clientUDPSize(q []byte) int {
	size := plainUDPSize
	if len(q) < hdrLen {
		return size
	}
	off := hdrLen
	for i := 0; i < int(binary.BigEndian.Uint16(q[4:])); i++ { // questions
		if off = skipName(q, off); off < 0 || off+4 > len(q) {
			return size
		}
		off += 4
	}
	rrs := int(binary.BigEndian.Uint16(q[6:])) + int(binary.BigEndian.Uint16(q[8:])) + int(binary.BigEndian.Uint16(q[10:]))
	for i := 0; i < rrs; i++ {
		if off = skipName(q, off); off < 0 || off+10 > len(q) {
			return size
		}
		if binary.BigEndian.Uint16(q[off:]) == 41 { // OPT: class is the UDP size
			size = max(size, int(binary.BigEndian.Uint16(q[off+2:])))
			break
		}
		off += 10 + int(binary.BigEndian.Uint16(q[off+8:]))
	}
	return min(size, maxUDP)
}

// skipName returns the offset after the name at off, or -1 if malformed.
func skipName(m []byte, off int) int {
	for off < len(m) {
		switch c := int(m[off]); {
		case c == 0:
			return off + 1
		case c&0xc0 == 0xc0:
			if off+2 > len(m) {
				return -1
			}
			return off + 2
		case c&0xc0 != 0:
			return -1
		default:
			off += 1 + c
		}
	}
	return -1
}

// truncated returns the header and question of resp with TC=1 and no records.
func truncated(resp []byte) []byte {
	if len(resp) < hdrLen {
		return nil
	}
	end := hdrLen
	for i := 0; i < int(binary.BigEndian.Uint16(resp[4:])); i++ {
		if end = skipName(resp, end); end < 0 || end+4 > len(resp) {
			end = hdrLen
			break
		}
		end += 4
	}
	r := append([]byte(nil), resp[:end]...)
	r[2] |= 0x02 // TC
	if end == hdrLen {
		r[4], r[5] = 0, 0
	}
	for i := 6; i < hdrLen; i++ { // ANCOUNT, NSCOUNT, ARCOUNT
		r[i] = 0
	}
	return r
}

func (f *Forwarder) exchangeUDP(ctx context.Context, up string, q []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := f.dial(ctx, "udp", up)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	dl, _ := ctx.Deadline()
	_ = c.SetDeadline(dl)
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, maxUDP)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		// Ignore datagrams that do not answer this query (ID mismatch).
		if n >= hdrLen && buf[0] == q[0] && buf[1] == q[1] {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

func (f *Forwarder) exchangeTCP(ctx context.Context, up string, q []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := f.dial(ctx, "tcp", up)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	dl, _ := ctx.Deadline()
	_ = c.SetDeadline(dl)
	out := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(out, uint16(len(q)))
	copy(out[2:], q)
	if _, err := c.Write(out); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// servfail builds a SERVFAIL reply for q: same ID and opcode, QR and RA set,
// RD copied, no records.
func servfail(q []byte) []byte {
	if len(q) < hdrLen {
		return nil
	}
	r := make([]byte, hdrLen)
	r[0], r[1] = q[0], q[1]
	r[2] = 0x80 | (q[2] & 0x79) // QR, opcode, RD kept; AA/TC cleared
	r[3] = 0x80 | 0x02          // RA, RCODE=SERVFAIL
	return r
}
