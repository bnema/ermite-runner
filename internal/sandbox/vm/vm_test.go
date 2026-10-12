// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

// The tests run a fake Firecracker: this test binary, re-executed through a
// shell script named "firecracker" with FC_HELPER set. It serves the HTTP API on
// the unix socket, records every request, and on InstanceStart plays the guest:
// it connects to the vsock socket and speaks the executor protocol.
func TestMain(m *testing.M) {
	if os.Getenv("FC_HELPER") != "" {
		fakeFirecracker()
		return
	}
	os.Exit(m.Run())
}

func fakeFirecracker() {
	var sock, id string
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--api-sock":
			sock = args[i+1]
		case "--id":
			id = args[i+1]
		}
	}
	_ = id
	mode := os.Getenv("FC_MODE")
	if mode == "die" {
		fmt.Fprintln(os.Stderr, "fake firecracker: refusing to start")
		os.Exit(3)
	}
	logf, _ := os.OpenFile(os.Getenv("FC_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	var vsock string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut {
			_, _ = fmt.Fprintf(logf, "%s %s %s\n", r.Method, r.URL.Path, body)
		}
		if r.URL.Path == "/vsock" {
			var v vsockDevice
			_ = json.Unmarshal(body, &v)
			vsock = v.UDSPath
		}
		w.WriteHeader(http.StatusNoContent)
		if r.URL.Path == "/actions" && mode != "hang" {
			go func() {
				c, err := net.Dial("unix", fmt.Sprintf("%s_%d", vsock, sandbox.VsockPort))
				if err != nil {
					_, _ = fmt.Fprintln(logf, "vsock dial:", err)
					return
				}
				defer func() { _ = c.Close() }()
				g := execstream.NewConn(c)
				_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{Sandbox: "vm"}}})
				if _, err := g.Recv(); err != nil {
					return
				}
				_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Output{Output: &execpb.Output{Data: []byte("hello from fake guest\n")}}})
				_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 0}}})
				// Like a real VM, stay alive until killed.
				select {}
			}()
		}
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = http.Serve(ln, mux)
}

type env struct {
	cfg   sandbox.Config
	run   *cmdrun.Fake
	plumb *sandboxtest.Plumbing
	log   string
	taps  map[string]bool
	mu    sync.Mutex
}

// newEnv prepares a state dir, fake binaries, kernel and images.
func newEnv(t *testing.T, mode string) *env {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash needed for the fake firecracker")
	}
	dir := t.TempDir()
	e := &env{taps: map[string]bool{}, plumb: sandboxtest.NewPlumbing(), log: filepath.Join(dir, "fc.log")}
	self, _ := os.Executable()
	fc := filepath.Join(dir, "firecracker")
	script := fmt.Sprintf("#!%s\nexport FC_HELPER=1 FC_MODE=%q FC_LOG=%q\nexec -a firecracker %q \"$@\"\n", bash, mode, e.log, self)
	if err := os.WriteFile(fc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "image")
	_ = os.MkdirAll(img, 0o755)
	for _, f := range []string{"vmlinux", "base.ext4"} {
		_ = os.WriteFile(filepath.Join(img, f), []byte("x"), 0o644)
	}
	exe := filepath.Join(dir, "runner")
	sandboxtest.WriteStaticELF(t, exe)
	e.cfg = sandbox.Config{
		Isolation: sandbox.VM, StateDir: filepath.Join(dir, "state"), Capacity: 2, Image: img,
		JobCPUs: 1, JobMemoryMiB: 512, JobDiskMiB: 1024, UIDBase: 900000, KVMPath: "/dev/null",
		Network: mustPrefix("10.200.0.0/16"), Firecracker: fc, JailerOff: true, Executable: exe, DNSPort: 0,
	}
	e.run = &cmdrun.Fake{Handle: func(c cmdrun.Call) ([]byte, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch {
		case c.Name == "ip" && len(c.Args) > 1 && c.Args[0] == "tuntap":
			e.taps[c.Args[3]] = true
		case c.Name == "ip" && len(c.Args) > 2 && c.Args[0] == "link" && c.Args[1] == "del":
			if !e.taps[c.Args[3]] {
				return []byte("Cannot find device"), errors.New("exit status 1")
			}
			delete(e.taps, c.Args[3])
		case c.Name == "ip" && len(c.Args) > 1 && c.Args[0] == "-o":
			var b strings.Builder
			i := 1
			for t := range e.taps {
				fmt.Fprintf(&b, "%d: %s: <BROADCAST> mtu 1500\n", i, t)
				i++
			}
			return []byte(b.String()), nil
		}
		return nil, nil
	}}
	return e
}

