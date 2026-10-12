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
	"sync"
	"syscall"
	"time"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/netsetup"
)

// Options are the testable knobs of the driver.
type Options struct {
	Run cmdrun.Runner
	// ProcDir is the /proc used to find orphan Firecracker processes.
	ProcDir string
	// BootTimeout bounds boot + init + executor connection (default 90 s).
	BootTimeout time.Duration
	// Net overrides the network plumbing (tests).
	Net netsetup.Plumbing
	// FirecrackerArgs are extra arguments for Firecracker (tests use them to
	// select the fake).
	FirecrackerArgs []string
}

// Driver implements sandbox.Driver for micro-VMs.
type Driver struct {
	cfg      sandbox.Config
	opt      Options
	fcPath   string
	jailer   string
	execDisk string
	store    store
	net      netsetup.Plumbing
	lock     *os.File

	mu    sync.Mutex
	slots *sandbox.Slots
}

// New builds the driver: resolves binaries, builds the exec disk, takes the
// state-directory lock and enables IPv4 forwarding. Run Reap before New sandboxes.
func New(ctx context.Context, cfg sandbox.Config, opt Options) (*Driver, error) {
	if opt.Run == nil {
		opt.Run = cmdrun.Exec{}
	}
	if opt.ProcDir == "" {
		opt.ProcDir = "/proc"
	}
	if opt.BootTimeout == 0 {
		opt.BootTimeout = 90 * time.Second
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
	if err := d.acquireLock(); err != nil {
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

func (d *Driver) acquireLock() error {
	f, err := os.OpenFile(filepath.Join(d.cfg.StateDir, "vm.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("another ermite-runner already uses state directory %s (reaping would destroy its VMs): %w", d.cfg.StateDir, err)
	}
	d.lock = f
	return nil
}

// Mode implements sandbox.Driver.
func (d *Driver) Mode() sandbox.Mode { return sandbox.VM }

// Close releases the lock and the DNS listeners.
func (d *Driver) Close() error {
	if d.net != nil {
		d.net.Close()
	}
	if d.lock != nil {
		err := d.lock.Close()
		d.lock = nil
		return err
	}
	return nil
}

// New implements sandbox.Driver.
func (d *Driver) New() (sandbox.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	slot, err := d.slots.Acquire()
	if err != nil {
		return nil, err
	}
	return &vmSandbox{d: d, slot: slot}, nil
}

func (d *Driver) release(slot int) {
	d.mu.Lock()
	d.slots.Release(slot)
	d.mu.Unlock()
}

// vmSandbox is one VM, single use.
type vmSandbox struct {
	d    *Driver
	slot int

	mu       sync.Mutex
	started  bool
	stopped  bool
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

func (s *vmSandbox) uid() int { return s.d.cfg.UIDBase + s.slot }

// Start implements sandbox.Sandbox.
func (s *vmSandbox) Start(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return nil, errors.New("vm sandbox: Start called twice or after Stop")
	}
	s.started = true
	log := zerowrap.FromCtx(ctx)
	d := s.d
	spec = d.cfg.Resolve(spec)

	id, err := sandbox.NewID()
	if err != nil {
		s.releaseLocked()
		return nil, err
	}
	jailed := !d.cfg.JailerOff
	runUID := s.uid()
	if !jailed {
		runUID = os.Getuid()
	}
	s.lay = newLayout(d.cfg.StateDir, filepath.Base(d.fcPath), id, jailed, runUID)
	s.rec = record{ID: id, Slot: s.slot, Job: spec.JobID, Dir: s.lay.dir}
	// The record exists before the first resource, so any crash from here on is
	// reapable.
	if err := d.store.save(s.rec); err != nil {
		s.releaseLocked()
		return nil, fmt.Errorf("write state record: %w", err)
	}

	conn, err := s.boot(ctx, spec, id)
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
	log.Info().Str("vm", id).Int("slot", s.slot).Str("job", spec.JobID).Msg("micro-VM running")
	return conn, nil
}

func (s *vmSandbox) boot(ctx context.Context, spec sandbox.JobSpec, id string) (sandbox.Conn, error) {
	d := s.d
	netw, err := netpolicy.Subnet(d.cfg.Network, s.slot)
	if err != nil {
		return nil, err
	}

	// Network first: the TAP must exist when Firecracker opens it.
	s.tap, s.gateway = tapName(s.slot), netw.Gateway
	s.rec.Tap, s.rec.Gate = s.tap, s.gateway.String()
	if err := d.store.save(s.rec); err != nil {
		return nil, err
	}
	if err := createTap(ctx, d.opt.Run, s.tap, s.lay.uid, s.gateway); err != nil {
		return nil, err
	}
	s.attached = true // from here Detach is idempotent and always safe
	if err := d.net.Attach(ctx, netpolicy.Guest{Iface: s.tap, Gateway: netw.Gateway, Addr: netw.Guest}); err != nil {
		return nil, err
	}

	// The VMM process.
	apiSock := s.lay.host("api.sock")
	if s.lay.jailed {
		apiSock = s.lay.host("run/firecracker.socket")
	} else if err := os.MkdirAll(s.lay.root, 0o700); err != nil {
		return nil, err
	}
	if err := s.startVMM(id, apiSock); err != nil {
		return nil, err
	}
	api := newAPIClient(apiSock)
	if err := s.waitAPI(ctx, api, apiSock); err != nil {
		return nil, err
	}

	// Files: the process only opens them when configured below.
	execDisk, err := s.lay.share(d.execDisk, "exec.ext4")
	if err != nil {
		return nil, err
	}
	baseDisk, err := s.lay.share(d.cfg.BaseImagePath(), "base.ext4")
	if err != nil {
		return nil, err
	}
	kernel, err := s.lay.share(d.cfg.KernelPath(), "kernel")
	if err != nil {
		return nil, err
	}
	if err := makeScratch(ctx, d.opt.Run, s.lay.host("scratch.ext4"), spec.DiskMiB); err != nil {
		return nil, err
	}
	if err := s.lay.own("scratch.ext4"); err != nil {
		return nil, err
	}

	// The host side of the vsock stream: Firecracker connects to
	// <uds_path>_<port> when the guest connects to the host CID.
	udsName := "v.sock"
	ln, err := net.Listen("unix", s.lay.host(udsName)+fmt.Sprintf("_%d", sandbox.VsockPort))
	if err != nil {
		return nil, fmt.Errorf("listen vsock socket: %w", err)
	}
	s.listener = ln
	if s.lay.jailed {
		if err := os.Chown(s.lay.host(udsName+fmt.Sprintf("_%d", sandbox.VsockPort)), s.lay.uid, s.lay.uid); err != nil {
			return nil, err
		}
	}
	accepted := make(chan acceptResult, 1)
	go func() { c, err := ln.Accept(); accepted <- acceptResult{c, err} }()

	err = api.configure(ctx, vmConfig{
		Machine: machineConfig{VcpuCount: spec.CPUs, MemSizeMiB: spec.MemoryMiB},
		Boot:    bootSource{KernelImagePath: kernel, BootArgs: bootArgs(netw.Guest, netw.Gateway, time.Now().Unix())},
		Drives: []drive{
			{DriveID: "rootfs", PathOnHost: execDisk, IsRootDevice: true, IsReadOnly: true},
			{DriveID: "base", PathOnHost: baseDisk, IsReadOnly: true},
			{DriveID: "scratch", PathOnHost: s.lay.fc("scratch.ext4")},
		},
		Net:   networkInterface{IfaceID: "eth0", HostDevName: s.tap, GuestMAC: macFor(s.slot)},
		Vsock: vsockDevice{GuestCID: 3, UDSPath: s.lay.fc(udsName)},
	})
	if err != nil {
		return nil, err
	}

	timer := time.NewTimer(d.opt.BootTimeout)
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
		return nil, fmt.Errorf("executor did not connect within %s", d.opt.BootTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type acceptResult struct {
	c   net.Conn
	err error
}

func (s *vmSandbox) startVMM(id, apiSock string) error {
	d := s.d
	var cmd *exec.Cmd
	if s.lay.jailed {
		args := []string{"--id", id, "--exec-file", d.fcPath,
			"--uid", fmt.Sprint(s.uid()), "--gid", fmt.Sprint(s.uid()),
			"--chroot-base-dir", jailBase(d.cfg.StateDir),
			"--resource-limit", "no-file=4096",
			"--"}
		cmd = exec.Command(d.jailer, append(args, d.opt.FirecrackerArgs...)...)
	} else {
		args := append([]string{"--id", id, "--api-sock", apiSock}, d.opt.FirecrackerArgs...)
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
	go func() { _ = cmd.Wait(); close(s.exited) }()
	s.rec.PID = cmd.Process.Pid
	return s.d.store.save(s.rec)
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
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker API socket %s not ready after 10s", sock)
		}
	}
}

// Stop implements sandbox.Sandbox.
func (s *vmSandbox) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
	}
	if !s.started {
		s.stopped = true
		s.releaseLocked()
		return nil
	}
	return s.cleanupLocked(context.WithoutCancel(ctx))
}

func (s *vmSandbox) releaseLocked() {
	s.stopped = true
	s.d.release(s.slot)
}

// cleanupLocked removes everything the VM created, in dependency order, and
// keeps going after an error. The state record and the slot are released only
// when everything is gone, so that a half-failed cleanup is retried by Reap
// rather than forgotten and the slot (hence TAP name) is not reused meanwhile.
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
	if s.cmd != nil && s.cmd.Process != nil {
		killAndWait(s.cmd.Process.Pid, 5*time.Second)
		select {
		case <-s.exited:
		case <-time.After(5 * time.Second):
			errs = append(errs, fmt.Errorf("firecracker pid %d did not exit", s.cmd.Process.Pid))
		}
	}
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
	if len(errs) == 0 {
		if err := d.store.remove(s.rec.ID); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		s.releaseLocked()
		return nil
	}
	s.stopped = true // do not retry from here; Reap at the next start will
	err := errors.Join(errs...)
	log.Error().Err(err).Str("vm", s.rec.ID).Msg("micro-VM cleanup incomplete; the next runner start will retry")
	return err
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
