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
	"testing/synctest"
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

// listenUDPAndTCP binds UDP on a free loopback port and TCP on the same port.
// The TCP port may already be taken (outbound connections use the same
// ephemeral range), so it picks again a few times, like Listen does.
func listenUDPAndTCP(t *testing.T) (net.PacketConn, net.Listener) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Skip("no loopback:", err)
		}
		tl, err := net.Listen("tcp", pc.LocalAddr().String())
		if err == nil {
			return pc, tl
		}
		_ = pc.Close()
		if attempt == 49 || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatal(err)
		}
	}
}

// fakeUpstream serves UDP and TCP on one port of 127.0.0.1 and returns it.
func fakeUpstream(t *testing.T, handle func(q []byte) []byte) string {
	t.Helper()
	pc, tl := listenUDPAndTCP(t)
	addr := pc.LocalAddr().String()
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
	return startWith(t, nil, ups...)
}

// startWith is start with a hook to adjust the Forwarder before Listen.
func startWith(t *testing.T, cfg func(*Forwarder), ups ...string) (*Forwarder, string) {
	t.Helper()
	f := New(func() []string { return ups }, 0)
	if cfg != nil {
		cfg(f)
	}
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

// By default Listen fails fast on an address the host does not own (Docker and
// the VM driver have configured it by then: a missing one is an error).
func TestListenOnAMissingAddressFailsByDefault(t *testing.T) {
	f := New(func() []string { return nil }, 0)
	t.Cleanup(f.CloseAll)
	ip := netip.MustParseAddr("203.0.113.77") // TEST-NET-3: never configured here
	if err := f.Listen(context.Background(), ip); err == nil {
		t.Fatal("listening on an address the host does not own must fail without FreeBind")
	}
}

// With FreeBind (podman's netavark creates the bridge address only when the
// first container starts) Listen binds an address the host does not own yet.
func TestFreeBindListensOnAnAddressTheHostDoesNotOwnYet(t *testing.T) {
	f := New(func() []string { return nil }, 0)
	f.FreeBind = true
	t.Cleanup(f.CloseAll)
	ip := netip.MustParseAddr("203.0.113.77")
	if err := f.Listen(context.Background(), ip); err != nil {
		t.Fatalf("FreeBind: %v", err)
	}
	if _, ok := f.Addr(ip); !ok {
		t.Fatal("no listener")
	}
}

// queryType is query with a chosen type (and optionally EDNS0).
func queryType(id uint16, name string, qtype uint16, edns bool) []byte {
	q := query(id, name)
	binary.BigEndian.PutUint16(q[len(q)-4:], qtype)
	if edns {
		q[11] = 1
		q = append(q, 0, 0, 41, 0x10, 0, 0, 0, 0, 0, 0, 0)
	}
	return q
}

func udpExchange(t *testing.T, addr string, q []byte) []byte {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(q)
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func startOverride(t *testing.T, name string, ups ...string) (*Forwarder, string) {
	t.Helper()
	f := New(func() []string { return ups }, 0)
	f.Override = name
	ip := netip.MustParseAddr("127.0.0.1")
	if err := f.Listen(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.CloseAll)
	a, _ := f.Addr(ip)
	return f, a.String()
}

func TestOverrideAnswersAWithTheListenerAddress(t *testing.T) {
	var asked atomic.Int64
	up := fakeUpstream(t, func(q []byte) []byte { asked.Add(1); return answer(q) })
	// A trailing dot in the setting is ignored; on the wire a name has none.
	_, addr := startOverride(t, "Ermite.Example.org.", up)
	for _, name := range []string{"ermite.example.org", "ERMITE.example.ORG", "Ermite.Example.Org"} {
		r := udpExchange(t, addr, queryType(0x4242, name, 1, false))
		if binary.BigEndian.Uint16(r) != 0x4242 || r[2]&0x80 == 0 || r[2]&0x01 == 0 || r[3]&0x0F != 0 {
			t.Fatalf("%s: header %x", name, r[:hdrLen])
		}
		if an := binary.BigEndian.Uint16(r[6:]); an != 1 {
			t.Fatalf("%s: %d answers", name, an)
		}
		if !strings.HasSuffix(string(r), "\x00\x04\x7f\x00\x00\x01") {
			t.Fatalf("%s: rdata is not 127.0.0.1: %x", name, r)
		}
	}
	if asked.Load() != 0 {
		t.Fatalf("the override went upstream %d times", asked.Load())
	}
}

func TestOverrideAAAAIsEmptyNoError(t *testing.T) {
	up := fakeUpstream(t, answer)
	_, addr := startOverride(t, "ermite.example.org", up)
	for _, edns := range []bool{false, true} {
		r := udpExchange(t, addr, queryType(7, "ermite.EXAMPLE.org", 28, edns))
		if r[3]&0x0F != 0 || binary.BigEndian.Uint16(r[6:]) != 0 || r[2]&0x80 == 0 {
			t.Fatalf("AAAA (edns %v): %x", edns, r)
		}
		if edns != (binary.BigEndian.Uint16(r[10:]) == 1) {
			t.Fatalf("AAAA (edns %v): arcount %d", edns, binary.BigEndian.Uint16(r[10:]))
		}
	}
}

func TestOverrideForwardsEveryOtherName(t *testing.T) {
	var asked atomic.Int64
	up := fakeUpstream(t, func(q []byte) []byte { asked.Add(1); return answer(q) })
	_, addr := startOverride(t, "ermite.example.org", up)
	for _, name := range []string{"example.org", "other.example.org", "x.ermite.example.org", "ermite.example.org.evil.test", "ermite.example.or"} {
		r := udpExchange(t, addr, query(9, name))
		if string(r[len(r)-6:]) != "ANSWER" {
			t.Fatalf("%s was not forwarded: %x", name, r)
		}
	}
	if asked.Load() != 5 {
		t.Fatalf("%d upstream queries, want 5", asked.Load())
	}
}

func TestOverrideOverTCPAndDisabledByDefault(t *testing.T) {
	up := fakeUpstream(t, answer)
	_, addr := startOverride(t, "ermite.example.org", up)
	tc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	q := queryType(3, "ermite.example.org", 1, false)
	_, _ = tc.Write(append([]byte{byte(len(q) >> 8), byte(len(q))}, q...))
	var l [2]byte
	if _, err := io.ReadFull(tc, l[:]); err != nil {
		t.Fatal(err)
	}
	r := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(tc, r); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(r[6:]) != 1 || !strings.HasSuffix(string(r), "\x7f\x00\x00\x01") {
		t.Fatalf("tcp: %x", r)
	}

	// Off by default: the name is forwarded like any other.
	_, addr2 := start(t, up)
	r2 := udpExchange(t, addr2, query(4, "ermite.example.org"))
	if string(r2[len(r2)-6:]) != "ANSWER" {
		t.Fatalf("not forwarded without Override: %x", r2)
	}
}

func TestOverrideIgnoresMalformedAndNonQueries(t *testing.T) {
	f := New(func() []string { return nil }, 0)
	f.Override = "ermite.example.org"
	gw := netip.MustParseAddr("10.200.0.1")
	good := queryType(1, "ermite.example.org", 1, false)
	if f.overrideAnswer(gw, good) == nil {
		t.Fatal("control query not answered")
	}
	cut := good[:len(good)-3]
	chaos := queryType(1, "ermite.example.org", 1, false)
	binary.BigEndian.PutUint16(chaos[len(chaos)-2:], 3) // class CH
	resp := append([]byte(nil), good...)
	resp[2] |= 0x80 // QR set: not a query
	notify := append([]byte(nil), good...)
	notify[2] |= 4 << 3 // opcode 4
	ptr := append(append([]byte(nil), good[:hdrLen]...), 0xC0, 0x0C, 0, 1, 0, 1)
	for name, q := range map[string][]byte{"truncated": cut, "class CH": chaos, "response": resp, "opcode": notify, "pointer": ptr, "short": good[:5]} {
		if f.overrideAnswer(gw, q) != nil {
			t.Errorf("%s was answered locally", name)
		}
	}
}

// bigAnswer returns an answer to q padded to size bytes, with TC set if tc.
func bigAnswer(q []byte, size int, tc bool) []byte {
	r := answer(q)
	if tc {
		r[2] |= 0x02
	}
	for len(r) < size {
		r = append(r, 'x')
	}
	return r
}

func TestLargeAnswerNonEDNSClientGetsTC(t *testing.T) {
	up := fakeUpstream(t, func(q []byte) []byte { return bigAnswer(q, 700, false) })
	_, addr := start(t, up)
	q := queryType(0x3333, "storage.googleapis.com", 1, false)
	resp := udpExchange(t, addr, q)
	if resp[2]&0x02 == 0 {
		t.Fatalf("TC not set: %x", resp)
	}
	if len(resp) != len(q) || binary.BigEndian.Uint16(resp[4:]) != 1 || binary.BigEndian.Uint16(resp[6:]) != 0 {
		t.Fatalf("want header+question only, got %x", resp)
	}
	if resp[0] != q[0] || resp[1] != q[1] {
		t.Fatal("ID changed")
	}
}

func TestLargeAnswerEDNSClientGetsFullAnswer(t *testing.T) {
	up := fakeUpstream(t, func(q []byte) []byte { return bigAnswer(q, 700, false) })
	_, addr := start(t, up)
	// queryType advertises 0x1000 (4096) bytes.
	resp := udpExchange(t, addr, queryType(0x4444, "storage.googleapis.com", 1, true))
	if len(resp) != 700 || resp[2]&0x02 != 0 {
		t.Fatalf("want the full 700-byte answer, got len %d tc=%d", len(resp), resp[2]&0x02)
	}
}

// withOPT appends an OPT record (root name, type 41) with the given class and
// RDLEN to a query and bumps ARCOUNT.
func withOPT(q []byte, size, rdlen uint16, body int) []byte {
	q = append([]byte(nil), q...)
	q[11] = 1
	q = append(q, 0, 0, 41)
	q = binary.BigEndian.AppendUint16(q, size)
	q = append(q, 0, 0, 0, 0)
	q = binary.BigEndian.AppendUint16(q, rdlen)
	return append(q, make([]byte, body)...)
}

func TestClientUDPSize(t *testing.T) {
	plain := query(1, "a.b")
	bigQD := append([]byte(nil), plain[:hdrLen]...)
	bigQD[4], bigQD[5] = 0xff, 0xff
	bigQD = append(bigQD, 1, 'a', 0, 0, 1, 0, 1)
	ptrQ := append([]byte(nil), plain[:hdrLen]...)
	ptrQ = append(ptrQ, 0xc0, hdrLen, 0, 1, 0, 1) // compression pointer in question
	tests := []struct {
		name string
		q    []byte
		want int
	}{
		{"no edns", plain, 512},
		{"edns 1232", withOPT(plain, 1232, 0, 0), 1232},
		{"edns below 512", withOPT(plain, 100, 0, 0), 512},
		{"edns above cap", withOPT(plain, 65535, 0, 0), maxUDP},
		{"edns 4096", withOPT(plain, 4096, 0, 0), 4096},
		{"qdcount 65535 short body", bigQD, 512},
		{"compression pointer in question", ptrQ, 512},
		{"OPT rdlen overruns buffer", withOPT(plain, 4096, 500, 3), 4096},
		{"truncated header", plain[:15], 512},
		{"short", plain[:5], 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientUDPSize(tt.q); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTruncated(t *testing.T) {
	q := queryType(7, "a.b", 1, false)
	full := bigAnswer(q, 700, false)
	full[7] = 5 // ANCOUNT
	got := truncated(full)
	if len(got) != len(q) || got[2]&0x02 == 0 || got[6] != 0 || got[7] != 0 || got[4] != 0 || got[5] != 1 {
		t.Fatalf("bad truncated reply %x", got)
	}
	bad := append([]byte(nil), full...)
	bad[4], bad[5] = 0xff, 0xff // QDCOUNT lies, question parse fails
	got = truncated(bad)
	if len(got) != hdrLen || got[2]&0x02 == 0 || got[4] != 0 || got[5] != 0 {
		t.Fatalf("bad-question case: %x", got)
	}
	if truncated(full[:5]) != nil {
		t.Fatal("short input must give nil")
	}
}

func TestServfailAndOverrideRepliesAreNotTruncated(t *testing.T) {
	dead := "127.0.0.1:1" // nothing listens: UDP and TCP both fail at once
	_, addr := startOverride(t, "forge.test", dead)
	resp := udpExchange(t, addr, queryType(9, "x.test", 1, false))
	if len(resp) != hdrLen || resp[3]&0x0f != 2 || resp[2]&0x02 != 0 {
		t.Fatalf("want a plain 12-byte SERVFAIL, got %x", resp)
	}
	resp = udpExchange(t, addr, queryType(10, "forge.test", 1, false))
	if resp[2]&0x02 != 0 || resp[3]&0x0f != 0 || binary.BigEndian.Uint16(resp[6:]) != 1 {
		t.Fatalf("want the override A answer, got %x", resp)
	}
}

// fakeNet is an in-memory upstream network for Forwarder.dial. One Write on a
// UDP conn is one datagram (net.Pipe delivers a Write to a single Read when the
// reader's buffer is large enough). A handler returning nil never answers.
type fakeNet struct {
	udp, tcp           func(addr string, q []byte) []byte
	udpCalls, tcpCalls atomic.Int64
}

func (n *fakeNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil { // like net.Dialer
		return nil, err
	}
	c, s := net.Pipe()
	switch network {
	case "udp":
		go func() {
			defer func() { _ = s.Close() }()
			buf := make([]byte, 4096)
			for {
				k, err := s.Read(buf)
				if err != nil {
					return
				}
				n.udpCalls.Add(1)
				if r := n.udp(addr, append([]byte(nil), buf[:k]...)); r != nil {
					if _, err := s.Write(r); err != nil {
						return
					}
				}
			}
		}()
	case "tcp":
		go func() {
			defer func() { _ = s.Close() }()
			var l [2]byte
			if _, err := io.ReadFull(s, l[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(l[:]))
			if _, err := io.ReadFull(s, q); err != nil {
				return
			}
			n.tcpCalls.Add(1)
			if r := n.tcp(addr, q); r != nil {
				_, _ = s.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(r))), r...))
			}
			_, _ = io.Copy(io.Discard, s) // wait for the client to hang up
		}()
	default:
		return nil, errors.New("unexpected network " + network)
	}
	return c, nil
}

// silent is a handler that never answers.
func silent(string, []byte) []byte { return nil }

// fakeForwarder returns a Forwarder with production timeouts whose upstreams
// are served by n, and a listener for exchange.
func fakeForwarder(n *fakeNet, ups ...string) (*Forwarder, *listener) {
	f := New(func() []string { return ups }, 0)
	f.dial = n.dial
	return f, &listener{addr: netip.MustParseAddr("127.0.0.1")}
}

func isServfail(r []byte, id uint16) bool {
	return len(r) == hdrLen && binary.BigEndian.Uint16(r) == id && r[3]&0x0f == 2
}

// The tests below run in a synctest bubble: the fake clock makes the real
// production timeouts (1 s UDP, 3 s TCP, 6 s cap) instant and exact.

func TestExchangeSilentUDPFallsBackToTCPAfterUDPTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNet{udp: silent, tcp: func(_ string, q []byte) []byte { return answer(q) }}
		f, l := fakeForwarder(n, "up:53")
		start := time.Now()
		resp := f.exchange(t.Context(), l, query(1, "storage.googleapis.com"), false)
		if !strings.HasSuffix(string(resp), "ANSWER") {
			t.Fatalf("want the TCP answer, got %x", resp)
		}
		if got := time.Since(start); got != UDPAttemptTimeout {
			t.Fatalf("answered after %v, want %v", got, UDPAttemptTimeout)
		}
		if n.udpCalls.Load() != 1 || n.tcpCalls.Load() != 1 {
			t.Fatalf("udp=%d tcp=%d, want 1 and 1", n.udpCalls.Load(), n.tcpCalls.Load())
		}
	})
}