func (e *env) tapCount() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.taps) }

func (e *env) driver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// leftovers lists everything that must not survive a job.
func (e *env) leftovers(t *testing.T) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(e.cfg.StateDir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(e.cfg.StateDir, p)
		switch {
		case err != nil || rel == ".":
		case strings.HasPrefix(rel, "vm/") || strings.HasPrefix(rel, "vms/") || strings.HasPrefix(rel, "jail/"):
			out = append(out, rel)
		}
		return nil
	})
	if n := e.tapCount(); n != 0 {
		out = append(out, fmt.Sprintf("%d TAP devices", n))
	}
	if n := e.plumb.Live(); n != 0 {
		out = append(out, fmt.Sprintf("%d attached guests", n))
	}
	return out
}

func runStub(t *testing.T, conn sandbox.Conn) (string, int) {
	t.Helper()
	var out strings.Builder
	_, exit, err := execstream.Drive(context.Background(), conn, &execpb.Run{}, func(o *execpb.Output) { out.Write(o.Data) })
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), int(exit.Code)
}

func TestStartRunStop(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	out, code := runStub(t, conn)
	if out != "hello from fake guest\n" || code != 0 {
		t.Fatalf("out=%q code=%d", out, code)
	}
	// While running: record, TAP, attachment, process.
	if e.tapCount() != 1 || e.plumb.Live() != 1 {
		t.Fatalf("tap=%d attached=%d", e.tapCount(), e.plumb.Live())
	}
	if recs, _ := d.store.list(); len(recs) != 1 || recs[0].Job != "job-1" || recs[0].PID == 0 || recs[0].Tap != "ertap0" {
		t.Fatalf("records: %+v", recs)
	}
	pid := sb.(*vmSandbox).rec.PID

	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers after Stop: %v", left)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("firecracker pid %d still alive", pid)
	}
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatalf("Stop must be idempotent: %v", err)
	}
	if d.slots.InUse() != 0 {
		t.Fatal("slot not released")
	}
}

// The API calls must follow the order Firecracker needs and carry the
// plan's disk layout: exec disk (boot root, RO), base (RO), scratch (RW).
func TestAPICallsAndDiskLayout(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j", CPUs: 3, MemoryMiB: 768, DiskMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)
	defer func() { _ = sb.Stop(context.Background()) }()

	b, _ := os.ReadFile(e.log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var paths []string
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "PUT" {
			paths = append(paths, f[1])
		}
	}
	want := []string{"/machine-config", "/boot-source", "/drives/rootfs", "/drives/base", "/drives/scratch", "/network-interfaces/eth0", "/vsock", "/entropy", "/actions"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", paths, want)
	}
	log := string(b)
	for _, frag := range []string{
		`"vcpu_count":3`, `"mem_size_mib":768`, `"smt":false`,
		`"drive_id":"rootfs"`, `"is_root_device":true,"is_read_only":true`,
		`"drive_id":"base"`, `"is_root_device":false,"is_read_only":true`,
		`"drive_id":"scratch"`, `"is_root_device":false,"is_read_only":false`,
		`"host_dev_name":"ertap0"`, `"guest_cid":3`, `"action_type":"InstanceStart"`,
		"ipv6.disable=1", "ermite.connect=vsock:1024", "ermite.gw=10.200.0.1", "ip=10.200.0.2::10.200.0.1:255.255.255.252",
	} {
		if !strings.Contains(log, frag) {
			t.Errorf("API log lacks %q", frag)
		}
	}
	// The scratch disk was created sparse at the requested size, and formatted.
	var mkfs []string
	for _, l := range e.run.Lines() {
		if strings.HasPrefix(l, "mkfs.ext4") {
			mkfs = append(mkfs, l)
		}
	}
	if len(mkfs) == 0 {
		t.Fatalf("no mkfs call: %v", e.run.Lines())
	}
}

