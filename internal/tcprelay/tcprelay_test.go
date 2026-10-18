// SPDX-License-Identifier: MIT

package tcprelay

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

var lo = netip.MustParseAddr("127.0.0.1")

// echo starts a line echo server and returns its address.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind loopback:", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func dialRelay(t *testing.T, r *Relay) net.Conn {
	t.Helper()
	a, ok := r.Addr(lo)
	if !ok {
		t.Fatal("relay not listening")
	}
	c, err := net.DialTimeout("tcp", a.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRelayForwardsBothWays(t *testing.T) {
	r := New(echo(t))
	if err := r.Listen(context.Background(), lo, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.CloseAll)
	c := dialRelay(t, r)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("got %q, %v", line, err)
	}
}

func TestRelayListenTwiceFails(t *testing.T) {
	r := New(echo(t))
	if err := r.Listen(context.Background(), lo, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.CloseAll)
	if err := r.Listen(context.Background(), lo, 0); err == nil {
		t.Fatal("a second listener on the same address was accepted")
	}
}

func TestRelayCloseStopsListenerAndConnections(t *testing.T) {
	r := New(echo(t))
	if err := r.Listen(context.Background(), lo, 0); err != nil {
		t.Fatal(err)
	}
	a, _ := r.Addr(lo)
	c := dialRelay(t, r)
	if _, err := c.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(c).ReadString('\n')
	r.Close(lo)
	if _, ok := r.Addr(lo); ok {
		t.Fatal("still listening after Close")
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("relayed connection still open after Close")
	}
	if c2, err := net.DialTimeout("tcp", a.String(), time.Second); err == nil {
		_ = c2.Close()
		t.Fatal("new connections still accepted after Close")
	}
	r.Close(lo) // idempotent
}

// By default Listen fails on an address the host does not own; FreeBind
// (podman: the bridge address comes when the first container starts) binds it.
func TestRelayFreeBind(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.77") // TEST-NET-3: never configured here
	r := New("127.0.0.1:1")
	t.Cleanup(r.CloseAll)
	if err := r.Listen(context.Background(), ip, 0); err == nil {
		t.Fatal("listening on an address the host does not own must fail without FreeBind")
	}
	r.FreeBind = true
	if err := r.Listen(context.Background(), ip, 0); err != nil {
		t.Fatalf("FreeBind: %v", err)
	}
	if _, ok := r.Addr(ip); !ok {
		t.Fatal("no listener")
	}
}

// At most MaxConns connections are relayed at once; the next one is closed,
// and a slot freed by a closed connection is reused.
func TestRelayConnectionLimit(t *testing.T) {
	r := New(echo(t))
	if err := r.Listen(context.Background(), lo, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.CloseAll)
	roundTrip := func(c net.Conn) error {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte("x\n")); err != nil {
			return err
		}
		_, err := bufio.NewReader(c).ReadString('\n')
		return err
	}
	conns := make([]net.Conn, MaxConns)
	for i := range conns {
		conns[i] = dialRelay(t, r)
		if err := roundTrip(conns[i]); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
	}
	extra := dialRelay(t, r)
	if err := roundTrip(extra); err == nil {
		t.Fatal("a connection over MaxConns was relayed")
	}
	_ = conns[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dialRelay(t, r)
		if roundTrip(c) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the freed slot was not reused")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelayUnreachableTargetClosesTheGuestSide(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens there now
	r := New(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err := r.Listen(context.Background(), lo, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.CloseAll)
	c := dialRelay(t, r)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the connection to be closed")
	}
}
