// SPDX-License-Identifier: MIT

// Package vm is the micro-VM sandbox: one Firecracker VM per job.
//
// Per VM, on the host: a jailer chroot (or, without jailer, a plain directory)
// holding the per-job sparse scratch disk and the vsock socket; a TAP device
// with a /30; rules in the dedicated nftables table; the DNS forwarder bound on
// the TAP's host address. In the guest: three virtio disks (read-only runner
// executable as the boot root, read-only shared base image, per-job scratch)
// and a vsock device. Everything is traced in a state-directory record before it
// is created, so that Reap can remove it after a crash.
package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
	"bnema.dev/ermite-runner/internal/sandbox/netsetup"
)

// Options are the testable knobs of the driver.
type Options struct {
	Run cmdrun.Runner
	// ProcDir is the /proc used to find orphan VMM processes.
	ProcDir string
	// CgroupRoot is the cgroup v2 mount, where the jailer creates one cgroup per
	// VM (default /sys/fs/cgroup).
	CgroupRoot string
	// BootTimeout bounds boot + init + executor connection (default 90 s).
	BootTimeout time.Duration
	// Net overrides the network plumbing (tests).
	Net netsetup.Plumbing
	// FirecrackerArgs are extra arguments for Firecracker (tests use them to
	// select the fake).
	FirecrackerArgs []string
	// FreeSpace overrides statfs for the free-space warning (tests).
	FreeSpace func(string) (int64, error)
	// kill overrides killAndWait (tests simulate a VMM that cannot be killed).
	kill func(pid int, timeout time.Duration) bool
}

// vmmOverheadMiB is what the Firecracker process itself may use on top of the
// guest memory (device emulation, the API server, the seccomp filter, stacks):
// it is added to memory.max so that the guest, not the VMM, is what gets limited.
const vmmOverheadMiB = 256

// Driver implements sandbox.Driver for micro-VMs.
type Driver struct {
	cfg      sandbox.Config
	opt      Options
	fcPath   string
	jailer   string
	execDisk string
	store    store
	net      netsetup.Plumbing
	lock     *sandbox.HostLock

	mu    sync.Mutex
	slots *sandbox.Slots
}

// New builds the driver: takes the host-wide lock, resolves binaries, builds the
// exec disk and enables IPv4 forwarding (a host-wide sysctl that stays on: see
// the README). Run Reap before New sandboxes.
func New(ctx context.Context, cfg sandbox.Config, opt Options) (*Driver, error) {
	if opt.Run == nil {
		opt.Run = cmdrun.Exec{}
	}
	if opt.ProcDir == "" {
		opt.ProcDir = "/proc"
	}
	if opt.CgroupRoot == "" {
		opt.CgroupRoot = "/sys/fs/cgroup"
	}
	if opt.BootTimeout == 0 {
		opt.BootTimeout = 90 * time.Second
	}
	if opt.kill == nil {
		opt.kill = killAndWait
	}
	d := &Driver{cfg: cfg, opt: opt, slots: sandbox.NewSlots(cfg.Capacity), store: store{dir: filepath.Join(cfg.StateDir, "vms")}}

	var err error
	if d.fcPath, err = resolveBin(cfg.Firecracker, "firecracker"); err != nil {
		return nil, err
	}
	if !cfg.JailerOff {
		if d.jailer, err = resolveBin(cfg.Jailer, "jailer"); err != nil {
			return nil, err
		}
	}
	exe, err := cfg.ExecutablePath()
	if err != nil {
		return nil, err
	}
	if err := sandbox.CheckStatic(exe); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	if d.lock, err = sandbox.AcquireHostLock(cfg.LockFile); err != nil {
		return nil, err
	}
	fail := func(err error) (*Driver, error) { _ = d.Close(); return nil, err }

	if !cfg.JailerOff {
		for _, p := range []string{cfg.KernelPath(), cfg.BaseImagePath()} {
			if err := checkWorldReadable(p); err != nil {
				return fail(err)
			}
		}
	}
	sandbox.WarnIfLowSpace(ctx, cfg.StateDir, cfg.Capacity, cfg.JobDiskMiB, opt.FreeSpace)
	if err := disk.CheckMkfs(ctx, opt.Run); err != nil {
		return fail(err)
	}
	if d.execDisk, err = ensureExecDisk(ctx, opt.Run, exe, cfg.StateDir); err != nil {
		return fail(err)
	}
	d.net = opt.Net
	if d.net == nil {
		d.net = netsetup.New(opt.Run, cfg.DNSPort, "")
	}
	if err := d.net.EnableForwarding(); err != nil {
		return fail(err)
	}
	return d, nil
}