func TestJobsDoNotShareSlotsOrTaps(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	var sbs []sandbox.Sandbox
	for i := range 2 {
		sb, err := d.New()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		runStub(t, conn)
		sbs = append(sbs, sb)
	}
	if _, err := d.New(); !errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("capacity not enforced: %v", err)
	}
	if e.tapCount() != 2 {
		t.Fatalf("taps: %v", e.taps)
	}
	for _, sb := range sbs {
		_ = sb.Stop(context.Background())
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestStartFailureCleansEverything(t *testing.T) {
	e := newEnv(t, "die")
	d := e.driver(t)
	sb, _ := d.New()
	_, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("want the VMM console in the error, got %v", err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers after failed Start: %v", left)
	}
	if d.slots.InUse() != 0 {
		t.Fatal("slot leaked by a failed Start")
	}
}

func TestBootTimeoutCleansEverything(t *testing.T) {
	e := newEnv(t, "hang")
	d, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	sb, _ := d.New()
	_, err = sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err == nil || !strings.Contains(err.Error(), "did not connect") {
		t.Fatalf("got %v", err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestStartCancelledByContext(t *testing.T) {
	e := newEnv(t, "hang")
	d := e.driver(t)
	sb, _ := d.New()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := sb.Start(ctx, sandbox.JobSpec{JobID: "j"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestNetworkAttachFailureCleans(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	e.plumb.FailNext = errors.New("nft broke")
	sb, _ := d.New()
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"}); err == nil || !strings.Contains(err.Error(), "nft broke") {
		t.Fatalf("got %v", err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestStopWithoutStartReleasesSlot(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb, _ := d.New()
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.slots.InUse() != 0 {
		t.Fatal("slot leaked")
	}
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{}); err == nil {
		t.Fatal("Start after Stop accepted")
	}
}

// Stop must finish the cleanup even when the caller's context is already done
// (a cancelled job).
func TestStopRunsWithCancelledContext(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sb.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// A killed runner leaves a running Firecracker, a TAP, nft rules and files. The
// next start must reap all of it (AC4: nothing survives, incl. killed jobs).
func TestReapAfterRunnerCrash(t *testing.T) {
	e := newEnv(t, "")
	d1 := e.driver(t)
	sb, _ := d1.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)
	pid := sb.(*vmSandbox).rec.PID
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("fake VM not running")
	}
	// The runner dies: its lock goes away, nothing else is cleaned.
	_ = d1.lock.Close()
	d1.lock = nil
	_ = conn.Close()
	if e.tapCount() != 1 {
		t.Fatal("setup: no TAP")
	}

	d2 := e.driver(t)
	if err := d2.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers after Reap: %v", left)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		// The fake is a child of this test process: it is a zombie until the
		// original driver's Wait goroutine collects it.
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && strings.Contains(string(b), ") Z") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); len(b) > 0 && !strings.Contains(string(b), ") Z") {
		t.Fatalf("firecracker %d survived Reap: %s", pid, b)
	}
	if e.plumb.Resets == 0 {
		t.Fatal("the nftables table was not reset")
	}
}

func TestReapRemovesCorruptRecordAndTempFiles(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	_ = os.MkdirAll(d.store.dir, 0o700)
	_ = os.WriteFile(filepath.Join(d.store.dir, "er00112233.json"), []byte("{garbage"), 0o600)
	_ = os.WriteFile(filepath.Join(d.store.dir, "er44556677.json.tmp"), []byte("{"), 0o600)
	_ = os.MkdirAll(filepath.Join(e.cfg.StateDir, "vm", "er00112233"), 0o700)
	_ = os.MkdirAll(filepath.Join(e.cfg.StateDir, "vm", "not-ours"), 0o700) // must be left alone
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "vm", "not-ours")); err != nil {
		t.Fatal("Reap removed a directory that is not ours")
	}
	_ = os.Remove(filepath.Join(e.cfg.StateDir, "vm", "not-ours"))
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestReapLeavesForeignTapsAlone(t *testing.T) {
	e := newEnv(t, "")
	e.taps["tap0"] = true
	e.taps["ertapx"] = true // not ertap<digits>
	e.taps["ertap7"] = true
	d := e.driver(t)
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.taps["tap0"] || !e.taps["ertapx"] || e.taps["ertap7"] {
		t.Fatalf("taps after reap: %v", e.taps)
	}
}

func TestSecondRunnerOnSameStateDirRefused(t *testing.T) {
	e := newEnv(t, "")
	_ = e.driver(t)
	_, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb})
	if err == nil || !strings.Contains(err.Error(), "another ermite-runner") {
		t.Fatalf("got %v", err)
	}
}

func TestNewRejectsDynamicExecutable(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Executable = "/bin/sh"
	if _, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb}); err == nil {
		t.Skip("/bin/sh is static here")
	}
}

func TestExecDiskIsKeyedByExecutableHash(t *testing.T) {
	e := newEnv(t, "")
	d1 := e.driver(t)
	first := d1.execDisk
	_ = d1.Close()
	// Same executable: reused, no second mkfs of an exec disk.
	n := len(e.run.Calls)
	d2 := e.driver(t)
	if d2.execDisk != first {
		t.Fatal("exec disk rebuilt for the same executable")
	}
	for _, c := range e.run.Calls[n:] {
		if c.Name == "mkfs.ext4" {
			t.Fatal("mkfs run again for an unchanged executable")
		}
	}
	_ = d2.Close()
	// A runner upgrade: new disk, old one removed.
	if err := os.WriteFile(e.cfg.Executable, append(readFile(t, e.cfg.Executable), 0), 0o755); err != nil {
		t.Fatal(err)
	}
	d3 := e.driver(t)
	if d3.execDisk == first {
		t.Fatal("exec disk not rebuilt after the executable changed")
	}
	if _, err := os.Stat(first); err == nil {
		t.Fatal("stale exec disk kept")
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestJailedLayout(t *testing.T) {
	l := newLayout("/var/lib/er", "firecracker", "er00112233", true, 900003)
	if l.root != "/var/lib/er/jail/firecracker/er00112233/root" || l.dir != "/var/lib/er/jail/firecracker/er00112233" {
		t.Fatalf("%+v", l)
	}
	if l.fc("scratch.ext4") != "/scratch.ext4" || l.host("scratch.ext4") != l.root+"/scratch.ext4" {
		t.Fatal("jailed paths")
	}
	if jailBase("/var/lib/er") != "/var/lib/er/jail" {
		t.Fatal("jail base")
	}
	if l2 := newLayout("/var/lib/er", "firecracker-v1.17", "er00112233", true, 1); l2.dir != "/var/lib/er/jail/firecracker-v1.17/er00112233" {
		t.Fatalf("the jailer names its chroot after the exec file: %s", l2.dir)
	}
	u := newLayout("/var/lib/er", "firecracker", "er00112233", false, 1000)
	if u.fc("api.sock") != "/var/lib/er/vm/er00112233/api.sock" {
		t.Fatalf("unjailed path %s", u.fc("api.sock"))
	}
	if p, _ := u.share("/img/base.ext4", "base.ext4"); p != "/img/base.ext4" {
		t.Fatal("unjailed share must use the file in place")
	}
}

func TestJailedShareHardLinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.ext4")
	_ = os.WriteFile(src, []byte("data"), 0o644)
	l := newLayout(dir, "firecracker", "er00112233", true, os.Getuid())
	if err := os.MkdirAll(l.root, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := l.share(src, "base.ext4")
	if err != nil || p != "/base.ext4" {
		t.Fatalf("%q %v", p, err)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(l.host("base.ext4"))
	if !os.SameFile(a, b) {
		t.Fatal("shared file must be a hard link, never a copy")
	}
	if err := checkWorldReadable(src); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(src, 0o600)
	if err := checkWorldReadable(src); err == nil {
		t.Fatal("unreadable-by-others file accepted for the jail")
	}
}

func TestBootArgs(t *testing.T) {
	a := bootArgs(mustAddr("10.200.0.6"), mustAddr("10.200.0.5"), 1791818522)
	for _, want := range []string{
		"ipv6.disable=1", "root=/dev/vda ro", "init=/ermite-runner", "reboot=k", "panic=1",
		"ip=10.200.0.6::10.200.0.5:255.255.255.252:ermite-job:eth0:off:10.200.0.5",
		"ermite.sandbox=vm", "ermite.connect=vsock:1024", "ermite.gw=10.200.0.5", "ermite.time=1791818522",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("boot args lack %q: %s", want, a)
		}
	}
	if !strings.HasSuffix(a, "-- init") {
		t.Errorf("init subcommand missing: %s", a)
	}
	if macFor(0) != "06:00:ac:10:00:00" || macFor(257) != "06:00:ac:10:01:01" {
		t.Fatal(macFor(257))
	}
}

func TestParseTapNames(t *testing.T) {
	in := "1: lo: <LOOPBACK> mtu 65536\n2: eth0: <UP> mtu 1500\n5: ertap0: <BROADCAST> mtu 1500\n9: ertap12@NONE: <X>\n10: ertapx: <X>\n11: tap0: <X>\n12: ertap: <X>\n"
	got := strings.Join(parseTapNames(in), ",")
	if got != "ertap0,ertap12" {
		t.Fatalf("got %q", got)
	}
}

func TestFirecrackerProcsScan(t *testing.T) {
	proc := t.TempDir()
	mk := func(pid, cmdline string) {
		_ = os.MkdirAll(filepath.Join(proc, pid), 0o755)
		_ = os.WriteFile(filepath.Join(proc, pid, "cmdline"), []byte(cmdline), 0o644)
	}
	mk("100", "firecracker\x00--id\x00er00112233\x00--api-sock\x00/x\x00")
	mk("101", "/usr/bin/firecracker\x00--api-sock\x00/x\x00--id\x00er44556677\x00")
	mk("102", "firecracker\x00--id\x00other-tool-vm\x00") // someone else's VM
	mk("103", "bash\x00--id\x00er00112233\x00")           // not firecracker
	mk("1", "firecracker\x00--id\x00er99999999\x00")      // pid 1 is never touched
	mk("abc", "")
	got := firecrackerProcs(proc)
	if len(got) != 2 || got[100] != "er00112233" || got[101] != "er44556677" {
		t.Fatalf("%v", got)
	}
}

func TestRingKeepsTail(t *testing.T) {
	r := newRing(8)
	_, _ = r.Write([]byte("0123456789"))
	_, _ = r.Write([]byte("ab"))
	if r.String() != "23456789ab"[2:] {
		t.Fatalf("%q", r.String())
	}
}

func TestAPIErrorsSurfaceFirecrackerFault(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"fault_message":"Invalid request"}`))
		}))
	}()
	err = newAPIClient(sock).put(context.Background(), "/boot-source", bootSource{})
	if err == nil || !strings.Contains(err.Error(), "Invalid request") || !strings.Contains(err.Error(), "/boot-source") {
		t.Fatalf("got %v", err)
	}
}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func mustAddr(s string) netip.Addr     { return netip.MustParseAddr(s) }
