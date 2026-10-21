// SPDX-License-Identifier: MIT

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
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
func newEnv(t *testing.T, mode string) *env { return newEnvNamed(t, mode, "firecracker") }

// newEnvNamed runs the fake VMM under another program name (argv[0]).
func newEnvNamed(t *testing.T, mode, argv0 string) *env {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash needed for the fake firecracker")
	}
	dir := t.TempDir()
	e := &env{taps: map[string]bool{}, plumb: sandboxtest.NewPlumbing(), log: filepath.Join(dir, "fc.log")}
	self, _ := os.Executable()
	fc := filepath.Join(dir, "firecracker")
	script := fmt.Sprintf("#!%s\nexport FC_HELPER=1 FC_MODE=%q FC_LOG=%q\nexec -a %q %q \"$@\"\n", bash, mode, e.log, argv0, self)
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
		LockFile: filepath.Join(dir, "host.lock"), JobPids: 1000, DiskBandwidthMiB: 400, DiskIOPS: 20000, NetMbit: 1000,
	}
	e.run = &cmdrun.Fake{Handle: func(c cmdrun.Call) ([]byte, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch {
		case c.Name == "mkfs.ext4" && len(c.Args) == 1 && c.Args[0] == "-V":
			return []byte("mke2fs 1.47.4 (6-Mar-2025)\n"), nil
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

func (e *env) driverScope() scope { return newScope(e.cfg.StateDir) }

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
	_, exit, err := execstream.Drive(conn, &execpb.Run{}, nil, func(o *execpb.Output) { out.Write(o.Data) })
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
	timings := sb.(sandbox.Timed).StartTimings()
	for _, key := range []string{"tap_ms", "net_ms", "vmm_ms", "files_ms", "scratch_ms", "vsock_ms", "configure_ms", "connect_ms"} {
		if value, ok := timings[key]; !ok || value < 0 {
			t.Errorf("%s = %d, present=%v", key, value, ok)
		}
	}
	timings["tap_ms"] = -1
	if sb.(sandbox.Timed).StartTimings()["tap_ms"] < 0 {
		t.Error("shared timing map")
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
	_ = d1.lock.Release()
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
	if err == nil || !strings.Contains(err.Error(), "another ermite-runner is running on this host") {
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
		if c.Name == "mkfs.ext4" && (len(c.Args) != 1 || c.Args[0] != "-V") {
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
	a := bootArgs(mustAddr("10.200.0.6"), mustAddr("10.200.0.5"), 1791818522, false)
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
	if strings.Contains(a, "ermite.externals") {
		t.Errorf("externals announced without a disk: %s", a)
	}
	if b := bootArgs(mustAddr("10.200.0.6"), mustAddr("10.200.0.5"), 1791818522, true); !strings.Contains(b, " ermite.externals=1 -- init") {
		t.Errorf("externals not announced: %s", b)
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
	mk("100", "firecracker\x00--id\x00er00112233\x00--api-sock\x00/var/lib/er/vm/er00112233/api.sock\x00")
	mk("101", "/usr/bin/firecracker\x00--api-sock\x00/var/lib/er/vm/er44556677/api.sock\x00--id\x00er44556677\x00")
	mk("102", "firecracker\x00--id\x00other-tool-vm\x00") // someone else's VM
	mk("103", "bash\x00--id\x00er00112233\x00")           // not firecracker
	mk("1", "firecracker\x00--id\x00er99999999\x00")      // pid 1 is never touched
	mk("abc", "")
	got := vmmProcs(proc, newScope("/var/lib/er"))
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

// C3: processes are matched by "--id <our id>" in their command line, not by the
// program name: a renamed or versioned Firecracker binary is still found and
// reaped, and a recycled pid is never killed on the strength of a record alone.
func TestReapFindsRenamedVMMByID(t *testing.T) {
	e := newEnvNamed(t, "", "fc-renamed-1.17")
	d1 := e.driver(t)
	sb, _ := d1.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)
	pid := sb.(*vmSandbox).rec.PID
	if got := cmdlineID("/proc", pid, e.driverScope()); got != sb.ID() {
		t.Fatalf("cmdlineID=%q want %q (argv[0] is %q)", got, sb.ID(), "fc-renamed-1.17")
	}
	_ = d1.lock.Release()
	_ = conn.Close()
	d2 := e.driver(t)
	if err := d2.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pidAlive(pid) {
		t.Fatalf("a VMM with a renamed binary survived Reap (pid %d)", pid)
	}
}

func TestIDFromCmdline(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"firecracker": {[]string{"firecracker", "--id", "er00112233", "--api-sock", "/x"}, "er00112233"},
		"jailer":      {[]string{"jailer", "--id", "er00112233", "--exec-file", "/fc", "--uid", "1"}, "er00112233"},
		"renamed":     {[]string{"anything", "--api-sock", "/x", "--id", "er44556677"}, "er44556677"},
		// What a real jailer 1.17 leaves: /firecracker --id <id> --start-time-us ...
		"jailed firecracker": {[]string{"/firecracker", "--id", "er00112233", "--start-time-us", "150801025577", "--start-time-cpu-us", "60", "--parent-cpu-time-us", "1332"}, "er00112233"},
		"foreign id":         {[]string{"firecracker", "--id", "my-vm", "--api-sock", "/x"}, ""},
		"no marker":          {[]string{"python", "--id", "er00112233"}, ""},
		"id without value":   {[]string{"firecracker", "--api-sock", "/x", "--id"}, ""},
		"path-like id":       {[]string{"firecracker", "--id", "er../../../x", "--api-sock", "/x"}, ""},
	} {
		if got := IDFromCmdline(tc.args); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestReapDoesNotTrustARecycledPid(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	// A record pointing at this very test process: its command line carries no
	// VM id, so it must not be killed.
	_ = d.store.save(record{ID: "er00112233", Slot: 0, PID: os.Getpid()})
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pidAlive(os.Getpid()) {
		t.Fatal("Reap killed a process on the strength of a stale record")
	}
}

// C5 + C8: when the VMM cannot be killed, the TAP, the firewall policy and the
// files are left alone (the VM may still be running), the error wraps ErrLeaked,
// the slot stays taken, and Reap (with a working kill) finishes the cleanup and
// frees the slot.
func TestStopKeepsNetworkWhenTheVMMSurvives(t *testing.T) {
	e := newEnv(t, "")
	stubborn := true
	d, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 5 * time.Second,
		kill: func(pid int, still func() bool, to time.Duration) bool {
			if stubborn {
				return false
			}
			return killAndWait(pid, still, to)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)

	err = sb.Stop(context.Background())
	if !errors.Is(err, sandbox.ErrLeaked) {
		t.Fatalf("Stop must report ErrLeaked, got %v", err)
	}
	if e.tapCount() != 1 || e.plumb.Live() != 1 {
		t.Fatalf("the TAP and the policy must stay while the VMM may run: taps=%d attached=%d", e.tapCount(), e.plumb.Live())
	}
	if recs, _ := d.store.list(); len(recs) != 1 {
		t.Fatalf("the state record must stay for Reap: %v", recs)
	}
	if err := sb.Stop(context.Background()); !errors.Is(err, sandbox.ErrLeaked) {
		t.Fatalf("a second Stop must keep reporting the leak, got %v", err)
	}
	if d.slots.InUse() != 1 {
		t.Fatal("the slot of a leaked sandbox stays taken")
	}
	// Other jobs keep running on the free slot; the leaked one is never reused.
	other, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	if other.(*vmSandbox).slot == sb.(*vmSandbox).slot {
		t.Fatal("a leaked slot was handed out again")
	}
	// Reap refuses while a sandbox is running...
	if err := d.Reap(context.Background()); !errors.Is(err, sandbox.ErrBusy) {
		t.Fatalf("Reap with a handed-out sandbox: %v", err)
	}
	_ = other.Stop(context.Background())
	// ...and finishes the job once idle.
	stubborn = false
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers after Reap: %v", left)
	}
	if d.slots.InUse() != 0 {
		t.Fatal("Reap must free the slot of the leaked sandbox")
	}
}

// C7: Stop from another goroutine aborts a boot in progress at once, instead of
// waiting for the boot timeout, and leaves nothing behind.
func TestStopAbortsABootInProgress(t *testing.T) {
	e := newEnv(t, "hang")
	d, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	sb, _ := d.New()
	started := make(chan error, 1)
	go func() {
		_, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
		started <- err
	}()
	time.Sleep(700 * time.Millisecond) // the fake is now waiting for the executor
	t0 := time.Now()
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if !errors.Is(err, sandbox.ErrAborted) {
			t.Fatalf("Start must report that Stop aborted it, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start was not aborted by Stop")
	}
	if time.Since(t0) > 8*time.Second {
		t.Fatalf("Stop took %v", time.Since(t0))
	}
	if left := e.leftovers(t); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
	// Stop before Start: Start must refuse.
	sb2, _ := d.New()
	_ = sb2.Stop(context.Background())
	if _, err := sb2.Start(context.Background(), sandbox.JobSpec{}); err == nil {
		t.Fatal("Start after Stop accepted")
	}
}

// C16: the sandbox exposes its id, which names its files.
func TestSandboxIDNamesItsFiles(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb, _ := d.New()
	if !sandbox.ValidID(sb.ID()) {
		t.Fatalf("id %q", sb.ID())
	}
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)
	defer func() { _ = sb.Stop(context.Background()) }()
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "vm", sb.ID())); err != nil {
		t.Fatalf("the VM directory must be named after the sandbox id: %v", err)
	}
	if d.Isolation() != sandbox.IsolationVM || d.Mode() != sandbox.VM {
		t.Fatal("isolation")
	}
}

// S7: cgroup v2 limits, a new PID namespace, and the jailer's own options.
func TestJailerArgs(t *testing.T) {
	args := jailerArgs("/usr/bin/jailer", "/usr/bin/firecracker", "er00112233", 900003, "/var/lib/er/jail",
		sandbox.JobSpec{CPUs: 4, MemoryMiB: 2048}, nil)
	line := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --id er00112233 ", " --exec-file /usr/bin/firecracker ", " --uid 900003 ", " --gid 900003 ",
		" --chroot-base-dir /var/lib/er/jail ", " --cgroup-version 2 ", " --new-pid-ns ",
		" --cgroup memory.max=" + fmt.Sprint((2048+vmmOverheadMiB)<<20) + " ",
		" --cgroup pids.max=64 ", " --resource-limit no-file=4096 ", " -- ",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("jailer args lack %q: %s", want, line)
		}
	}
	if args[len(args)-1] != "--" {
		t.Errorf("the jailer separator must come last when there are no extra args: %v", args)
	}
	more := jailerArgs("j", "f", "er00112233", 1, "/b", sandbox.JobSpec{CPUs: 1, MemoryMiB: 512}, []string{"--x"})
	if more[len(more)-2] != "--" || more[len(more)-1] != "--x" {
		t.Errorf("extra args follow the separator: %v", more)
	}
}

// S7: sane rate limiters on the scratch disk and both directions of eth0, off
// when configured to zero, and only on the writable disk.
func TestRateLimitersInTheVMConfig(t *testing.T) {
	e := newEnv(t, "")
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	runStub(t, conn)
	defer func() { _ = sb.Stop(context.Background()) }()
	b, _ := os.ReadFile(e.log)
	log := string(b)
	for _, want := range []string{
		`"rate_limiter":{"bandwidth":{"size":419430400,"one_time_burst":419430400,"refill_time":1000},"ops":{"size":20000,"one_time_burst":20000,"refill_time":1000}}`,
		`"rx_rate_limiter":{"bandwidth":{"size":125000000,"one_time_burst":125000000,"refill_time":1000}}`,
		`"tx_rate_limiter":{"bandwidth":{"size":125000000,"one_time_burst":125000000,"refill_time":1000}}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("API log lacks %s\n%s", want, log)
		}
	}
	if n := strings.Count(log, `"rate_limiter"`); n != 1 {
		t.Errorf("only the scratch drive is rate limited, got %d rate_limiter fields", n)
	}
	if diskLimiter(0, 0) != nil || netLimiter(0) != nil {
		t.Error("zero must disable the limiters")
	}
	if l := diskLimiter(0, 100); l.Bandwidth != nil || l.Ops == nil {
		t.Errorf("%+v", l)
	}
}

// S8: the free-space check warns, and only warns.
func TestNewWarnsOnLowFreeSpace(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf}))
	d, err := New(ctx, e.cfg, Options{Run: e.run, Net: e.plumb, FreeSpace: func(string) (int64, error) { return 1, nil }})
	if err != nil {
		t.Fatalf("low space must not fail New: %v", err)
	}
	defer func() { _ = d.Close() }()
	if !strings.Contains(buf.String(), "less free space") {
		t.Fatalf("no warning: %s", buf.String())
	}
}

// S11: the scratch disk is created without following symlinks and chowned
// through its descriptor.
func TestMakeScratchRefusesSymlinksAndExistingFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	_ = os.WriteFile(target, []byte("precious"), 0o600)
	link := filepath.Join(dir, "scratch.ext4")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	run := &cmdrun.Fake{}
	if err := disk.CreateScratch(context.Background(), run, link, 8, -1); err == nil {
		t.Fatal("a planted symlink must be refused")
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatalf("the symlink target was modified: %q", b)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("mkfs must not run: %v", run.Lines())
	}
	ok := filepath.Join(dir, "fresh.ext4")
	if err := disk.CreateScratch(context.Background(), run, ok, 8, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(ok); err != nil || st.Size() != 8<<20 || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if err := disk.CreateScratch(context.Background(), run, ok, 8, -1); err == nil {
		t.Fatal("an existing file must be refused (O_EXCL)")
	}
}

func TestOwnUsesLchown(t *testing.T) {
	dir := t.TempDir()
	l := newLayout(dir, "firecracker", "er00112233", true, os.Getuid())
	_ = os.MkdirAll(l.root, 0o700)
	target := filepath.Join(dir, "victim")
	_ = os.WriteFile(target, nil, 0o600)
	if err := os.Symlink(target, l.host("link")); err != nil {
		t.Fatal(err)
	}
	if err := l.own("link"); err != nil {
		t.Fatalf("Lchown on a link must act on the link itself: %v", err)
	}
	if err := l.own("missing"); err == nil {
		t.Fatal("a missing file is an error")
	}
	if u := newLayout(dir, "f", "er00112233", false, 1); u.own("anything") != nil {
		t.Fatal("no-op without jailer")
	}
}

func TestRemoveCgroup(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "firecracker", "er00112233"), 0o755)
	if err := removeCgroup(root, "firecracker", "er00112233"); err != nil {
		t.Fatal(err)
	}
	if err := removeCgroup(root, "firecracker", "er00112233"); err != nil {
		t.Fatalf("a missing cgroup is fine: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(root, "firecracker", "er44556677", "child"), 0o755)
	if err := removeCgroup(root, "firecracker", "er44556677"); err == nil {
		t.Fatal("a non-empty cgroup cannot be removed and must be reported")
	}
}

// Jailer 1.17 canonicalizes --chroot-base-dir and exits when it does not exist:
// the driver must create it (seen on a real host, invisible with the jailer off).
func TestNewCreatesTheJailerChrootBase(t *testing.T) {
	e := newEnv(t, "ok")
	e.cfg.JailerOff = false
	e.cfg.Jailer = e.cfg.Firecracker // any executable: it is only resolved here
	short, err := os.MkdirTemp("", "er")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(short) }()
	e.cfg.StateDir = filepath.Join(short, "s") // the jailed API socket path must fit in sun_path
	d, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	st, err := os.Stat(jailBase(e.cfg.StateDir))
	if err != nil || !st.IsDir() {
		t.Fatalf("jail base not created: %v", err)
	}
}

// GO-004: the longest socket path of a VM, in both jailer modes, 107 bytes ok /
// 108 rejected.
func TestSocketPathBoundaryInBothModes(t *testing.T) {
	for _, jailed := range []bool{true, false} {
		name := map[bool]string{true: "jailed", false: "unjailed"}[jailed]
		// Find the longest socket path with an empty-ish state dir, then pad the
		// state dir so that it is exactly maxSockPath, then one more.
		longest := func(state string) int {
			n := 0
			for _, p := range socketPaths(state, "firecracker", jailed) {
				n = max(n, len(p))
			}
			return n
		}
		base := "/s"
		pad := maxSockPath - longest(base)
		if pad < 0 {
			t.Fatalf("%s: base already too long", name)
		}
		ok := base + strings.Repeat("d", pad)
		if longest(ok) != maxSockPath {
			t.Fatalf("%s: longest=%d want %d", name, longest(ok), maxSockPath)
		}
		if err := checkSocketPaths(ok, "firecracker", jailed); err != nil {
			t.Errorf("%s: %d bytes rejected: %v", name, maxSockPath, err)
		}
		err := checkSocketPaths(ok+"d", "firecracker", jailed)
		if err == nil || !strings.Contains(err.Error(), "limit of a Unix socket") {
			t.Errorf("%s: %d bytes accepted: %v", name, maxSockPath+1, err)
		}
	}
}

func TestNewRejectsAStateDirThatMakesASocketPathTooLong(t *testing.T) {
	for _, jailerOff := range []bool{false, true} {
		e := newEnv(t, "ok")
		e.cfg.JailerOff = jailerOff
		e.cfg.Jailer = e.cfg.Firecracker
		e.cfg.StateDir = filepath.Join(e.cfg.StateDir, strings.Repeat("d", 60))
		_, err := New(context.Background(), e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 5 * time.Second})
		if err == nil || !strings.Contains(err.Error(), "limit of a Unix socket") {
			t.Fatalf("jailerOff=%v: want a clear socket path error, got %v", jailerOff, err)
		}
	}
}

// GO-003: the chroot base must be a real directory, owned by root (or the
// runner's own user), not writable by group or others; an existing one is never
// chmod-ed into compliance.
func TestEnsureJailBase(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := ensureJailBase(good); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(good); fi.Mode().Perm() != 0o700 {
		t.Fatalf("created with %v", fi.Mode().Perm())
	}
	if err := ensureJailBase(good); err != nil { // idempotent
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"group-writable": 0o770, "other-writable": 0o707, "sticky-world": 0o777} {
		d := filepath.Join(root, name)
		_ = os.Mkdir(d, 0o700)
		_ = os.Chmod(d, mode)
		if err := ensureJailBase(d); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Errorf("%s: %v", name, err)
		}
		if fi, _ := os.Stat(d); fi.Mode().Perm() != mode {
			t.Errorf("%s: mode was changed to %v", name, fi.Mode().Perm())
		}
	}
	rx := filepath.Join(root, "rx")
	_ = os.Mkdir(rx, 0o755) // readable by others is fine
	_ = os.Chmod(rx, 0o755)
	if err := ensureJailBase(rx); err != nil {
		t.Errorf("0755 refused: %v", err)
	}
	file := filepath.Join(root, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if err := ensureJailBase(file); err == nil {
		t.Error("a file accepted")
	}
	link := filepath.Join(root, "link")
	_ = os.Symlink(good, link)
	if err := ensureJailBase(link); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a symlink accepted: %v", err)
	}
}

type fakeInfo struct {
	fs.FileInfo
	uid uint32
}

func (f fakeInfo) Sys() any { return &syscall.Stat_t{Uid: f.uid} }

func TestJailBaseOwner(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := "/x/jail"
	mine := os.Geteuid()
	if err := checkJailBaseInfo(d, fakeInfo{fi, 0}, mine); err != nil {
		t.Errorf("root-owned refused: %v", err)
	}
	if err := checkJailBaseInfo(d, fakeInfo{fi, uint32(mine)}, mine); err != nil {
		t.Errorf("own user refused: %v", err)
	}
	if err := checkJailBaseInfo(d, fakeInfo{fi, uint32(mine) + 4242}, mine); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("foreign owner accepted: %v", err)
	}
}

// GO-001: a Firecracker with only the jailer's --start-time-us is ours only when
// it lives in our jail base; a jailer only when it was given our chroot base.
func TestScanIsScopedToOurJailBase(t *testing.T) {
	state := t.TempDir()
	sc := newScope(state)
	foreign := t.TempDir()
	proc := t.TempDir()
	mk := func(pid, root string, args ...string) {
		dir := filepath.Join(proc, pid)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o644)
		if root != "" {
			if err := os.Symlink(root, filepath.Join(dir, "root")); err != nil {
				t.Fatal(err)
			}
		}
	}
	ours := filepath.Join(jailBase(state), "firecracker", "er00112233", "root")
	if err := os.MkdirAll(ours, 0o755); err != nil {
		t.Fatal(err)
	}
	// What a pivot_root'ed jailed VMM looks like: its root is our chroot directory
	// but readlink does not say so (it says "/"); a symlinked alias reproduces
	// that: another path, same inode.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(ours, alias); err != nil {
		t.Fatal(err)
	}
	// Our jail base has a chroot for another id: a foreign VMM must not match it.
	if err := os.MkdirAll(filepath.Join(jailBase(state), "firecracker", "er44556677", "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	foreignRoot := filepath.Join(foreign, "jail", "firecracker", "er44556677", "root")
	_ = os.MkdirAll(foreignRoot, 0o755)
	mk("100", ours, "/firecracker", "--id", "er00112233", "--start-time-us", "1", "--start-time-cpu-us", "2")
	mk("101", foreignRoot, "/firecracker", "--id", "er44556677", "--start-time-us", "1")
	mk("109", alias, "/firecracker", "--id", "er00112233", "--start-time-us", "1") // same inode, other path
	mk("110", alias, "/firecracker", "--id", "er77777777", "--start-time-us", "1") // our dir, but not that id's chroot
	mk("102", "/", "/firecracker", "--id", "er8899aabb", "--start-time-us", "1")   // not chrooted at all
	mk("103", "", "/firecracker", "--id", "er0000cccc", "--start-time-us", "1")    // root unreadable
	mk("104", "/", "jailer", "--id", "er11111111", "--exec-file", "/fc", "--chroot-base-dir", jailBase(state))
	mk("105", "/", "jailer", "--id", "er22222222", "--exec-file", "/fc", "--chroot-base-dir", filepath.Join(foreign, "jail"))
	mk("106", "/", "jailer", "--id", "er33333333", "--exec-file", "/fc")
	mk("107", ours+"/../../../../x", "/firecracker", "--id", "er55555555", "--start-time-us", "1") // lexical escape
	vmDir := filepath.Join(state, "vm", "er66666666")
	mk("108", "/", "firecracker", "--id", "er66666666", "--api-sock", filepath.Join(vmDir, "api.sock")) // unjailed, in our dir
	mk("111", "/", "firecracker", "--id", "er99999999", "--api-sock", "/x")                             // socket elsewhere
	mk("112", "/", "firecracker", "--id", "erabababab", "--api-sock", filepath.Join(vmDir, "api.sock")) // our dir, another id
	mk("113", "/", "firecracker", "--id", "er66666666", "--api-sock", vmDir+"/../../x/api.sock")        // escapes by ..
	mk("114", "/", "firecracker", "--id", "er66666666", "--api-sock", "vm/er66666666/api.sock")         // relative
	mk("115", "/", "firecracker", "--id", "er66666666", "--api-sock")                                   // no value
	mk("116", "/", "firecracker", "--id", "er66666666", "--api-sock", vmDir+"x/api.sock")               // prefix sibling
	got := vmmProcs(proc, sc)
	want := map[int]string{100: "er00112233", 104: "er11111111", 108: "er66666666", 109: "er00112233"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for pid, id := range want {
		if got[pid] != id {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if cmdlineID(proc, 101, sc) != "" || cmdlineID(proc, 100, sc) != "er00112233" {
		t.Fatal("cmdlineID must apply the same scope")
	}
}

// GO-001 (pid reuse): the kill goes through a pidfd, after the identity check.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

func needPidfd(t *testing.T) {
	t.Helper()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("no pidfd on this kernel: %v", err)
	}
	_ = unix.Close(fd)
}

func TestKillAndWaitKillsThroughThePidfd(t *testing.T) {
	needPidfd(t)
	c := startSleeper(t)
	asked := false
	if !killAndWait(c.Process.Pid, func() bool { asked = true; return true }, 5*time.Second) {
		t.Fatal("the process is still there")
	}
	if !asked {
		t.Fatal("the identity check did not run")
	}
	// The child is a zombie now (we have not waited for it): it must count as gone.
	if _, err := c.Process.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestKillAndWaitDoesNotKillAMismatchAfterTheOpen(t *testing.T) {
	needPidfd(t)
	c := startSleeper(t)
	// The pid now belongs to something that is not the VMM we meant (what a
	// recycled pid looks like): no signal, and "gone" (the VMM we meant is).
	if !killAndWait(c.Process.Pid, func() bool { return false }, 2*time.Second) {
		t.Fatal("a mismatch means the intended process is gone: true expected")
	}
	if !pidAlive(c.Process.Pid) {
		t.Fatal("a process that failed the identity check was killed")
	}
}

func TestKillAndWaitOnAProcessThatIsAlreadyGone(t *testing.T) {
	needPidfd(t)
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Skip(err)
	}
	called := false
	if !killAndWait(c.Process.Pid, func() bool { called = true; return true }, time.Second) {
		t.Fatal("a reaped process must count as gone")
	}
	_ = called
}

func TestKillAndWaitReportsAProcessThatStays(t *testing.T) {
	needPidfd(t)
	// pid 1 cannot be killed by us in a test (and must never be: the identity
	// check says it is ours here only to show the failure path would not hang).
	if os.Geteuid() == 0 {
		t.Skip("root could signal pid 1")
	}
	if killAndWait(1, func() bool { return true }, 200*time.Millisecond) {
		t.Fatal("an unkillable process was reported gone")
	}
}

// Guest forge access: the exception reaches the policy of the guest, and the
// warning naming the origin is logged at the driver start and at every job.
func TestGuestForgeIsAttachedAndWarnedAtEveryJob(t *testing.T) {
	e := newEnv(t, "")
	var err error
	if e.cfg.GuestForge, err = sandbox.ParseGuestForge("https://ermite.example.org:8443", false); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf}))
	d, err := New(ctx, e.cfg, Options{Run: e.run, Net: e.plumb, BootTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	msg := "guest forge access: sandboxes may reach https://ermite.example.org:8443 on the host"
	if n := strings.Count(buf.String(), msg); n != 1 {
		t.Fatalf("one warning at driver start, got %d", n)
	}
	for i := range 2 {
		sb, _ := d.New()
		conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: fmt.Sprint("job", i)})
		if err != nil {
			t.Fatal(err)
		}
		if len(e.plumb.Attached) != 1 {
			t.Fatalf("%d attached", len(e.plumb.Attached))
		}
		for _, g := range e.plumb.Attached {
			if g.ForgePort != 8443 {
				t.Errorf("guest attached with ForgePort %d", g.ForgePort)
			}
		}
		runStub(t, conn)
		_ = sb.Stop(ctx)
	}
	if n := strings.Count(buf.String(), msg); n != 3 {
		t.Fatalf("the warning must be logged at every job start (1 + 2), got %d", n)
	}
}

// The host's external tools ride on a fourth, read-only drive, keyed by their
// content: a new Node.js builds a new disk and leaves the exec disk alone.
func TestExternalsDiskIsAttachedReadOnlyAndKeyedByContent(t *testing.T) {
	e := newEnv(t, "")
	src := &sandboxtest.Externals{}
	n24 := sandboxtest.MakeExternal(t, "node24", "node24-24.21.0-x64-aaaa", "v24.21.0")
	n20 := sandboxtest.MakeExternal(t, "node20", "node20-20.20.2-x64-bbbb", "v20.20.2")
	src.Set(n24, n20)
	e.cfg.Externals = src
	d := e.driver(t)
	execDisk := d.execDisk

	boot := func() (drives []string, log string) {
		t.Helper()
		_ = os.Remove(e.log)
		sb, _ := d.New()
		conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
		if err != nil {
			t.Fatal(err)
		}
		runStub(t, conn)
		defer func() { _ = sb.Stop(context.Background()) }()
		b, _ := os.ReadFile(e.log)
		log = string(b)
		for _, l := range strings.Split(log, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "PUT" && strings.HasPrefix(f[1], "/drives/") {
				drives = append(drives, f[1])
			}
		}
		return drives, log
	}
	drives, log := boot()
	if strings.Join(drives, ",") != "/drives/rootfs,/drives/base,/drives/scratch,/drives/externals" {
		t.Fatalf("drives: %v", drives)
	}
	if !strings.Contains(log, `"drive_id":"externals"`) || !strings.Contains(log, `"is_root_device":false,"is_read_only":true`) || !strings.Contains(log, "ermite.externals=1") {
		t.Fatalf("API log:\n%s", log)
	}
	// The scratch drive is still the third (vdc), and the only writable one.
	if strings.Count(log, `"is_read_only":false`) != 1 {
		t.Errorf("only the scratch drive is writable:\n%s", log)
	}
	first, _ := filepath.Glob(filepath.Join(e.cfg.StateDir, "cache", "extdisk-*"))
	if len(first) != 1 {
		t.Fatalf("externals disks: %v", first)
	}
	mkfs := func() (n int) {
		for _, c := range e.run.Calls {
			if c.Name == "mkfs.ext4" && len(c.Args) > 1 {
				for _, a := range c.Args {
					if a == "ermite-ext" {
						n++
					}
				}
			}
		}
		return
	}
	if mkfs() != 1 {
		t.Fatalf("externals disk built %d times", mkfs())
	}

	// A second job with the same tools reuses the disk.
	boot()
	if mkfs() != 1 {
		t.Fatal("externals disk rebuilt for the same content")
	}

	// A new Node.js: another disk, and the exec disk is untouched.
	src.Set(sandboxtest.MakeExternal(t, "node24", "node24-24.22.0-x64-cccc", "v24.22.0"), n20)
	boot()
	if mkfs() != 2 {
		t.Fatal("externals disk not rebuilt for a new version")
	}
	if d.execDisk != execDisk {
		t.Fatal("exec disk changed")
	}
	if _, err := os.Stat(execDisk); err != nil {
		t.Fatal("exec disk removed")
	}
	if now, _ := filepath.Glob(filepath.Join(e.cfg.StateDir, "cache", "extdisk-*")); len(now) != 2 || (now[0] != first[0] && now[1] != first[0]) {
		// The previous disk may still be linked by a starting VM: kept for an hour.
		t.Fatalf("externals disks: %v", now)
	}
}

func TestNoExternalsMeansThreeDrives(t *testing.T) {
	for _, src := range []sandbox.ExternalsSource{nil, &sandboxtest.Externals{}} {
		e := newEnv(t, "")
		e.cfg.Externals = src
		d := e.driver(t)
		sb, _ := d.New()
		conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
		if err != nil {
			t.Fatal(err)
		}
		runStub(t, conn)
		b, _ := os.ReadFile(e.log)
		if strings.Contains(string(b), "externals") {
			t.Errorf("externals attached without any:\n%s", b)
		}
		_ = sb.Stop(context.Background())
		if left, _ := filepath.Glob(filepath.Join(e.cfg.StateDir, "cache", "extdisk-*")); len(left) != 0 {
			t.Errorf("disk built for nothing: %v", left)
		}
	}
}

func TestExternalsDiskFailureDoesNotStopTheJob(t *testing.T) {
	e := newEnv(t, "")
	src := &sandboxtest.Externals{}
	src.Set(sandbox.External{Name: "node24", ID: "x", Dir: filepath.Join(t.TempDir(), "missing")})
	e.cfg.Externals = src
	var buf bytes.Buffer
	ctx := zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf}))
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatalf("a broken externals set must not stop the job: %v", err)
	}
	runStub(t, conn)
	_ = sb.Stop(ctx)
	if !strings.Contains(buf.String(), "external tools (Node.js) not attached") {
		t.Fatalf("failure not logged: %s", buf.String())
	}
	b, _ := os.ReadFile(e.log)
	if strings.Contains(string(b), `"externals"`) {
		t.Fatal("drive attached after a failure")
	}
}

// With the real mkfs.ext4 the disk holds <name>/bin/node at its root, which the
// guest mounts at /home/runner/externals.
func TestExternalsDiskContent(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 needed")
	}
	dbg, err := exec.LookPath("debugfs")
	if err != nil {
		t.Skip("debugfs needed")
	}
	set := []sandbox.External{
		sandboxtest.MakeExternal(t, "node24", "a", "v24"),
		sandboxtest.MakeExternal(t, "node20", "b", "v20"),
	}
	path, err := ensureExternalsDisk(context.Background(), cmdrun.Exec{}, set, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := func(cmd string) string {
		b, err := exec.Command(dbg, "-R", cmd, path).CombinedOutput()
		if err != nil {
			t.Fatalf("debugfs %s: %v\n%s", cmd, err, b)
		}
		_, body, _ := strings.Cut(string(b), "\n") // the banner line
		return body
	}
	if ls := out("ls -l /"); !strings.Contains(ls, "node24") || !strings.Contains(ls, "node20") {
		t.Fatalf("root:\n%s", ls)
	}
	if got := out("cat /node24/bin/node"); got != "v24" {
		t.Fatalf("node24/bin/node: %q", got)
	}
	if ls := out("stat /node20/bin/node"); !strings.Contains(ls, "Mode:  0755") {
		t.Fatalf("mode:\n%s", ls)
	}
	if ls := out("stat /node20/nodejs"); !strings.Contains(ls, "Type: symlink") || !strings.Contains(ls, `"bin/node"`) {
		t.Fatalf("symlink:\n%s", ls)
	}
	if st, _ := os.Stat(path); st.Size() > 64<<20 {
		t.Fatalf("a tiny tree gave a %d byte disk", st.Size())
	}
}