func TestExchangeTruncatedUDPRetriedOverTCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNet{
			udp: func(_ string, q []byte) []byte { return bigAnswer(q, 0, true) },
			tcp: func(_ string, q []byte) []byte { return bigAnswer(q, 700, false) },
		}
		f, l := fakeForwarder(n, "up:53")
		start := time.Now()
		resp := f.exchange(t.Context(), l, query(2, "example.com"), false)
		if len(resp) != 700 || resp[2]&0x02 != 0 {
			t.Fatalf("want the full TCP answer, got len %d tc=%d", len(resp), resp[2]&0x02)
		}
		if time.Since(start) != 0 {
			t.Fatalf("took %v, want no waiting", time.Since(start))
		}
	})
}

func TestExchangeKeepsTruncatedReplyWhenTCPFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNet{udp: func(_ string, q []byte) []byte { return bigAnswer(q, 0, true) }, tcp: silent}
		f, l := fakeForwarder(n, "up:53")
		resp := f.exchange(t.Context(), l, query(3, "example.com"), false)
		if len(resp) < hdrLen || resp[2]&0x02 == 0 {
			t.Fatalf("want the TC=1 reply, got %x", resp)
		}
	})
}

func TestExchangeSecondUpstreamNotStarved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNet{
			udp: func(a string, q []byte) []byte {
				if a == "dead:53" {
					return nil
				}
				return answer(q)
			},
			tcp: func(a string, q []byte) []byte {
				if a == "dead:53" {
					return nil
				}
				return answer(q)
			},
		}
		f, l := fakeForwarder(n, "dead:53", "live:53")
		start := time.Now()
		resp := f.exchange(t.Context(), l, query(4, "a.test"), false)
		if !strings.HasSuffix(string(resp), "ANSWER") {
			t.Fatalf("want the second upstream's answer, got %x", resp)
		}
		if got, want := time.Since(start), UDPAttemptTimeout+UpstreamTimeout; got != want {
			t.Fatalf("took %v, want %v", got, want)
		}
	})
}

func TestExchangeMismatchedUpstreamIDIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wrong := func(_ string, q []byte) []byte {
			r := answer(q)
			r[0] ^= 0xff
			return r
		}
		f, l := fakeForwarder(&fakeNet{udp: wrong, tcp: wrong}, "up:53")
		start := time.Now()
		resp := f.exchange(t.Context(), l, query(5, "a.test"), false)
		if !isServfail(resp, 5) {
			t.Fatalf("want SERVFAIL, got %x", resp)
		}
		// UDP keeps listening for the right ID until its timeout; the TCP
		// reply is read once and rejected.
		if got := time.Since(start); got != UDPAttemptTimeout {
			t.Fatalf("took %v, want %v", got, UDPAttemptTimeout)
		}
	})
}

func TestExchangeCappedAtMaxExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNet{udp: silent, tcp: silent}
		f, l := fakeForwarder(n, "a:53", "b:53", "c:53")
		start := time.Now()
		resp := f.exchange(t.Context(), l, query(6, "a.test"), false)
		if !isServfail(resp, 6) {
			t.Fatalf("want SERVFAIL, got %x", resp)
		}
		if got := time.Since(start); got != MaxExchange {
			t.Fatalf("took %v, want %v", got, MaxExchange)
		}
		// a: 1 s UDP + 3 s TCP; b: 1 s UDP + 1 s TCP (cut by the cap); c: skipped.
		if n.udpCalls.Load() != 2 || n.tcpCalls.Load() != 2 {
			t.Fatalf("udp=%d tcp=%d, want 2 and 2", n.udpCalls.Load(), n.tcpCalls.Load())
		}
	})
}

func TestExchangeTCPClientUsesTCPOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &fakeNet{udp: silent, tcp: func(_ string, q []byte) []byte { return answer(q) }}
		f, l := fakeForwarder(n, "up:53")
		resp := f.exchange(t.Context(), l, query(7, "a.test"), true)
		if !strings.HasSuffix(string(resp), "ANSWER") || n.udpCalls.Load() != 0 {
			t.Fatalf("resp=%x udp=%d", resp, n.udpCalls.Load())
		}
	})
}

func TestExchangeCancelledReturnsNil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, l := fakeForwarder(&fakeNet{udp: silent, tcp: silent}, "up:53")
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(500*time.Millisecond, cancel)
		start := time.Now()
		if resp := f.exchange(ctx, l, query(8, "a.test"), false); resp != nil {
			t.Fatalf("want nil on cancellation, got %x", resp)
		}
		// Cancellation is noticed at the next attempt boundary: the blocked UDP
		// read runs to its own deadline, then no TCP attempt is started.
		if got := time.Since(start); got != UDPAttemptTimeout {
			t.Fatalf("returned after %v, want %v", got, UDPAttemptTimeout)
		}
	})
}

// End to end through real client-facing sockets; the upstream is in memory.
func TestUDPClientGetsTCPAnswerWhenUpstreamUDPSilent(t *testing.T) {
	n := &fakeNet{udp: silent, tcp: func(_ string, q []byte) []byte { return answer(q) }}
	_, addr := startWith(t, func(f *Forwarder) {
		f.dial = n.dial
		f.udpTimeout = 50 * time.Millisecond
	}, "up:53")
	resp := udpExchange(t, addr, query(0x1111, "storage.googleapis.com"))
	if !strings.HasSuffix(string(resp), "ANSWER") || resp[3]&0x0f != 0 {
		t.Fatalf("want the TCP answer, got %x", resp)
	}
}