func resolveBin(configured, name string) (string, error) {
	p := configured
	if p == "" {
		var err error
		if p, err = exec.LookPath(name); err != nil {
			return "", fmt.Errorf("%s not found in PATH (set ERMITE_RUNNER_%s)", name, map[string]string{"firecracker": "FIRECRACKER", "jailer": "JAILER"}[name])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return abs, nil
}

// Mode implements sandbox.Driver.
func (d *Driver) Mode() sandbox.Mode { return sandbox.VM }

// Isolation implements sandbox.Driver.
func (d *Driver) Isolation() sandbox.Isolation { return sandbox.IsolationVM }

// Close releases the host lock and the DNS listeners.
func (d *Driver) Close() error {
	if d.net != nil {
		d.net.Close()
	}
	return d.lock.Release()
}

// New implements sandbox.Driver.
func (d *Driver) New() (sandbox.Sandbox, error) {
	id, err := sandbox.NewID()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	slot, err := d.slots.Acquire()
	if err != nil {
		return nil, err
	}
	return &vmSandbox{d: d, slot: slot, id: id}, nil
}

func (d *Driver) release(slot int) {
	d.mu.Lock()
	d.slots.Release(slot)
	d.mu.Unlock()
}

func (d *Driver) markLeaked(slot int) {
	d.mu.Lock()
	d.slots.MarkLeaked(slot)
	d.mu.Unlock()
}

// vmSandbox is one VM, single use.
type vmSandbox struct {
	d    *Driver
	slot int
	id   string
	gate sandbox.BootGate

	mu      sync.Mutex
	started bool
	stopped bool
	// leakErr is the error of a cleanup that left something behind; later Stop
	// calls return it again.
	leakErr error

	rec      record
	lay      layout
	cmd      *exec.Cmd
	exited   chan struct{}
	console  *ring
	listener net.Listener
	conn     net.Conn
	tap      string
	gateway  netip.Addr
	attached bool
}

// ID implements sandbox.Sandbox.
func (s *vmSandbox) ID() string { return s.id }

func (s *vmSandbox) uid() int { return s.d.cfg.UIDBase + s.slot }

// Start implements sandbox.Sandbox.
func (s *vmSandbox) Start(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	// A Stop from another goroutine aborts the boot through the gate.
	bootCtx, err := s.gate.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("vm sandbox: %w", err)
	}
	defer s.gate.End()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return nil, errors.New("vm sandbox: Start called twice or after Stop")
	}
	s.started = true
	log := zerowrap.FromCtx(ctx)
	d := s.d
	spec = d.cfg.Resolve(spec)

	jailed := !d.cfg.JailerOff
	runUID := s.uid()
	if !jailed {
		runUID = os.Getuid()
	}
	s.lay = newLayout(d.cfg.StateDir, filepath.Base(d.fcPath), s.id, jailed, runUID)
	s.rec = record{ID: s.id, Slot: s.slot, Job: spec.JobID, Dir: s.lay.dir}
	// The record exists before the first resource, so any crash from here on is
	// reapable.
	if err := d.store.save(s.rec); err != nil {
		s.releaseLocked()
		return nil, fmt.Errorf("write state record: %w", err)
	}

	conn, err := s.boot(bootCtx, spec)
	if err != nil {
		console := ""
		if s.console != nil {
			console = s.console.String()
		}
		cerr := s.cleanupLocked(context.WithoutCancel(ctx))
		if console != "" {
			err = fmt.Errorf("%w\nVMM console tail:\n%s", err, console)
		}
		return nil, errors.Join(err, cerr)
	}
	log.Info().Str("vm", s.id).Int("slot", s.slot).Str("job", spec.JobID).Msg("micro-VM running")
	return conn, nil
}

