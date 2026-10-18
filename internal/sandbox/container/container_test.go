// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/moby/moby/api/types/container"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

// fakeEngine plays the Docker daemon. When a container "starts", it plays the
// executor inside it by connecting to the bind-mounted socket.
type fakeEngine struct {
	mu         sync.Mutex
	info       daemonInfo
	infoErr    error
	imageErr   error
	networks   map[string]networkSpec
	containers map[string]containerSpec
	failStart  error
	noConnect  bool
	exitOnRun  bool
	removeErr  error
	waiters    map[string]chan exitResult
	seq        int
	pulled     []string
}

func newFake() *fakeEngine {
	return &fakeEngine{
		info:       daemonInfo{OSType: "linux", Runtimes: []string{"runc"}},
		networks:   map[string]networkSpec{},
		containers: map[string]containerSpec{},
		waiters:    map[string]chan exitResult{},
	}
}

func (f *fakeEngine) Info(context.Context) (daemonInfo, error) { return f.info, f.infoErr }
func (f *fakeEngine) EnsureImage(_ context.Context, ref string) error {
	f.pulled = append(f.pulled, ref)
	return f.imageErr
}
func (f *fakeEngine) CreateNetwork(_ context.Context, s networkSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("net%d", f.seq)
	f.networks[id] = s
	return id, nil
}
func (f *fakeEngine) RemoveNetwork(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, id)
	return nil
}
func (f *fakeEngine) ListNetworks(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, n := range f.networks {
		if _, ok := n.Labels[sandbox.LabelJob]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}
func (f *fakeEngine) CreateContainer(_ context.Context, s containerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("ctr%d", f.seq)
	f.containers[id] = s
	f.waiters[id] = make(chan exitResult, 1)
	return id, nil
}
func (f *fakeEngine) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	spec := f.containers[id]
	fail, noConnect, exitOnRun := f.failStart, f.noConnect, f.exitOnRun
	w := f.waiters[id]
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	if exitOnRun {
		w <- exitResult{Code: 125}
		return nil
	}
	if noConnect {
		return nil
	}
	// The host side of the bind mount is the source of the run-dir mount.
	var sock string
	for _, b := range spec.Binds {
		if b.Target == sandbox.ContainerRunDir {
			sock = filepath.Join(b.Source, sandbox.ContainerSocket)
		}
	}
	go func() {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		g := execstream.NewConn(c)
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{Sandbox: "container"}}})
		if _, err := g.Recv(); err != nil {
			return
		}
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Output{Output: &execpb.Output{Data: []byte("hello from fake container\n")}}})
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 7}}})
		_, _ = g.Recv()
	}()
	return nil
}
func (f *fakeEngine) WaitExit(_ context.Context, id string) <-chan exitResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waiters[id]
}
func (f *fakeEngine) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.containers, id)
	return nil
}
func (f *fakeEngine) ListContainers(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.containers {
		out = append(out, id)
	}
	return out, nil
}
func (f *fakeEngine) LogsTail(context.Context, string, int) (string, error) {
	return "fake container log line", nil
}
func (f *fakeEngine) Close() error { return nil }

func (f *fakeEngine) objects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.containers {
		out = append(out, "container "+id)
	}
	for id := range f.networks {
		out = append(out, "network "+id)
	}
	return out
}

// fakeMounts plays mount/umount: it records mounts and writes a mountinfo file
// that disk.Mounter reads.
type fakeMounts struct {
	mu      sync.Mutex
	mounted map[string]bool
	file    string
	failUm  error
}

func (m *fakeMounts) sync() {
	var b strings.Builder
	for d := range m.mounted {
		fmt.Fprintf(&b, "40 28 7:0 / %s rw - ext4 /dev/loop0 rw\n", d)
	}
	_ = os.WriteFile(m.file, []byte(b.String()), 0o644)
}

func (m *fakeMounts) handle(c cmdrun.Call) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch c.Name {
	case "mkfs.ext4":
		if len(c.Args) == 1 && c.Args[0] == "-V" {
			return []byte("mke2fs 1.47.4 (6-Mar-2025)\n"), nil
		}
	case "mount":
		m.mounted[c.Args[len(c.Args)-1]] = true
		m.sync()
	case "umount":
		if m.failUm != nil {
			return nil, m.failUm
		}
		delete(m.mounted, c.Args[0])
		m.sync()
	}
	return nil, nil
}

