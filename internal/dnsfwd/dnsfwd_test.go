// SPDX-License-Identifier: MIT

package dnsfwd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// query builds a minimal query for name with the given ID.
func query(id uint16, name string) []byte {
	b := make([]byte, hdrLen)
	binary.BigEndian.PutUint16(b, id)
	b[2] = 0x01 // RD
	b[5] = 1    // QDCOUNT
	for _, l := range strings.Split(name, ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0, 0, 1, 0, 1) // root, A, IN
}

// answer turns a query into a response carrying a recognisable payload.
func answer(q []byte) []byte {
	r := append([]byte(nil), q...)
	r[2] |= 0x80
	return append(r, "ANSWER"...)
}

// fakeUpstream serves UDP and TCP on one port of 127.0.0.1 and returns it.
func fakeUpstream(t *testing.T, handle func(q []byte) []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback:", err)
	}
	addr := pc.LocalAddr().String()
	tl, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close(); _ = tl.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := handle(append([]byte(nil), buf[:n]...)); r != nil {
				_, _ = pc.WriteTo(r, from)
			}
		}
	}()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				r := handle(q)
				out := make([]byte, 2+len(r))
				binary.BigEndian.PutUint16(out, uint16(len(r)))
				copy(out[2:], r)
				_, _ = c.Write(out)
			}()
		}
	}()
	return addr
}

func start(t *testing.T, ups ...string) (*Forwarder, string) {
	t.Helper()
	f := New(func() []string { return ups }, 0)
	ip := netip.MustParseAddr("127.0.0.1")
	if err := f.Listen(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.CloseAll)
	a, _ := f.Addr(ip)
	return f, a.String()
}

func TestUDPAndTCPForwarding(t *testing.T) {
	up := fakeUpstream(t, answer)
	_, addr := start(t, up)

	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	q := query(0x1234, "example.test")
	_, _ = c.Write(q)
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[n-6:n]) != "ANSWER" || binary.BigEndian.Uint16(buf) != 0x1234 {
		t.Fatalf("udp answer: %x", buf[:n])
	}

	tc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	// Two queries on one connection.
	for _, id := range []uint16{7, 8} {
		q := query(id, "tcp.test")
		framed := append([]byte{byte(len(q) >> 8), byte(len(q))}, q...)
		_, _ = tc.Write(framed)
		var l [2]byte
		if _, err := io.ReadFull(tc, l[:]); err != nil {
			t.Fatal(err)
		}
		r := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(tc, r); err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(r) != id || string(r[len(r)-6:]) != "ANSWER" {
			t.Fatalf("tcp answer for %d: %x", id, r)
		}
	}
}

func TestFailoverToSecondUpstream(t *testing.T) {
	dead := "127.0.0.1:1" // nothing listens: UDP read fails with ICMP refused
	live := fakeUpstream(t, answer)
	_, addr := start(t, dead, live)
	c, _ := net.Dial("udp", addr)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	_, _ = c.Write(query(1, "a.test"))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || string(buf[n-6:n]) != "ANSWER" {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestServfailWhenNoUpstreamAnswers(t *testing.T) {
	_, addr := start(t) // no upstream at all
	c, _ := net.Dial("udp", addr)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(query(0xbeef, "a.test"))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != hdrLen || binary.BigEndian.Uint16(buf) != 0xbeef || buf[3]&0x0f != 2 || buf[2]&0x80 == 0 {
		t.Fatalf("not a SERVFAIL: %x", buf[:n])
	}
}

func TestMismatchedUpstreamIDIgnored(t *testing.T) {
	// Upstream answers with a wrong ID: treated as no answer, client gets SERVFAIL.
	up := fakeUpstream(t, func(q []byte) []byte {
		r := answer(q)
		r[0] ^= 0xff
		return r
	})
	f, addr := start(t, up)
	_ = f
	c, _ := net.Dial("udp", addr)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2*UpstreamTimeout + 2*time.Second))
	_, _ = c.Write(query(5, "a.test"))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if buf[3]&0x0f != 2 {
		t.Fatalf("want SERVFAIL, got %x", buf[:n])
	}
}

func TestShortPacketsIgnored(t *testing.T) {
	up := fakeUpstream(t, answer)
	_, addr := start(t, up)
	c, _ := net.Dial("udp", addr)
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte{1, 2, 3}) // dropped
	_, _ = c.Write(query(9, "a.test"))
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || binary.BigEndian.Uint16(buf) != 9 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestListenTwiceAndClose(t *testing.T) {
	f := New(func() []string { return nil }, 0)
	ip := netip.MustParseAddr("127.0.0.1")
	if err := f.Listen(context.Background(), ip); err != nil {
		t.Skip(err)
	}
	if err := f.Listen(context.Background(), ip); err == nil {
		t.Fatal("second Listen on the same address must fail")
	}
	a, _ := f.Addr(ip)
	f.Close(ip)
	f.Close(ip) // idempotent
	if _, ok := f.Addr(ip); ok {
		t.Fatal("still listening after Close")
	}
	if pc, err := net.ListenPacket("udp", a.String()); err != nil {
		t.Fatalf("port not released: %v", err)
	} else {
		_ = pc.Close()
	}
}