// boot runs the stages in order; each stage records what it created so that
// cleanupLocked can undo exactly that.
func (s *vmSandbox) boot(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	netw, err := netpolicy.Subnet(s.d.cfg.Network, s.slot)
	if err != nil {
		return nil, err
	}
	if err := s.bootNetwork(ctx, netw); err != nil {
		return nil, err
	}
	api, err := s.bootVMM(ctx, spec)
	if err != nil {
		return nil, err
	}
	files, err := s.bootFiles(ctx, spec)
	if err != nil {
		return nil, err
	}
	accepted, err := s.bootVsock()
	if err != nil {
		return nil, err
	}
	if err := api.configure(ctx, s.vmConfig(spec, netw, files)); err != nil {
		return nil, err
	}
	return s.waitExecutor(ctx, accepted)
}

// bootNetwork creates the TAP (it must exist when Firecracker opens it) and
// installs the firewall policy and the DNS listener.
func (s *vmSandbox) bootNetwork(ctx context.Context, netw netpolicy.Net) error {
	d := s.d
	s.tap, s.gateway = tapName(s.slot), netw.Gateway
	s.rec.Tap, s.rec.Gate = s.tap, s.gateway.String()
	if err := d.store.save(s.rec); err != nil {
		return err
	}
	if err := createTap(ctx, d.opt.Run, s.tap, s.lay.uid, s.gateway); err != nil {
		return err
	}
	s.attached = true // from here Detach is idempotent and always safe
	return d.net.Attach(ctx, netpolicy.Guest{Iface: s.tap, Gateway: netw.Gateway, Addr: netw.Guest})
}

// bootVMM starts the jailer (or Firecracker) and waits for its API.
func (s *vmSandbox) bootVMM(ctx context.Context, spec sandbox.JobSpec) (*apiClient, error) {
	apiSock := s.lay.host("api.sock")
	if s.lay.jailed {
		apiSock = s.lay.host("run/firecracker.socket")
	} else if err := os.MkdirAll(s.lay.root, 0o700); err != nil {
		return nil, err
	}
	if err := s.startVMM(spec, apiSock); err != nil {
		return nil, err
	}
	api := newAPIClient(apiSock)
	if err := s.waitAPI(ctx, api, apiSock); err != nil {
		return nil, err
	}
	return api, nil
}

// vmFiles are the paths Firecracker is given.
type vmFiles struct{ execDisk, baseDisk, kernel string }

// bootFiles makes the shared files available and creates the scratch disk.
func (s *vmSandbox) bootFiles(ctx context.Context, spec sandbox.JobSpec) (vmFiles, error) {
	d := s.d
	var f vmFiles
	var err error
	if f.execDisk, err = s.lay.share(d.execDisk, "exec.ext4"); err != nil {
		return f, err
	}
	if f.baseDisk, err = s.lay.share(d.cfg.BaseImagePath(), "base.ext4"); err != nil {
		return f, err
	}
	if f.kernel, err = s.lay.share(d.cfg.KernelPath(), "kernel"); err != nil {
		return f, err
	}
	owner := -1
	if s.lay.jailed {
		owner = s.lay.uid
	}
	return f, disk.CreateScratch(ctx, d.opt.Run, s.lay.host("scratch.ext4"), spec.DiskMiB, owner)
}

// bootVsock listens on the host side of the vsock stream: Firecracker connects
// to <uds_path>_<port> when the guest connects to the host CID.
func (s *vmSandbox) bootVsock() (<-chan acceptResult, error) {
	path := s.lay.host(udsName) + fmt.Sprintf("_%d", sandbox.VsockPort)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen vsock socket: %w", err)
	}
	s.listener = ln
	if s.lay.jailed {
		if err := os.Lchown(path, s.lay.uid, s.lay.uid); err != nil {
			return nil, err
		}
	}
	accepted := make(chan acceptResult, 1)
	go func() { c, err := ln.Accept(); accepted <- acceptResult{c, err} }()
	return accepted, nil
}

const udsName = "v.sock"