func (m *fakeMounts) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.mounted) }

type cenv struct {
	cfg    sandbox.Config
	eng    *fakeEngine
	plumb  *sandboxtest.Plumbing
	mounts *fakeMounts
	run    *cmdrun.Fake
}

func newCEnv(t *testing.T) *cenv {
	t.Helper()
	// A short directory: the executor socket path is limited to 107 bytes and
	// t.TempDir embeds the (long) test name.
	dir, err := os.MkdirTemp("", "erc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	exe := filepath.Join(dir, "runner")
	sandboxtest.WriteStaticELF(t, exe)
	fm := &fakeMounts{mounted: map[string]bool{}, file: filepath.Join(dir, "mountinfo")}
	fm.sync()
	return &cenv{
		eng:    newFake(),
		plumb:  sandboxtest.NewPlumbing(),
		mounts: fm,
		run:    &cmdrun.Fake{Handle: fm.handle},
		cfg: sandbox.Config{
			Isolation: sandbox.Container, StateDir: filepath.Join(dir, "state"), Capacity: 2,
			JobCPUs: 2, JobMemoryMiB: 1024, JobDiskMiB: 1024, UIDBase: 900000,
			Network: netip.MustParsePrefix("10.200.0.0/16"), Executable: exe, DNSPort: 0,
			LockFile: filepath.Join(dir, "host.lock"), JobPids: 32768,
		},
	}
}

func (e *cenv) opts() Options {
	return Options{engine: e.eng, Net: e.plumb, ConnectTimeout: 3 * time.Second, Run: e.run,
		Mounter: &disk.Mounter{Run: e.run, Mountinfo: e.mounts.file}}
}

func (e *cenv) driver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(context.Background(), e.cfg, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func withTimeout(o Options, d time.Duration) Options { o.ConnectTimeout = d; return o }

func (e *cenv) leftovers() []string {
	out := e.eng.objects()
	if n := e.mounts.count(); n != 0 {
		out = append(out, fmt.Sprintf("%d scratch mounts", n))
	}
	if n := e.plumb.Live(); n != 0 {
		out = append(out, fmt.Sprintf("%d attached guests", n))
	}
	ents, _ := os.ReadDir(filepath.Join(e.cfg.StateDir, "containers"))
	for _, d := range ents {
		out = append(out, "dir "+d.Name())
	}
	return out
}

func stub(t *testing.T, conn sandbox.Conn) (string, int) {
	t.Helper()
	var out strings.Builder
	_, exit, err := execstream.Drive(conn, &execpb.Run{}, nil, func(o *execpb.Output) { out.Write(o.Data) })
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), int(exit.Code)
}

func TestRootlessDockerIsRefused(t *testing.T) {
	e := newCEnv(t)
	e.eng.info.Rootless = true
	_, err := New(context.Background(), e.cfg, e.opts())
	if err == nil || !strings.Contains(err.Error(), "rootless") || !strings.Contains(err.Error(), "rootful") {
		t.Fatalf("want a clear rootless error, got %v", err)
	}
}

func TestRuntimeSelection(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	if !d.Privileged() || d.runtime != "" {
		t.Fatal("without sysbox the driver must run privileged")
	}
	e2 := newCEnv(t)
	e2.eng.info.Runtimes = []string{"runc", "sysbox-runc"}
	d2 := e2.driver(t)
	if d2.Privileged() || d2.runtime != SysboxRuntime {
		t.Fatal("sysbox listed: must use it, unprivileged")
	}
}

func TestNewErrors(t *testing.T) {
	e := newCEnv(t)
	e.eng.infoErr = errors.New("connection refused")
	if _, err := New(context.Background(), e.cfg, e.opts()); err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Fatalf("got %v", err)
	}
	e = newCEnv(t)
	e.eng.imageErr = errors.New("no such image")
	if _, err := New(context.Background(), e.cfg, e.opts()); err == nil {
		t.Fatal("missing image accepted")
	}
	e = newCEnv(t)
	e.eng.info.OSType = "windows"
	if _, err := New(context.Background(), e.cfg, e.opts()); err == nil {
		t.Fatal("windows daemon accepted")
	}
	e = newCEnv(t)
	e.cfg.Executable = "/bin/sh"
	if _, err := New(context.Background(), e.cfg, e.opts()); err == nil {
		t.Skip("/bin/sh is static")
	}
}

