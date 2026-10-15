// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// hostSide is a Unix socket server standing for the host end of the channel.
type hostSide struct {
	ln      net.Listener
	accepts atomic.Int32
}

func newHostSide(t *testing.T, serve func(net.Conn)) *hostSide {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "h"))
	if err != nil {
		t.Fatal(err)
	}
	h := &hostSide{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.accepts.Add(1)
			go serve(c)
		}
	}()
	return h
}

func (h *hostSide) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", h.ln.Addr().String())
}

func startRelay(t *testing.T, dial DialFunc, max int) *Relay {
	t.Helper()
	r, err := StartRelay(context.Background(), "127.0.0.1:0", dial, max)
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func dialRelay(t *testing.T, r *Relay) *net.TCPConn {
	t.Helper()
	c, err := net.Dial("tcp", r.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*net.TCPConn)
}

func readAllTimeout(t *testing.T, c net.Conn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// Bytes go both ways, and the end of the client's stream reaches the host as an
// EOF while the host can still answer (half-close).
func TestRelayCopiesBothWaysAndHalfCloses(t *testing.T) {
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		req, _ := io.ReadAll(c) // returns only at the client's half-close
		_, _ = c.Write(append([]byte("echo:"), req...))
	})
	r := startRelay(t, h.dial, 4)
	c := dialRelay(t, r)
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := string(readAllTimeout(t, c)); got != "echo:hello" {
		t.Fatalf("got %q", got)
	}
}

// The host closing its side reaches the client as an EOF, and bulk data in both
// directions is not lost.
func TestRelayBulkAndHostClose(t *testing.T) {
	const n = 3 << 20
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		got, _ := io.CopyN(io.Discard, c, n)
		_, _ = io.CopyN(c, zeroReader{}, got)
	})
	r := startRelay(t, h.dial, 4)
	c := dialRelay(t, r)
	go func() { _, _ = io.CopyN(c, zeroReader{}, n) }()
	if got := len(readAllTimeout(t, c)); got != n {
		t.Fatalf("read %d bytes, want %d", got, n)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Every accepted connection is a new stream to the host.
func TestRelayOneStreamPerConnection(t *testing.T) {
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(c, c)
	})
	r := startRelay(t, h.dial, 8)
	for i := 0; i < 3; i++ {
		c := dialRelay(t, r)
		_, _ = c.Write([]byte("x"))
		_ = c.CloseWrite()
		if got := string(readAllTimeout(t, c)); got != "x" {
			t.Fatalf("got %q", got)
		}
	}
	if n := h.accepts.Load(); n != 3 {
		t.Fatalf("%d host streams for 3 connections", n)
	}
}

// The cap is on open connections: one over it is closed at once, never queued,
// and a slot freed by a closing connection is usable again.
func TestRelayConcurrencyCap(t *testing.T) {
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(c, c)
	})
	r := startRelay(t, h.dial, 2)
	a, b := dialRelay(t, r), dialRelay(t, r)
	roundTrip := func(c net.Conn) {
		t.Helper()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte("p")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
	}
	roundTrip(a)
	roundTrip(b)
	over := dialRelay(t, r)
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := over.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		// A reset is as good as an EOF: the connection was refused service.
		var ne net.Error
		if !errors.As(err, &ne) || ne.Timeout() {
			t.Fatalf("a connection over the cap must be closed, got %v", err)
		}
	}
	if n := h.accepts.Load(); n != 2 {
		t.Fatalf("%d host streams, want 2", n)
	}
	_ = a.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dialRelay(t, r)
		_ = c.SetDeadline(time.Now().Add(time.Second))
		_, _ = c.Write([]byte("p"))
		if _, err := io.ReadFull(c, make([]byte, 1)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot of a closed connection is never freed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A channel that does not carry the half-close (Firecracker's vsock proxy may
// not) must not hold a slot for ever once the client has gone: the idle half is
// cut after the grace, while a transfer that makes progress is not.
func TestRelayCutsAnIdleHalfAfterTheGrace(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		<-release // never answers and never closes its side
	})
	const grace = 150 * time.Millisecond
	r, err := newRelay(context.Background(), "127.0.0.1:0", h.dial, 1, grace)
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	c := dialRelay(t, r)
	_, _ = c.Write([]byte("x"))
	_ = c.CloseWrite() // the client is done: the host half stays idle
	start := time.Now()
	// The host never ends its half, so an EOF here is the relay cutting it.
	if b := readAllTimeout(t, c); len(b) != 0 {
		t.Fatalf("read %q", b)
	}
	if took := time.Since(start); took < grace {
		t.Fatalf("cut after %v, before the grace of %v", took, grace)
	}
	// The slot is back (the cap is 1): a new connection reaches the host.
	waitFor(t, func() bool {
		d := dialRelay(t, r)
		_, _ = d.Write([]byte("y"))
		return h.accepts.Load() >= 2
	})
}