func (s *vmSandbox) vmConfig(spec sandbox.JobSpec, netw netpolicy.Net, f vmFiles) vmConfig {
	cfg := s.d.cfg
	return vmConfig{
		Machine: machineConfig{VcpuCount: spec.CPUs, MemSizeMiB: spec.MemoryMiB},
		Boot:    bootSource{KernelImagePath: f.kernel, BootArgs: bootArgs(netw.Guest, netw.Gateway, time.Now().Unix())},
		Drives: []drive{
			{DriveID: "rootfs", PathOnHost: f.execDisk, IsRootDevice: true, IsReadOnly: true},
			{DriveID: "base", PathOnHost: f.baseDisk, IsReadOnly: true},
			{DriveID: "scratch", PathOnHost: s.lay.fc("scratch.ext4"), RateLimiter: diskLimiter(cfg.DiskBandwidthMiB, cfg.DiskIOPS)},
		},
		Net: networkInterface{
			IfaceID: "eth0", HostDevName: s.tap, GuestMAC: macFor(s.slot),
			RxRateLimiter: netLimiter(cfg.NetMbit), TxRateLimiter: netLimiter(cfg.NetMbit),
		},
		Vsock: vsockDevice{GuestCID: 3, UDSPath: s.lay.fc(udsName)},
	}
}

