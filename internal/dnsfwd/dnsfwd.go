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
	"time"

	"github.com/bnema/zerowrap"
)

const (
	// TCPLifetime is the absolute lifetime of a TCP connection, set at accept.
	TCPLifetime = 30 * time.Second
	// UpstreamTimeout is the deadline of one upstream attempt.
	UpstreamTimeout = 3 * time.Second
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
)

// Forwarder relays DNS queries. The zero value is not usable; use New.
type Forwarder struct {
	upstreams func() []string
	port      int
	// Lifetime overrides TCPLifetime; set before Listen (tests).
	Lifetime time.Duration

	mu        sync.Mutex
	listeners map[netip.Addr]*listener
}

type listener struct {
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
		listeners: map[netip.Addr]*listener{},
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
	pc, err := lc.ListenPacket(ctx, "udp", hp)
	if err != nil {
		return fmt.Errorf("dnsfwd: listen udp %s: %w", hp, err)
	}
	// With port 0 (tests) UDP and TCP must share the port the kernel picked.
	if f.port == 0 {
		hp = pc.LocalAddr().String()
	}
	tl, err := lc.Listen(ctx, "tcp", hp)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("dnsfwd: listen tcp %s: %w", hp, err)
	}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &listener{udp: pc, tcp: tl, cancel: cancel, inflight: make(chan struct{}, MaxInflight), conns: make(chan struct{}, MaxTCPConns)}
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
			resp := f.exchange(ctx, q, false)
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
		resp := f.exchange(ctx, q, true)
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

// exchange relays q to the upstreams in order and returns the first answer, or
// a SERVFAIL when none answers. The transport towards the upstream matches the
// client's: a client that fell back to TCP gets a complete answer.
func (f *Forwarder) exchange(ctx context.Context, q []byte, overTCP bool) []byte {
	for _, up := range f.upstreams() {
		var resp []byte
		var err error
		if overTCP {
			resp, err = exchangeTCP(ctx, up, q)
		} else {
			resp, err = exchangeUDP(ctx, up, q)
		}
		if err == nil && len(resp) >= hdrLen && resp[0] == q[0] && resp[1] == q[1] {
			return resp
		}
	}
	return servfail(q)
}

func exchangeUDP(ctx context.Context, up string, q []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, UpstreamTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", up)
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

func exchangeTCP(ctx context.Context, up string, q []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, UpstreamTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", up)
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