// A slow but live transfer after the first direction ended is not cut: the
// grace counts idle time, not total time.
func TestRelayGraceIsIdleTimeNotTotalTime(t *testing.T) {
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(io.Discard, c) // the request, until the client's half-close
		for i := 0; i < 8; i++ {      // a response slower than the grace in total
			_, _ = c.Write([]byte("0123456789"))
			time.Sleep(60 * time.Millisecond)
		}
	})
	r, err := newRelay(context.Background(), "127.0.0.1:0", h.dial, 2, 200*time.Millisecond)
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	c := dialRelay(t, r)
	_, _ = c.Write([]byte("req"))
	_ = c.CloseWrite()
	if got := len(readAllTimeout(t, c)); got != 80 {
		t.Fatalf("read %d bytes, want 80", got)
	}
}

// A host that cannot be reached closes the client's connection.
func TestRelayDialFailureClosesClient(t *testing.T) {
	var calls atomic.Int32
	r := startRelay(t, func(context.Context) (io.ReadWriteCloser, error) {
		calls.Add(1)
		return nil, errors.New("no host")
	}, 2)
	c := dialRelay(t, r)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || calls.Load() != 1 {
		t.Fatalf("read err %v after %d dials", err, calls.Load())
	}
	// The failed connection holds no slot.
	for i := 0; i < 4; i++ {
		d := dialRelay(t, r)
		_ = d.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := d.Read(make([]byte, 1)); err == nil {
			t.Fatal("expected a close")
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("%d dials, a leaked slot would have refused some", calls.Load())
	}
}

// Close stops the listener, cuts the open connections on both sides and returns
// once every goroutine has ended; it can be called twice.
func TestRelayCloseCutsEverything(t *testing.T) {
	hostGone := make(chan struct{}, 1)
	h := newHostSide(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(io.Discard, c) // ends when the relay closes its stream
		hostGone <- struct{}{}
	})
	r, err := StartRelay(context.Background(), "127.0.0.1:0", h.dial, 4)
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	c := dialRelay(t, r)
	_, _ = c.Write([]byte("x"))
	waitFor(t, func() bool { return h.accepts.Load() == 1 })

	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close does not return")
	}
	select {
	case <-hostGone:
	case <-time.After(5 * time.Second):
		t.Fatal("the host stream was not closed")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the client connection is still open")
	}
	if c2, err := net.DialTimeout("tcp", r.Addr().String(), time.Second); err == nil {
		_ = c2.Close()
		t.Fatal("the relay still listens")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The relay ends with the context of the job.
func TestRelayEndsWithItsContext(t *testing.T) {
	h := newHostSide(t, func(c net.Conn) { _ = c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	r, err := StartRelay(ctx, "127.0.0.1:0", h.dial, 2)
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	addr := r.Addr().String()
	cancel()
	waitFor(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
}

// A port that is taken is an error, not a silent no-cache.
func TestRelayListenError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback sockets are not allowed here: %v", err)
	}
	defer func() { _ = busy.Close() }()
	if _, err := StartRelay(context.Background(), busy.Addr().String(), nil, 1); err == nil {
		t.Fatal("expected an error")
	}
}

// The cache channel is a sibling of the executor's: its own vsock port in a VM,
// a second socket in the same private directory in a container.
func TestCacheTarget(t *testing.T) {
	for in, want := range map[string]string{
		"vsock:1024":                 "vsock:9089",
		"unix:/run/ermite/exec.sock": "unix:/run/ermite/cache.sock",
	} {
		tg, err := ParseTarget(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := tg.CacheTarget().String(); got != want {
			t.Errorf("%s: cache target %s, want %s", in, got, want)
		}
	}
}

// DialStream opens a new connection at every call and does not retry.
func TestDialStreamUnix(t *testing.T) {
	h := newHostSide(t, func(c net.Conn) { _ = c.Close() })
	tg := Target{Network: "unix", Addr: h.ln.Addr().String()}
	for i := 0; i < 2; i++ {
		c, err := tg.DialStream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	waitFor(t, func() bool { return h.accepts.Load() == 2 })
	start := time.Now()
	if _, err := (Target{Network: "unix", Addr: filepath.Join(t.TempDir(), "none")}).DialStream(context.Background()); err == nil || time.Since(start) > time.Second {
		t.Fatalf("want an immediate error, got %v after %s", err, time.Since(start))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached in 5s")
}
