// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// The cache service of the runner never listens on a network. In the sandbox it
// is reached on the guest's own loopback (CacheListenHost, on the port the
// host gives in JobSpec.cache_port), where a Relay accepts the TCP connections
// of the actions and pipes each of them to a new stream on the host↔guest
// channel (a vsock port for a VM, a Unix socket for a container); the host
// serves HTTP on the other end of that stream.
const (
	// CacheListenHost is the loopback address of the relay.
	CacheListenHost = "127.0.0.1"
	// DefaultRelayMax bounds the connections a Relay pipes at once. The host
	// bounds them too (cachesvc.DefaultConnsPerListener): a connection over
	// either limit is closed at once and the client retries.
	DefaultRelayMax = 32
	// relayDialTimeout bounds one connection to the host: a stream either opens
	// at once or fails.
	relayDialTimeout = 5 * time.Second
	// DefaultHalfCloseGrace is how long the second direction of a connection may
	// stay idle once the first has ended. A half-close is passed on, but whether
	// every channel carries it is not something the relay can rely on (it is
	// untested over Firecracker's vsock proxy): without a limit, a client that
	// closed its connection could hold a slot until the host's idle timeout. A
	// transfer that makes progress is never idle, so only a stalled half is cut.
	DefaultHalfCloseGrace = 30 * time.Second
)

// DialFunc opens one new stream to the host.
type DialFunc func(ctx context.Context) (io.ReadWriteCloser, error)

// Relay accepts TCP connections on a loopback address and pipes each one to a
// new stream from its DialFunc, in both directions, until Close.
type Relay struct {
	ln    net.Listener
	dial  DialFunc
	slot  chan struct{}
	grace time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	active map[io.Closer]struct{}
}

// StartRelay listens on addr ("127.0.0.1:9089") and relays every connection to
// a stream made by dial; at most max connections are open at once (<= 0 means
// DefaultRelayMax). The relay ends with Close or when ctx ends.
func StartRelay(ctx context.Context, addr string, dial DialFunc, max int) (*Relay, error) {
	return newRelay(ctx, addr, dial, max, DefaultHalfCloseGrace)
}

func newRelay(ctx context.Context, addr string, dial DialFunc, max int, grace time.Duration) (*Relay, error) {
	if max <= 0 {
		max = DefaultRelayMax
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cache relay: %w", err)
	}
	rctx, cancel := context.WithCancel(ctx)
	r := &Relay{ln: ln, dial: dial, slot: make(chan struct{}, max), grace: grace, ctx: rctx, cancel: cancel, active: map[io.Closer]struct{}{}}
	r.wg.Add(1)
	go r.accept()
	context.AfterFunc(rctx, func() { _ = r.Close() })
	return r, nil
}

// Addr is the address the relay listens on.
func (r *Relay) Addr() net.Addr { return r.ln.Addr() }

func (r *Relay) accept() {
	defer r.wg.Done()
	for {
		c, err := r.ln.Accept()
		if err != nil {
			if r.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient accept error (descriptors, aborted connection): go on.
			select {
			case <-time.After(10 * time.Millisecond):
				continue
			case <-r.ctx.Done():
				return
			}
		}
		select {
		case r.slot <- struct{}{}:
		default:
			_ = c.Close() // over the cap: never queued
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() { <-r.slot }()
			r.pipe(c)
		}()
	}
}

// track registers a connection to be closed by Close; it reports false (and
// closes c) when the relay is already closing.
func (r *Relay) track(c io.Closer) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		_ = c.Close()
		return false
	}
	r.active[c] = struct{}{}
	return true
}

func (r *Relay) untrack(c io.Closer) {
	r.mu.Lock()
	delete(r.active, c)
	r.mu.Unlock()
	_ = c.Close()
}

func (r *Relay) pipe(client net.Conn) {
	if !r.track(client) {
		return
	}
	defer r.untrack(client)
	dctx, cancel := context.WithTimeout(r.ctx, relayDialTimeout)
	host, err := r.dial(dctx)
	cancel()
	if err != nil {
		return
	}
	if !r.track(host) {
		return
	}
	defer r.untrack(host)
	Pipe(client, host, r.grace)
}

// closeWriter is a stream that can shut down its write side only.
type closeWriter interface{ CloseWrite() error }

// Pipe copies a to b and b to a until both directions end. The end of one
// direction is passed on as a half-close of the other side's write end, so a
// request sent in full and answered afterwards works; a stream that cannot
// half-close is closed once its peer is done. Once one direction has ended, the
// other may stay idle for grace (0: no limit) before both are closed. Pipe
// closes a and b only in that case; the caller closes them otherwise.
func Pipe(a, b io.ReadWriteCloser, grace time.Duration) {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		timer *time.Timer
		done  int
	)
	touch := func() {
		mu.Lock()
		if timer != nil {
			timer.Reset(grace)
		}
		mu.Unlock()
	}
	ended := func() {
		mu.Lock()
		defer mu.Unlock()
		done++
		switch {
		case done == 1 && grace > 0:
			timer = time.AfterFunc(grace, func() { _ = a.Close(); _ = b.Close() })
		case done == 2 && timer != nil:
			timer.Stop()
		}
	}
	cp := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		_, _ = io.Copy(activityWriter{dst, touch}, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		ended()
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// activityWriter calls touch after every write that goes through.
type activityWriter struct {
	io.Writer
	touch func()
}

func (w activityWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.touch()
	}
	return n, err
}

// Close stops listening, closes every open connection and waits for the pipes
// to end. It is idempotent.
func (r *Relay) Close() error {
	r.cancel()
	err := r.ln.Close()
	r.mu.Lock()
	for c := range r.active {
		_ = c.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
