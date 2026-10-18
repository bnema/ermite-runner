// SPDX-License-Identifier: MIT

// Package tcprelay forwards TCP connections accepted on a sandbox's gateway
// address to one fixed target. It carries the guest forge access when the
// forge does not listen on the runner's own network namespace: a runner in a
// container reaches the forge by name on its container network, and the
// sandboxes, which only reach their gateway, are relayed there.
package tcprelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
)

// MaxConns bounds the connections relayed at once for one listener: a guest
// cannot exhaust the runner's file descriptors.
const MaxConns = 64

// dialTimeout bounds the connection to the target.
const dialTimeout = 10 * time.Second

// Relay forwards every connection accepted on its listeners to Target.
type Relay struct {
	// Target is host:port, resolved again for every connection.
	Target string
	// Dial connects to Target; nil means a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	mu        sync.Mutex
	listeners map[netip.Addr]*listener
}

type listener struct {
	ln     net.Listener
	cancel context.CancelFunc
	done   sync.WaitGroup
	slots  chan struct{}
}

// New returns a relay to target (host:port).
func New(target string) *Relay {
	return &Relay{Target: target, listeners: map[netip.Addr]*listener{}}
}

// Listen starts relaying connections to addr:port. It returns once the socket
// is bound. Listening twice on the same address is an error.
func (r *Relay) Listen(ctx context.Context, addr netip.Addr, port uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listeners == nil {
		r.listeners = map[netip.Addr]*listener{}
	}
	if _, ok := r.listeners[addr]; ok {
		return fmt.Errorf("tcprelay: already listening on %s", addr)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(int(port))))
	if err != nil {
		return fmt.Errorf("tcprelay: listen %s:%d: %w", addr, port, err)
	}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &listener{ln: ln, cancel: cancel, slots: make(chan struct{}, MaxConns)}
	r.listeners[addr] = l
	l.done.Add(1)
	go func() {
		defer l.done.Done()
		r.serve(lctx, l)
	}()
	return nil
}

// Addr returns the bound address for addr, useful when port is 0.
func (r *Relay) Addr(addr netip.Addr) (net.Addr, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.listeners[addr]
	if !ok {
		return nil, false
	}
	return l.ln.Addr(), true
}

// Close stops the listener of addr and its relayed connections. It is a no-op
// when there is none.
func (r *Relay) Close(addr netip.Addr) {
	r.mu.Lock()
	l, ok := r.listeners[addr]
	delete(r.listeners, addr)
	r.mu.Unlock()
	if ok {
		l.stop()
	}
}

// CloseAll stops every listener.
func (r *Relay) CloseAll() {
	r.mu.Lock()
	ls := r.listeners
	r.listeners = map[netip.Addr]*listener{}
	r.mu.Unlock()
	for _, l := range ls {
		l.stop()
	}
}

func (l *listener) stop() {
	l.cancel()
	_ = l.ln.Close()
	l.done.Wait()
}

func (r *Relay) serve(ctx context.Context, l *listener) {
	log := zerowrap.FromCtx(ctx)
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Warn().Err(err).Msg("tcprelay: accept")
			continue
		}
		select {
		case l.slots <- struct{}{}:
		default:
			log.Warn().Str("target", r.Target).Msg("tcprelay: too many relayed connections, refusing one")
			_ = c.Close()
			continue
		}
		l.done.Add(1)
		go func() {
			defer l.done.Done()
			defer func() { <-l.slots }()
			r.relay(ctx, c)
		}()
	}
}

func (r *Relay) relay(ctx context.Context, in net.Conn) {
	defer func() { _ = in.Close() }()
	dial := r.Dial
	if dial == nil {
		d := net.Dialer{}
		dial = d.DialContext
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	out, err := dial(dctx, "tcp", r.Target)
	cancel()
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Str("target", r.Target).Msg("tcprelay: dial")
		return
	}
	defer func() { _ = out.Close() }()
	// Closing both ends when the listener stops ends the copies.
	stop := context.AfterFunc(ctx, func() { _ = in.Close(); _ = out.Close() })
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pipe(out, in) }()
	go func() { defer wg.Done(); pipe(in, out) }()
	wg.Wait()
}

// pipe copies src to dst, then half-closes dst so the other side sees the end
// of the stream while the reverse direction may still be sending.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.Close()
	}
}
