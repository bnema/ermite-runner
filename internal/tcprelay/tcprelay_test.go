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