func TestStartRunStop(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "job-7", CPUs: 3, MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	out, code := stub(t, conn)
	if out != "hello from fake container\n" || code != 7 {
		t.Fatalf("out=%q code=%d", out, code)
	}

	// What was asked of Docker.
	e.eng.mu.Lock()
	var spec containerSpec
	for _, s := range e.eng.containers {
		spec = s
	}
	var netSpec networkSpec
	for _, n := range e.eng.networks {
		netSpec = n
	}
	e.eng.mu.Unlock()
	if !spec.Privileged || spec.Runtime != "" {
		t.Errorf("privileged fallback expected: %+v", spec)
	}
	if spec.Labels[sandbox.LabelJob] != "job-7" || netSpec.Labels[sandbox.LabelJob] != "job-7" {
		t.Errorf("every object must carry the job label: %v / %v", spec.Labels, netSpec.Labels)
	}
	if spec.MemoryMiB != 2048 || spec.CPUs != 3 {
		t.Errorf("resources: %+v", spec)
	}
	if !slices.Equal(spec.Entrypoint, []string{"/opt/ermite/ermite-runner", "init"}) {
		t.Errorf("entrypoint: %v", spec.Entrypoint)
	}
	var roExe, runDir bool
	for _, b := range spec.Binds {
		if b.Target == "/opt/ermite/ermite-runner" && b.ReadOnly && b.Source == e.cfg.Executable {
			roExe = true
		}
		if b.Target == sandbox.ContainerRunDir && !b.ReadOnly {
			runDir = true
		}
	}
	if !roExe || !runDir {
		t.Errorf("binds: %+v", spec.Binds)
	}
	var scratch, work, tmp bool
	for _, b := range spec.Binds {
		switch b.Target {
		case sandbox.GuestScratch:
			scratch = strings.HasSuffix(b.Source, "/scratch") && !b.ReadOnly
		case "/home/runner/work":
			work = strings.HasSuffix(b.Source, "/scratch/work")
		case "/tmp":
			tmp = strings.HasSuffix(b.Source, "/scratch/tmp")
		}
	}
	if !scratch || !work || !tmp {
		t.Errorf("scratch, workspace and /tmp must come from the loop-mounted disk: %+v", spec.Binds)
	}
	if spec.DNS.String() != "10.200.0.1" || spec.IPv4.String() != "10.200.0.2" || netSpec.Bridge != "erbr0" || netSpec.Subnet.String() != "10.200.0.0/30" {
		t.Errorf("network: %+v %+v", spec, netSpec)
	}
	if !slices.Contains(spec.Env, "ERMITE_CONNECT=unix:/run/ermite/exec.sock") {
		t.Errorf("env: %v", spec.Env)
	}
	if e.plumb.Live() != 1 {
		t.Fatal("firewall policy not attached")
	}

	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatalf("Stop must be idempotent: %v", err)
	}
	if d.slots.InUse() != 0 {
		t.Fatal("slot not released")
	}
}

func TestSysboxJobIsNotPrivileged(t *testing.T) {
	e := newCEnv(t)
	e.eng.info.Runtimes = []string{"sysbox-runc", "runc"}
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	e.eng.mu.Lock()
	for _, s := range e.eng.containers {
		if s.Privileged || s.Runtime != "sysbox-runc" {
			t.Errorf("sysbox job: %+v", s)
		}
	}
	e.eng.mu.Unlock()
	_ = sb.Stop(context.Background())
}

