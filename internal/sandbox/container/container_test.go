// SPDX-License-Identifier: MIT

package container

import (
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

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
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
	volumes    map[string]map[string]string
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
		volumes:    map[string]map[string]string{},
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
func (f *fakeEngine) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[name] = labels
	return nil
}
func (f *fakeEngine) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
	return nil
}
func (f *fakeEngine) ListVolumes(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.volumes {
		out = append(out, n)
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
	for n := range f.volumes {
		out = append(out, "volume "+n)
	}
	return out
}

type cenv struct {
	cfg   sandbox.Config
	eng   *fakeEngine
	plumb *sandboxtest.Plumbing
}

func newCEnv(t *testing.T) *cenv {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "runner")
	sandboxtest.WriteStaticELF(t, exe)
	return &cenv{
		eng:   newFake(),
		plumb: sandboxtest.NewPlumbing(),
		cfg: sandbox.Config{
			Isolation: sandbox.Container, StateDir: filepath.Join(dir, "state"), Capacity: 2,
			JobCPUs: 2, JobMemoryMiB: 1024, JobDiskMiB: 1024, UIDBase: 900000,
			Network: netip.MustParsePrefix("10.200.0.0/16"), Executable: exe, DNSPort: 0,
		},
	}
}

func (e *cenv) driver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb, ConnectTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func (e *cenv) leftovers() []string {
	out := e.eng.objects()
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
	_, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb})
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
	if _, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb}); err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Fatalf("got %v", err)
	}
	e = newCEnv(t)
	e.eng.imageErr = errors.New("no such image")
	if _, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb}); err == nil {
		t.Fatal("missing image accepted")
	}
	e = newCEnv(t)
	e.eng.info.OSType = "windows"
	if _, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb}); err == nil {
		t.Fatal("windows daemon accepted")
	}
	e = newCEnv(t)
	e.cfg.Executable = "/bin/sh"
	if _, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb}); err == nil {
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
	if len(spec.Volumes) != 1 || spec.Volumes[0].Target != sandbox.GuestScratch {
		t.Errorf("scratch volume: %+v", spec.Volumes)
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
			d, err := New(context.Background(), e.cfg, Options{engine: e.eng, Net: e.plumb, ConnectTimeout: 300 * time.Millisecond})
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
	_ = e.eng.CreateVolume(ctx, "old", map[string]string{sandbox.LabelJob: "x"})
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
