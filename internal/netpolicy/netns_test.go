// SPDX-License-Identifier: MIT

package netpolicy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

// TestRulesetBetweenTwoGuestsAndAForgedSource loads the real ruleset into the
// kernel and sends real packets: two guests (ertap0, ertap1) and an outside
// host hang off veth pairs, each in its own network namespace. It needs
// CAP_NET_ADMIN, which scripts/it-vm-userns.sh provides without root:
//
//	ERMITE_RUNNER_IT_NET=1 unshare --user --map-root-user --net go test -run Netns ./internal/netpolicy
func TestRulesetBetweenTwoGuestsAndAForgedSource(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_NET") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_NET=1 inside a user+network namespace (scripts/it-vm-userns.sh)")
	}
	for _, c := range []string{"ip", "nft", "nsenter", "unshare", "python3"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("%s not found", c)
		}
	}
	sh := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("ip", "link", "add", "ertapprobe", "type", "dummy").CombinedOutput(); err != nil {
		t.Skipf("no CAP_NET_ADMIN: %v: %s", err, out)
	}
	sh("ip", "link", "del", "ertapprobe")
	sh("ip", "link", "set", "lo", "up")

	// Three machines: guest A, guest B, an outside host.
	pid := map[string]int{}
	for _, n := range []string{"a", "b", "ext"} {
		c := exec.Command("unshare", "--net", "sleep", "120")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		pid[n] = c.Process.Pid
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	}
	time.Sleep(300 * time.Millisecond)
	in := func(n string, args ...string) string {
		t.Helper()
		return sh(append([]string{"nsenter", fmt.Sprintf("--net=/proc/%d/ns/net", pid[n])}, args...)...)
	}
	link := func(host, n string) {
		sh("ip", "link", "add", host, "type", "veth", "peer", "name", "v0", "netns", fmt.Sprint(pid[n]))
		in(n, "ip", "link", "set", "lo", "up")
		in(n, "ip", "link", "set", "v0", "up")
		sh("sysctl", "-qw", "net.ipv4.conf."+host+".rp_filter=0")
	}
	link("ertap0", "a")
	link("ertap1", "b")
	link("extif", "ext")
	sh("sysctl", "-qw", "net.ipv4.conf.all.rp_filter=0")
	sh("sysctl", "-qw", "net.ipv4.ip_forward=1")

	gA := guest(t, "ertap0", 0) // 10.200.0.1 / .2
	gB := guest(t, "ertap1", 1) // 10.200.0.5 / .6
	for _, g := range []Guest{gA, gB} {
		sh("ip", "addr", "add", g.Gateway.String()+"/30", "dev", g.Iface)
		sh("ip", "link", "set", g.Iface, "up")
	}
	sh("ip", "addr", "add", "203.0.114.1/24", "dev", "extif")
	sh("ip", "link", "set", "extif", "up")
	in("a", "ip", "addr", "add", gA.Addr.String()+"/30", "dev", "v0")
	in("a", "ip", "route", "add", "default", "via", gA.Gateway.String())
	in("b", "ip", "addr", "add", gB.Addr.String()+"/30", "dev", "v0")
	in("b", "ip", "route", "add", "default", "via", gB.Gateway.String())
	in("ext", "ip", "addr", "add", "203.0.114.2/24", "dev", "v0")
	in("ext", "ip", "route", "add", "default", "via", "203.0.114.1")
	// Guest A holds guest B's address too, to be able to forge it.
	in("a", "ip", "addr", "add", gB.Addr.String()+"/32", "dev", "lo")

	m := NewManager(cmdrun.Exec{}, "nft")
	ctx := context.Background()
	t.Cleanup(func() { _ = m.Reset(ctx) })
	for _, g := range []Guest{gA, gB} {
		if err := m.Add(ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	// An outside UDP server: logs each payload with the source it saw, and
	// answers "pong".
	logf := t.TempDir() + "/ext.log"
	const server = `
import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",9999))
f=open(sys.argv[1],"a",buffering=1)
while True:
    d,a=s.recvfrom(100); f.write(d.decode()+" from "+a[0]+"\n"); s.sendto(b"pong",a)
`
	srv := exec.Command("nsenter", fmt.Sprintf("--net=/proc/%d/ns/net", pid["ext"]), "python3", "-c", server, logf)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	time.Sleep(500 * time.Millisecond)

	// udp sends one datagram from machine n (optionally bound to src:sport) and
	// reports whether an answer came back.
	const client = `
import socket,sys
dst,payload,src,sport=sys.argv[1],sys.argv[2],sys.argv[3],int(sys.argv[4])
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(2)
if src: s.bind((src,sport))
s.sendto(payload.encode(),(dst,int(sys.argv[5]) if len(sys.argv)>5 else 9999))
try: s.recv(10); print("answer")
except Exception: print("silence")
`
	udp := func(n, dst, payload, src string, sport int, dport int) string {
		return in(n, "python3", "-c", client, dst, payload, src, fmt.Sprint(sport), fmt.Sprint(dport))
	}
	seen := func() string { b, _ := os.ReadFile(logf); return string(b) }

	// Control: a guest reaches the outside host and gets the reply (NAT and the
	// established path work, so the drops below are the policy, not a broken rig).
	if got := udp("a", "203.0.114.2", "ctl-a", "", 0, 9999); got != "answer" {
		t.Fatalf("control: guest A cannot reach the outside host: %s", got)
	}

	// Guests cannot reach each other, directly or through the other's gateway.
	if got := udp("a", gB.Addr.String(), "a-to-b", "", 0, 9999); got != "silence" {
		t.Errorf("guest A reached guest B: %s", got)
	}
	// Forged reply (S1): B opens a flow to the outside host from port 5555 and
	// keeps listening. Guest A then forges a reply of that flow: source = the
	// outside host's address and port, destination = the host's masquerade
	// address and B's NAT port. Conntrack would map it onto B's flow and deliver
	// it to B as "established" traffic if an established-accept came before the
	// source check.
	in("a", "ip", "addr", "add", "203.0.114.2/32", "dev", "lo")
	bLog := t.TempDir() + "/b.log"
	const listener = `
import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(6)
s.bind(("10.200.0.6",5555)); s.sendto(b"open-flow",("203.0.114.2",9999))
f=open(sys.argv[1],"a",buffering=1)
try:
    while True:
        d,a=s.recvfrom(100); f.write(d.decode()+" from "+a[0]+"\n")
except Exception: pass
`
	bProc := exec.Command("nsenter", fmt.Sprintf("--net=/proc/%d/ns/net", pid["b"]), "python3", "-c", listener, bLog)
	if err := bProc.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if got := udp("a", "203.0.114.1", "forged-reply-to-b", "203.0.114.2", 9999, 5555); got != "silence" {
		t.Errorf("unexpected answer to the forgery: %s", got)
	}
	_ = bProc.Wait()
	if b, _ := os.ReadFile(bLog); strings.Contains(string(b), "forged-reply-to-b") {
		t.Errorf("guest B received a reply forged by guest A through conntrack:\n%s", b)
	}
	// A forged source address on a plain forwarded packet is dropped as well.
	if got := udp("a", "203.0.114.2", "forged-src", gB.Addr.String(), 7777, 9999); got != "silence" {
		t.Errorf("a packet with another guest's source address got through: %s", got)
	}
	// S6: guest A cannot use guest B's DNS gateway, nor the host's other ports.
	if got := udp("a", gB.Gateway.String(), "dns-a-to-b-gw", "", 0, 53); got != "silence" {
		t.Errorf("guest A reached guest B's gateway on 53: %s", got)
	}
	// The outside host cannot open connections to a guest.
	if got := in("ext", "python3", "-c", client, gA.Addr.String(), "ext-to-a", "", "0", "9999"); got != "silence" {
		t.Errorf("the outside host reached guest A: %s", got)
	}
	// Special-use ranges (S10) are dropped even though the rig could route them.
	in("ext", "ip", "addr", "add", "198.51.100.7/32", "dev", "lo")
	sh("ip", "route", "add", "198.51.100.7/32", "via", "203.0.114.2")
	if got := udp("a", "198.51.100.7", "doc-range", "", 0, 9999); got != "silence" {
		t.Errorf("TEST-NET-2 reached: %s", got)
	}

	out := seen()
	for _, bad := range []string{"forged-src", "a-to-b", "ext-to-a", "doc-range"} {
		if strings.Contains(out, bad) {
			t.Errorf("outside host saw %q:\n%s", bad, out)
		}
	}
	for _, good := range []string{"ctl-a", "open-flow"} {
		if !strings.Contains(out, good) {
			t.Errorf("outside host did not see %q:\n%s", good, out)
		}
	}
}