func TestStartFailuresCleanEverything(t *testing.T) {
	cases := map[string]func(*cenv){
		"container cannot start": func(e *cenv) { e.eng.failStart = errors.New("oci runtime error") },
		"container exits early":  func(e *cenv) { e.eng.exitOnRun = true },
		"executor never connects": func(e *cenv) {
			e.eng.noConnect = true
		},
		"firewall attach fails": func(e *cenv) { e.plumb.FailNext = errors.New("nft broke") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newCEnv(t)
			setup(e)
			d, err := New(context.Background(), e.cfg, withTimeout(e.opts(), 300*time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Close() }()
			sb, _ := d.New()
			_, err = sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
			if err == nil {
				t.Fatal("Start succeeded")
			}
			if left := e.leftovers(); len(left) != 0 {
				t.Fatalf("leftovers: %v (err %v)", left, err)
			}
			if d.slots.InUse() != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}

func TestFailedStartIncludesContainerLogs(t *testing.T) {
	e := newCEnv(t)
	e.eng.exitOnRun = true
	d := e.driver(t)
	sb, _ := d.New()
	_, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err == nil || !strings.Contains(err.Error(), "fake container log line") {
		t.Fatalf("got %v", err)
	}
}

func TestCleanupFailureKeepsSlotAndIsRetriedByReap(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	e.eng.removeErr = errors.New("daemon busy")
	if err := sb.Stop(context.Background()); err == nil {
		t.Fatal("Stop must report the failed removal")
	}
	if d.slots.InUse() != 1 {
		t.Fatal("the slot must stay taken while its bridge and subnet still exist")
	}
	e.eng.removeErr = nil
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("Reap left %v", left)
	}
}

func TestReapRemovesLabelledObjectsOnly(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	// Leftovers of a crashed run, and an unrelated user's network.
	_, _ = e.eng.CreateContainer(ctx, containerSpec{Name: "old", Labels: map[string]string{sandbox.LabelJob: "x"}})
	_, _ = e.eng.CreateNetwork(ctx, networkSpec{Name: "old", Labels: map[string]string{sandbox.LabelJob: "x"}})
	_, _ = e.eng.CreateNetwork(ctx, networkSpec{Name: "mine", Labels: map[string]string{"app": "other"}})
	_ = os.MkdirAll(filepath.Join(e.cfg.StateDir, "containers", "er00112233"), 0o700)
	_ = os.MkdirAll(filepath.Join(e.cfg.StateDir, "containers", "keep-me"), 0o700)

	if err := d.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	left := e.leftovers()
	slices.Sort(left)
	if len(left) != 2 || !strings.HasPrefix(left[0], "dir keep-me") || !strings.HasPrefix(left[1], "network") {
		t.Fatalf("only the unrelated network and directory may remain: %v", left)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "containers", "keep-me")); err != nil {
		t.Fatal("Reap removed a directory that is not ours")
	}
	if e.plumb.Resets != 1 {
		t.Fatal("nftables table not reset")
	}
}

func TestCapacityAndSlotIsolation(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	a, _ := d.New()
	b, _ := d.New()
	if _, err := d.New(); !errors.Is(err, sandbox.ErrCapacity) {
		t.Fatalf("got %v", err)
	}
	for _, sb := range []sandbox.Sandbox{a, b} {
		conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
		if err != nil {
			t.Fatal(err)
		}
		stub(t, conn)
	}
	ifaces := map[string]bool{}
	for iface := range e.plumb.Attached {
		ifaces[iface] = true
	}
	if !ifaces["erbr0"] || !ifaces["erbr1"] {
		t.Fatalf("each job needs its own bridge: %v", ifaces)
	}
	_ = a.Stop(context.Background())
	_ = b.Stop(context.Background())
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestStopWithCancelledContext(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sb.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

func TestDoubleStartRefused(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	if _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"}); err == nil {
		t.Fatal("second Start accepted")
	}
	_ = sb.Stop(context.Background())
}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func logger(buf *bytes.Buffer) context.Context {
	return zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: buf}))
}

// S4: the privileged fallback is allowed by default, warned about at the start
// of EVERY job, reported as the effective isolation, and refused on demand.
func TestPrivilegedWarnedAtEveryJobAndReportedAsIsolation(t *testing.T) {
	e := newCEnv(t)
	var buf bytes.Buffer
	ctx := logger(&buf)
	d, err := New(ctx, e.cfg, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Isolation() != sandbox.IsolationContainerPrivileged || d.Mode() != sandbox.Container {
		t.Fatalf("isolation %q", d.Isolation())
	}
	const msg = "privileged container: host-root equivalent, network policy best effort"
	if n := strings.Count(buf.String(), msg); n != 1 {
		t.Fatalf("one warning at driver start, got %d", n)
	}
	for i := range 3 {
		sb, _ := d.New()
		conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: fmt.Sprint("job", i)})
		if err != nil {
			t.Fatal(err)
		}
		stub(t, conn)
		_ = sb.Stop(ctx)
	}
	if n := strings.Count(buf.String(), msg); n != 4 {
		t.Fatalf("the warning must be logged at every job start (1 + 3), got %d:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), `"job":"job2"`) {
		t.Fatal("the warning must name the job")
	}
}