func TestServfailShape(t *testing.T) {
	if servfail([]byte{1}) != nil {
		t.Fatal("short query must give nil")
	}
	q := query(0xabcd, "x.test")
	q[2] = 0x79 // opcode bits + RD + AA
	r := servfail(q)
	if r[0] != 0xab || r[1] != 0xcd || r[2]&0x80 == 0 || r[3] != 0x82 {
		t.Fatalf("bad servfail %x", r)
	}
}

func TestParseResolvConf(t *testing.T) {
	in := `# comment
nameserver 127.0.0.53
nameserver 127.0.0.53
nameserver 192.0.2.1 # trailing
nameserver 2001:db8::1
nameserver fe80::1%eth0
nameserver 0.0.0.0
nameserver not-an-ip
search example.test
options edns0
nameserver
`
	got := ParseResolvConf(strings.NewReader(in))
	want := []string{"127.0.0.53:53", "192.0.2.1:53", "[2001:db8::1]:53", "[fe80::1%eth0]:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestResolversReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(p, []byte("nameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewResolvers(p)
	if got := r.Addrs(); !reflect.DeepEqual(got, []string{"192.0.2.1:53"}) {
		t.Fatal(got)
	}
	if err := os.WriteFile(p, []byte("nameserver 192.0.2.99\nnameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(p, later, later)
	if got := r.Addrs(); !reflect.DeepEqual(got, []string{"192.0.2.99:53", "192.0.2.1:53"}) {
		t.Fatal(got)
	}
	// Unreadable file keeps the last known list.
	_ = os.Remove(p)
	if got := r.Addrs(); len(got) != 2 {
		t.Fatalf("lost the last known list: %v", got)
	}
	if got := NewResolvers(filepath.Join(t.TempDir(), "missing")).Addrs(); len(got) != 0 {
		t.Fatal(got)
	}
}

// S2: a listener holds at most MaxTCPConns connections; extra ones are closed
// at once, and a free slot is usable again after a close.
func TestTCPConnectionCapPerListener(t *testing.T) {
	up := fakeUpstream(t, answer)
	_, addr := start(t, up)
	var held []net.Conn
	for range MaxTCPConns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		defer func() { _ = c.Close() }()
	}
	// Let the accept loop register all of them.
	time.Sleep(200 * time.Millisecond)
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = extra.Close() }()
	_ = extra.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the connection over the cap must be closed by the server, got %v", err)
	}
	// Free one slot: a new connection is served.
	_ = held[0].Close()
	time.Sleep(200 * time.Millisecond)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	q := query(3, "slot.test")
	_, _ = c.Write(append([]byte{byte(len(q) >> 8), byte(len(q))}, q...))
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		t.Fatalf("a freed slot must serve again: %v", err)
	}
}

// S2: the lifetime is absolute from accept: a client that keeps the connection
// alive by trickling bytes still loses it.
func TestTCPAbsoluteDeadline(t *testing.T) {
	up := fakeUpstream(t, answer)
	f := New(func() []string { return []string{up} }, 0)
	f.Lifetime = 600 * time.Millisecond
	ip := netip.MustParseAddr("127.0.0.1")
	if err := f.Listen(context.Background(), ip); err != nil {
		t.Skip(err)
	}
	t.Cleanup(f.CloseAll)
	la, _ := f.Addr(ip)
	addr := la.String()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c) // returns when the server closes
		close(closed)
	}()
	// Trickle one byte every 100 ms, never finishing a frame: only an idle
	// timeout (10 s) would let this live; the absolute deadline must not.
	go func() {
		for i := 0; i < 40; i++ {
			if _, err := c.Write([]byte{0}); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	select {
	case <-closed:
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("closed after %v, want about %v", d, f.Lifetime)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a trickling client kept its connection past the absolute deadline")
	}
}

// S2/S9: a saturated listener drops UDP queries instead of blocking its read
// loop, and another listener (another guest) keeps answering.
func TestUDPSaturationDropsAndIsolatesListeners(t *testing.T) {
	release := make(chan struct{})
	slow := fakeUpstream(t, func(q []byte) []byte { <-release; return answer(q) })
	fast := fakeUpstream(t, answer)
	var useSlow atomic.Bool
	useSlow.Store(true)
	f := New(func() []string {
		if useSlow.Load() {
			return []string{slow}
		}
		return []string{fast}
	}, 0)
	a1, a2 := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")
	if err := f.Listen(context.Background(), a1); err != nil {
		t.Skip(err)
	}
	if err := f.Listen(context.Background(), a2); err != nil {
		t.Skip(err)
	}
	t.Cleanup(f.CloseAll)
	t.Cleanup(func() { close(release) })
	l1, _ := f.Addr(a1)
	flood, err := net.Dial("udp", l1.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flood.Close() }()
	for i := 0; i < MaxInflight*3; i++ {
		_, _ = flood.Write(query(uint16(i), "flood.test"))
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.Dropped(a1) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if f.Dropped(a1) == 0 {
		t.Fatal("a saturated listener must drop queries")
	}
	// The other listener is unaffected: point the resolver at the fast upstream.
	useSlow.Store(false)
	l2, _ := f.Addr(a2)
	c, err := net.Dial("udp", l2.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	_, _ = c.Write(query(77, "other-guest.test"))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || string(buf[n-6:n]) != "ANSWER" {
		t.Fatalf("another listener must not be starved by the flood: n=%d err=%v", n, err)
	}
}