// waitExecutor waits for the guest's executor to connect. Only ctx (the caller's
// context, or Stop through the boot gate) and the boot timeout end the wait: a
// boot cannot be aborted any other way.
func (s *vmSandbox) waitExecutor(ctx context.Context, accepted <-chan acceptResult) (sandbox.Conn, error) {
	timer := time.NewTimer(s.d.opt.BootTimeout)
	defer timer.Stop()
	select {
	case r := <-accepted:
		if r.err != nil {
			return nil, fmt.Errorf("accept executor connection: %w", r.err)
		}
		s.conn = r.c
		return r.c, nil
	case <-s.exited:
		return nil, errors.New("firecracker exited before the executor connected")
	case <-timer.C:
		return nil, fmt.Errorf("executor did not connect within %s", s.d.opt.BootTimeout)
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

type acceptResult struct {
	c   net.Conn
	err error
}

// jailerArgs builds the jailer command line (after the binary name).
//
//   - cgroup v2 with memory.max (guest memory + VMM overhead) and pids.max (a
//     VMM needs a handful of threads per vCPU): a runaway or compromised VMM
//     cannot take the host's memory or fork-bomb it;
//   - a new PID namespace: the VMM cannot see or signal host processes;
//   - no-file: the descriptor budget of the VMM.
func jailerArgs(jailer, fc, id string, uid int, chrootBase string, spec sandbox.JobSpec, extra []string) []string {
	memBytes := (int64(spec.MemoryMiB) + vmmOverheadMiB) << 20
	pids := 32 + 8*spec.CPUs
	args := []string{
		"--id", id, "--exec-file", fc,
		"--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(uid),
		"--chroot-base-dir", chrootBase,
		"--cgroup-version", "2",
		"--cgroup", "memory.max=" + strconv.FormatInt(memBytes, 10),
		"--cgroup", "pids.max=" + strconv.Itoa(pids),
		"--new-pid-ns",
		"--resource-limit", "no-file=4096",
		"--",
	}
	return append(args, extra...)
}

func (s *vmSandbox) startVMM(spec sandbox.JobSpec, apiSock string) error {
	d := s.d
	var cmd *exec.Cmd
	if s.lay.jailed {
		args := jailerArgs(d.jailer, d.fcPath, s.id, s.uid(), jailBase(d.cfg.StateDir), spec, d.opt.FirecrackerArgs)
		cmd = exec.Command(d.jailer, args...)
	} else {
		args := append([]string{"--id", s.id, "--api-sock", apiSock}, d.opt.FirecrackerArgs...)
		cmd = exec.Command(d.fcPath, args...)
	}
	s.console = newRing(16 << 10)
	cmd.Stdout, cmd.Stderr = s.console, s.console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	s.cmd = cmd
	s.exited = make(chan struct{})
	jailed := s.lay.jailed
	go func() {
		_ = cmd.Wait()
		// With --new-pid-ns the jailer may exit as soon as it has started the
		// VMM in its namespace: the VMM is then gone only when its own pid is.
		if jailed {
			s.awaitVMMGone()
		}
		close(s.exited)
	}()
	s.rec.PID = cmd.Process.Pid
	return s.d.store.save(s.rec)
}

// awaitVMMGone blocks until no process of this VM's id is left.
func (s *vmSandbox) awaitVMMGone() {
	for {
		alive := false
		for pid, id := range vmmProcs(s.d.opt.ProcDir) {
			if id == s.id && pidAlive(pid) {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *vmSandbox) waitAPI(ctx context.Context, api *apiClient, sock string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := api.ready(ctx); err == nil {
			return nil
		}
		select {
		case <-s.exited:
			return errors.New("firecracker exited before its API was ready")
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker API socket %s not ready after 10s", sock)
		}
	}
}

// Stop implements sandbox.Sandbox.
func (s *vmSandbox) Stop(ctx context.Context) error {
	// Abort a boot in progress first: Start holds s.mu for its whole duration.
	s.gate.Abort()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return s.leakErr
	}
	if !s.started {
		s.releaseLocked()
		return nil
	}
	return s.cleanupLocked(context.WithoutCancel(ctx))
}

func (s *vmSandbox) releaseLocked() {
	s.stopped = true
	s.d.release(s.slot)
}

// vmmPIDs returns every host process of this VM: the launched command and
// whatever the /proc scan finds with this id.
func (s *vmSandbox) vmmPIDs() []int {
	set := map[int]bool{}
	if s.cmd != nil && s.cmd.Process != nil {
		set[s.cmd.Process.Pid] = true
	}
	for pid, id := range vmmProcs(s.d.opt.ProcDir) {
		if id == s.id {
			set[pid] = true
		}
	}
	var out []int
	for pid := range set {
		out = append(out, pid)
	}
	return out
}

// cleanupLocked removes everything the VM created, in dependency order. Steps
// that need the VMM to be dead (network device, firewall rules, files, cgroup)
// are skipped when it could not be killed: removing the TAP or the policy under
// a VM that may still run would break the isolation it relies on, so the
// sandbox is reported as leaked (sandbox.ErrLeaked), its slot stays taken, and
// Reap finishes the job.
func (s *vmSandbox) cleanupLocked(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
	d := s.d
	var errs []error
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	dead := true
	if s.cmd != nil && s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL) // the whole group
	}
	for _, pid := range s.vmmPIDs() {
		if !d.opt.kill(pid, 5*time.Second) {
			dead = false
			errs = append(errs, fmt.Errorf("vmm process %d did not exit", pid))
		}
	}
	if dead && s.exited != nil {
		select {
		case <-s.exited:
		case <-time.After(5 * time.Second):
			dead = false
			errs = append(errs, errors.New("vmm exit was not observed"))
		}
	}
	if dead {
		if s.attached {
			if err := d.net.Detach(ctx, s.tap, s.gateway); err != nil {
				errs = append(errs, err)
			}
			if err := deleteTap(ctx, d.opt.Run, s.tap); err != nil {
				errs = append(errs, err)
			} else {
				s.attached = false
			}
		}
		if s.lay.dir != "" {
			if err := removeAll(s.lay.dir); err != nil {
				errs = append(errs, err)
			}
		}
		if s.lay.jailed {
			if err := removeCgroup(d.opt.CgroupRoot, filepath.Base(d.fcPath), s.id); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) == 0 {
			if err := d.store.remove(s.id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) == 0 {
		s.releaseLocked()
		return nil
	}
	s.stopped = true
	d.markLeaked(s.slot)
	s.leakErr = fmt.Errorf("%w: vm %s: %w", sandbox.ErrLeaked, s.id, errors.Join(errs...))
	log.Error().Err(s.leakErr).Str("vm", s.id).Msg("micro-VM cleanup incomplete; call Reap to finish it")
	return s.leakErr
}

// removeAll is os.RemoveAll that also reports a directory that is still there.
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s still exists after removal", dir)
	}
	return nil
}

// removeCgroup removes the cgroup the jailer created for a VM
// (<root>/<parent>/<id>, parent = the exec file name). The jailer never removes
// it. A missing cgroup is fine; a non-empty one fails (rmdir semantics).
func removeCgroup(root, parent, id string) error {
	err := os.Remove(filepath.Join(root, parent, id))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove cgroup %s: %w", filepath.Join(root, parent, id), err)
}