func TestSysboxIsReportedAndSilent(t *testing.T) {
	e := newCEnv(t)
	e.eng.info.Runtimes = []string{"runc", "sysbox-runc"}
	var buf bytes.Buffer
	ctx := logger(&buf)
	d, err := New(ctx, e.cfg, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Isolation() != sandbox.IsolationContainerSysbox {
		t.Fatalf("isolation %q", d.Isolation())
	}
	sb, _ := d.New()
	conn, _ := sb.Start(ctx, sandbox.JobSpec{JobID: "j"})
	stub(t, conn)
	_ = sb.Stop(ctx)
	if strings.Contains(buf.String(), "host-root equivalent") {
		t.Fatal("no privileged warning with sysbox")
	}
}

func TestDenyPrivileged(t *testing.T) {
	e := newCEnv(t)
	e.cfg.DenyPrivileged = true
	_, err := New(context.Background(), e.cfg, e.opts())
	if err == nil || !strings.Contains(err.Error(), "ERMITE_RUNNER_DENY_PRIVILEGED") || !strings.Contains(err.Error(), "sysbox") {
		t.Fatalf("got %v", err)
	}
	// The host lock must be free again after a refused start.
	if l, err := sandbox.AcquireHostLock(e.cfg.LockFile); err != nil {
		t.Fatalf("a failed New must release the host lock: %v", err)
	} else {
		_ = l.Release()
	}
	// With sysbox available the setting does nothing.
	e2 := newCEnv(t)
	e2.cfg.DenyPrivileged = true
	e2.eng.info.Runtimes = []string{"sysbox-runc"}
	d, err := New(context.Background(), e2.cfg, e2.opts())
	if err != nil {
		t.Fatalf("deny-privileged with sysbox must work: %v", err)
	}
	_ = d.Close()
}

// S5: pids limit from the config and no swap.
func TestHostConfigPidsAndSwap(t *testing.T) {
	hc := hostConfigFor(containerSpec{Network: "n", MemoryMiB: 2048, CPUs: 3, Pids: 4096, DNS: netip.MustParseAddr("10.200.0.1")})
	if hc.Memory != 2048<<20 || hc.MemorySwap != hc.Memory {
		t.Errorf("memory %d swap %d: swap must equal memory (no swap)", hc.Memory, hc.MemorySwap)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != 4096 {
		t.Errorf("pids limit %v", hc.PidsLimit)
	}
	if hc.NanoCPUs != 3_000_000_000 {
		t.Errorf("cpus %d", hc.NanoCPUs)
	}
	if hc.Sysctls["net.ipv6.conf.all.disable_ipv6"] != "1" {
		t.Error("IPv6 must be disabled in the container")
	}
	if hostConfigFor(containerSpec{Network: "n"}).PidsLimit != nil {
		t.Error("no pids limit configured: leave it unset")
	}
}

func TestJobPidsFlowsFromConfig(t *testing.T) {
	e := newCEnv(t)
	e.cfg.JobPids = 777
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	defer func() { _ = sb.Stop(context.Background()) }()
	e.eng.mu.Lock()
	defer e.eng.mu.Unlock()
	for _, s := range e.eng.containers {
		if s.Pids != 777 {
			t.Fatalf("spec pids %d", s.Pids)
		}
	}
}

// C4: the disk is enforced by a loop-mounted ext4 file of ERMITE_RUNNER_JOB_DISK,
// mounted while the job runs and unmounted (before the files are removed) after.
func TestScratchDiskIsLoopMountedAndSized(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j", DiskMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	if e.mounts.count() != 1 {
		t.Fatalf("mounts: %d", e.mounts.count())
	}
	var mkfs, mount string
	for _, l := range e.run.Lines() {
		if strings.HasPrefix(l, "mkfs.ext4") && strings.Contains(l, "scratch.ext4") {
			mkfs = l
		}
		if strings.HasPrefix(l, "mount ") {
			mount = l
		}
	}
	if !strings.Contains(mkfs, "ermite-scratch") || !strings.Contains(mount, "loop") || !strings.Contains(mount, "scratch.ext4") {
		t.Fatalf("mkfs=%q mount=%q", mkfs, mount)
	}
	// The scratch file has the configured size (sparse) and is not visible to the job.
	matches, _ := filepath.Glob(filepath.Join(e.cfg.StateDir, "containers", sb.ID(), "scratch.ext4"))
	if len(matches) != 1 {
		t.Fatal("no scratch file")
	}
	if st, _ := os.Stat(matches[0]); st.Size() != 2048<<20 {
		t.Fatalf("size %d", st.Size())
	}
	e.eng.mu.Lock()
	for _, s := range e.eng.containers {
		for _, b := range s.Binds {
			if strings.HasSuffix(b.Source, "scratch.ext4") || b.Source == filepath.Join(e.cfg.StateDir, "containers", sb.ID()) {
				t.Errorf("the job must not see the disk file or the whole run directory: %+v", b)
			}
		}
	}
	e.eng.mu.Unlock()
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Order: umount comes before the directory removal (nothing below a mount is deleted).
	lines := e.run.Lines()
	if lines[len(lines)-1] != "umount "+filepath.Join(e.cfg.StateDir, "containers", sb.ID(), "scratch") {
		t.Fatalf("last command: %s", lines[len(lines)-1])
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// C5: when the container cannot be removed it may still run: keep the firewall
// policy, the bridge, the mount and the files; report ErrLeaked; keep the slot;
// let Reap finish.
func TestStopKeepsPolicyWhenTheContainerMayStillRun(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	e.eng.removeErr = errors.New("daemon busy")
	err = sb.Stop(context.Background())
	if !errors.Is(err, sandbox.ErrLeaked) {
		t.Fatalf("want ErrLeaked, got %v", err)
	}
	if e.plumb.Live() != 1 {
		t.Fatal("the firewall policy must stay while the container may run")
	}
	e.eng.mu.Lock()
	nets := len(e.eng.networks)
	e.eng.mu.Unlock()
	if nets != 1 || e.mounts.count() != 1 {
		t.Fatalf("the bridge and the scratch mount must stay: nets=%d mounts=%d", nets, e.mounts.count())
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "containers", sb.ID(), "scratch.ext4")); err != nil {
		t.Fatal("the scratch file must stay")
	}
	if err := sb.Stop(context.Background()); !errors.Is(err, sandbox.ErrLeaked) {
		t.Fatalf("second Stop: %v", err)
	}
	// C8: the slot is taken, a new sandbox gets another slot, Reap is busy while it runs.
	other, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Reap(context.Background()); !errors.Is(err, sandbox.ErrBusy) {
		t.Fatalf("Reap while a sandbox exists: %v", err)
	}
	_ = other.Stop(context.Background())
	e.eng.removeErr = nil
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers after Reap: %v", left)
	}
	if d.slots.InUse() != 0 {
		t.Fatal("Reap must free the leaked slot")
	}
}

// A scratch disk that cannot be unmounted keeps its files: nothing is deleted
// below a live mount.
func TestStopNeverDeletesBelowALiveMount(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	e.mounts.failUm = errors.New("target is busy")
	if err := sb.Stop(context.Background()); !errors.Is(err, sandbox.ErrLeaked) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "containers", sb.ID(), "scratch.ext4")); err != nil {
		t.Fatal("files below a mount that is still there were deleted")
	}
	e.mounts.failUm = nil
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.mounts.count() != 0 {
		t.Fatal("Reap must unmount leftover scratch disks")
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// C7: Stop aborts a boot in progress instead of waiting for the timeout.
func TestStopAbortsABootInProgress(t *testing.T) {
	e := newCEnv(t)
	e.eng.noConnect = true
	o := e.opts()
	o.ConnectTimeout = time.Minute
	d, err := New(context.Background(), e.cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	sb, _ := d.New()
	started := make(chan error, 1)
	go func() { _, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"}); started <- err }()
	time.Sleep(500 * time.Millisecond)
	t0 := time.Now()
	if err := sb.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if !errors.Is(err, sandbox.ErrAborted) {
			t.Fatalf("Start must report the abort, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start was not aborted")
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("Stop took %v", time.Since(t0))
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// C16: ID names the Docker objects.
func TestSandboxIDNamesDockerObjects(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	if !sandbox.ValidID(sb.ID()) {
		t.Fatal(sb.ID())
	}
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	defer func() { _ = sb.Stop(context.Background()) }()
	e.eng.mu.Lock()
	defer e.eng.mu.Unlock()
	for _, s := range e.eng.containers {
		if s.Name != "ermite-"+sb.ID() {
			t.Fatalf("container name %q", s.Name)
		}
	}
}

// S3: a second runner on the same host is refused whatever its state directory.
func TestHostLockAcrossStateDirs(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	_ = d
	e2 := newCEnv(t)
	e2.cfg.LockFile = e.cfg.LockFile // same host, another state directory
	_, err := New(context.Background(), e2.cfg, e2.opts())
	if err == nil || !strings.Contains(err.Error(), "another ermite-runner is running on this host") {
		t.Fatalf("got %v", err)
	}
}

// C10: a cancelled wait is reported as cancelled, never as a clean exit.
func TestAwaitExit(t *testing.T) {
	res := make(chan container.WaitResponse, 1)
	errc := make(chan error, 1)
	res <- container.WaitResponse{StatusCode: 3}
	if r := <-awaitExit(context.Background(), res, errc); r.Code != 3 || r.Err != nil {
		t.Fatalf("%+v", r)
	}
	errc <- errors.New("daemon gone")
	if r := <-awaitExit(context.Background(), make(chan container.WaitResponse), errc); r.Code != -1 || r.Err == nil {
		t.Fatalf("%+v", r)
	}
	// Both ready and the context already cancelled: the cancellation wins.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		res2 := make(chan container.WaitResponse, 1)
		res2 <- container.WaitResponse{StatusCode: 0}
		if r := <-awaitExit(ctx, res2, make(chan error)); r.Err == nil || !errors.Is(r.Err, context.Canceled) || r.Code != -1 {
			t.Fatalf("a cancelled wait must not look like a clean exit: %+v", r)
		}
	}
}

func TestFreeSpaceWarningInContainerMode(t *testing.T) {
	e := newCEnv(t)
	var buf bytes.Buffer
	o := e.opts()
	o.FreeSpace = func(string) (int64, error) { return 1, nil }
	d, err := New(logger(&buf), e.cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if !strings.Contains(buf.String(), "less free space") {
		t.Fatalf("no warning: %s", buf.String())
	}
}

func TestMkfsTooOldRefusedAtStart(t *testing.T) {
	e := newCEnv(t)
	run := &cmdrun.Fake{Handle: func(c cmdrun.Call) ([]byte, error) {
		if c.Name == "mkfs.ext4" {
			return []byte("mke2fs 1.42.9 (4-Feb-2014)\n"), nil
		}
		return nil, nil
	}}
	o := e.opts()
	o.Run = run
	if _, err := New(context.Background(), e.cfg, o); err == nil || !strings.Contains(err.Error(), "1.43") {
		t.Fatalf("got %v", err)
	}
}

func TestSocketPathTooLongIsExplained(t *testing.T) {
	e := newCEnv(t)
	e.cfg.StateDir = filepath.Join(e.cfg.StateDir, strings.Repeat("d", 100))
	d := e.driver(t)
	sb, _ := d.New()
	_, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err == nil || !strings.Contains(err.Error(), "108-byte limit") && !strings.Contains(err.Error(), "limit of a Unix socket") {
		t.Fatalf("got %v", err)
	}
	if left := e.leftovers(); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}

// GO-002: only a podman daemon gets a DNS listener that may precede the gateway
// address; Docker keeps a normal bind.
func TestOnlyPodmanGetsAFreeBindDNSListener(t *testing.T) {
	if newNet(nil, 0, false, "", "").DNS.FreeBind {
		t.Fatal("Docker must keep a normal bind")
	}
	if !newNet(nil, 0, true, "", "").DNS.FreeBind {
		t.Fatal("podman needs the listener before the bridge address")
	}
}

// Guest forge access: the exception reaches the policy of every guest, and the
// warning naming the origin is logged at the driver start and at every job.
func TestGuestForgeIsAttachedAndWarnedAtEveryJob(t *testing.T) {
	e := newCEnv(t)
	var err error
	if e.cfg.GuestForge, err = sandbox.ParseGuestForge("http://ermite.bnema.dev:8088", true); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ctx := logger(&buf)
	d, err := New(ctx, e.cfg, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	msg := "guest forge access: sandboxes may reach http://ermite.bnema.dev:8088 on the host"
	if n := strings.Count(buf.String(), msg); n != 1 {
		t.Fatalf("one warning at driver start, got %d", n)
	}
	for i := range 2 {
		sb, _ := d.New()
		conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: fmt.Sprint("job", i)})
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range e.plumb.Attached {
			if g.ForgePort != 8088 {
				t.Errorf("guest attached with ForgePort %d", g.ForgePort)
			}
		}
		stub(t, conn)
		_ = sb.Stop(ctx)
	}
	if n := strings.Count(buf.String(), msg); n != 3 {
		t.Fatalf("the warning must be logged at every job start (1 + 2), got %d", n)
	}
}

func TestNoGuestForgeByDefault(t *testing.T) {
	e := newCEnv(t)
	var buf bytes.Buffer
	ctx := logger(&buf)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range e.plumb.Attached {
		if g.ForgePort != 0 {
			t.Errorf("ForgePort %d without the setting", g.ForgePort)
		}
	}
	stub(t, conn)
	_ = sb.Stop(ctx)
	if strings.Contains(buf.String(), "guest forge") {
		t.Fatal("a guest forge warning without the setting")
	}
	if n := newNet(nil, 0, false, "ermite.example.org", "").DNS.Override; n != "ermite.example.org" {
		t.Fatalf("override %q", n)
	}
}

// The host's external tools are bind-mounted read-only at
// /home/runner/externals/<name> (GitHub's layout), whichever the isolation.
func TestExternalsAreBoundReadOnly(t *testing.T) {
	e := newCEnv(t)
	src := &sandboxtest.Externals{}
	n24 := sandboxtest.MakeExternal(t, "node24", "a", "v24")
	n20 := sandboxtest.MakeExternal(t, "node20", "b", "v20")
	gone := sandbox.External{Name: "node24_alpine", ID: "c", Dir: "/state/externals/.unavailable/x", Unavailable: true}
	src.Set(n24, n20, gone)
	e.cfg.Externals = src
	d := e.driver(t)
	run := func() containerSpec {
		t.Helper()
		sb, _ := d.New()
		conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
		if err != nil {
			t.Fatal(err)
		}
		stub(t, conn)
		e.eng.mu.Lock()
		var spec containerSpec
		for _, s := range e.eng.containers {
			spec = s
		}
		e.eng.mu.Unlock()
		_ = sb.Stop(context.Background())
		return spec
	}
	spec := run()
	got := map[string]bindMount{}
	for _, b := range spec.Binds {
		got[b.Target] = b
	}
	for _, w := range []sandbox.External{n24, n20, gone} {
		b, ok := got["/home/runner/externals/"+w.Name]
		if !ok || !b.ReadOnly || b.Source != w.Dir {
			t.Errorf("%s: %+v (binds %+v)", w.Name, b, spec.Binds)
		}
	}
	// The executable and the other binds are untouched.
	if b := got["/opt/ermite/ermite-runner"]; !b.ReadOnly || b.Source != e.cfg.Executable {
		t.Errorf("executable bind: %+v", b)
	}
	if len(spec.Binds) != 5+3 {
		t.Errorf("binds: %+v", spec.Binds)
	}

	// A new Node.js is picked up by the next job, without a driver restart.
	src.Set(sandboxtest.MakeExternal(t, "node24", "d", "v24.22"))
	spec = run()
	n := 0
	for _, b := range spec.Binds {
		if strings.HasPrefix(b.Target, "/home/runner/externals/") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("externals binds after the change: %+v", spec.Binds)
	}
}

func TestNoExternalsNoBinds(t *testing.T) {
	e := newCEnv(t)
	d := e.driver(t)
	sb, _ := d.New()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	stub(t, conn)
	defer func() { _ = sb.Stop(context.Background()) }()
	e.eng.mu.Lock()
	defer e.eng.mu.Unlock()
	for _, s := range e.eng.containers {
		for _, b := range s.Binds {
			if strings.Contains(b.Target, "externals") {
				t.Errorf("unexpected bind %+v", b)
			}
		}
	}
}
