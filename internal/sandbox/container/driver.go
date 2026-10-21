// SPDX-License-Identifier: MIT

// Package container is the container sandbox, for hosts without KVM: one
// container per job with its own Docker daemon inside (the sysbox runtime when
// the daemon lists it, else a privileged container), on its own bridge network
// and with its own scratch disk. The scratch disk is a sparse ext4 file of
// ERMITE_RUNNER_JOB_DISK, loop-mounted on the host (so it needs root, like the
// rest of this mode) and bind-mounted at /scratch: the nested dockerd keeps its
// data root there and the job workspace is a directory of it, so a job cannot
// write more than its disk size to the host (the container's own writable layer
// stays small: nothing big is written outside /scratch and /home/runner/work).
// Everything Docker-side carries the label sandbox.LabelJob so that Reap finds
// it after a crash.
//
// The executor stream is a Unix socket in a per-job directory of the runner's
// state directory, bind-mounted into the container. The runner executable and
// the host's external tools (Node.js) are bind-mounted read-only. Both paths are host paths given to the Docker daemon:
// the daemon must run on the same host (and mount namespace) as the runner.
package container

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
	"bnema.dev/ermite-runner/internal/sandbox/netsetup"
	"bnema.dev/ermite-runner/internal/tcprelay"
)

// SysboxRuntime is the name of the sysbox runtime in `docker info`.
const SysboxRuntime = "sysbox-runc"

// bridgePrefix must match the "erbr*" wildcard of the nftables policy.
const bridgePrefix = "erbr"

// Options are the testable knobs of the driver.
type Options struct {
	Run cmdrun.Runner
	// Net overrides the network plumbing (tests).
	Net netsetup.Plumbing
	// engine overrides the Docker client (tests).
	engine engine
	// UnsafeAllowRootlessForTests lets tests exercise the Engine API and the executor against
	// the rootless daemon of a workstation. Never set outside tests: a nested
	// dockerd cannot work in a rootless daemon, so the jobs would run without
	// Docker. The factories of package drivers never set it.
	UnsafeAllowRootlessForTests bool
	// ConnectTimeout bounds container start + init + executor connection
	// (default 90 s).
	ConnectTimeout time.Duration
	// Mounter overrides the loop mounts (tests).
	Mounter *disk.Mounter
	// FreeSpace overrides statfs for the free-space warning (tests).
	FreeSpace func(string) (int64, error)
}

// Driver implements sandbox.Driver for containers.
type Driver struct {
	cfg        sandbox.Config
	opt        Options
	eng        engine
	net        netsetup.Plumbing
	mnt        disk.Mounter
	lock       *sandbox.HostLock
	image      string
	runtime    string // "" when privileged
	privileged bool
	exe        string

	mu    sync.Mutex
	slots *sandbox.Slots
}

// New takes the host-wide lock, connects to the Docker daemon (DOCKER_HOST
// honoured) and checks it: not rootless, the job image present (pulled if
// missing), the runtime decided (sysbox when listed; else privileged, unless
// ERMITE_RUNNER_DENY_PRIVILEGED=1, which refuses). IPv4 forwarding is enabled
// host-wide and stays on (see the README).
func New(ctx context.Context, cfg sandbox.Config, opt Options) (*Driver, error) {
	if opt.Run == nil {
		opt.Run = cmdrun.Exec{}
	}
	if opt.ConnectTimeout == 0 {
		opt.ConnectTimeout = 90 * time.Second
	}
	d := &Driver{cfg: cfg, opt: opt, image: cfg.OCIImage(), slots: sandbox.NewSlots(cfg.Capacity)}
	if opt.Mounter != nil {
		d.mnt = *opt.Mounter
	} else {
		d.mnt = disk.Mounter{Run: opt.Run}
	}
	exe, err := cfg.ExecutablePath()
	if err != nil {
		return nil, err
	}
	if err := sandbox.CheckStatic(exe); err != nil {
		return nil, err
	}
	d.exe = exe

	if d.lock, err = sandbox.AcquireHostLock(cfg.LockFile); err != nil {
		return nil, err
	}
	eng := opt.engine
	if eng == nil {
		e, err := newDockerEngine()
		if err != nil {
			_ = d.lock.Release()
			return nil, err
		}
		eng = e
	}
	d.eng = eng
	fail := func(err error) (*Driver, error) {
		_ = eng.Close()
		_ = d.lock.Release()
		return nil, err
	}

	info, err := eng.Info(ctx)
	if err != nil {
		return fail(fmt.Errorf("cannot reach the Docker daemon (DOCKER_HOST): %w", err))
	}
	if info.Rootless && !opt.UnsafeAllowRootlessForTests {
		return fail(errRootless)
	}
	if info.OSType != "" && info.OSType != "linux" {
		return fail(fmt.Errorf("the Docker daemon runs %s containers, need linux", info.OSType))
	}
	log := zerowrap.FromCtx(ctx)
	if slices.Contains(info.Runtimes, SysboxRuntime) {
		d.runtime = SysboxRuntime
		log.Info().Str("runtime", SysboxRuntime).Msg("container isolation: using the sysbox runtime")
	} else {
		if cfg.DenyPrivileged {
			return fail(fmt.Errorf("the sysbox runtime is not available and %s=1 forbids privileged containers (install sysbox, or unset the setting to accept host-root-equivalent jobs)", sandbox.EnvDenyPrivileged))
		}
		d.privileged = true
		log.Warn().Msg(privilegedWarning)
	}
	if err := disk.CheckMkfs(ctx, opt.Run); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return fail(fmt.Errorf("state directory: %w", err))
	}
	sandbox.WarnIfLowSpace(ctx, cfg.StateDir, cfg.Capacity, cfg.JobDiskMiB, opt.FreeSpace)
	if err := eng.EnsureImage(ctx, d.image); err != nil {
		return fail(err)
	}
	d.net = opt.Net
	if d.net == nil {
		n := newNet(opt.Run, cfg.DNSPort, info.Podman, cfg.GuestForge.DNSName(), cfg.GuestForge.RelayTarget())
		d.net = n
	}
	if err := d.net.EnableForwarding(); err != nil {
		return fail(err)
	}
	if cfg.GuestForge != nil {
		log.Warn().Msg(cfg.GuestForge.Warning())
	}
	return d, nil
}

// newNet builds the host network plumbing. Only podman creates the bridge
// address late (see daemonInfo.Podman), so only there the DNS and relay
// listeners may come before the address; on Docker a missing gateway address stays a startup error.
func newNet(run cmdrun.Runner, dnsPort int, podman bool, forgeHost, relay string) *netsetup.Net {
	n := netsetup.New(run, dnsPort, "")
	n.DNS.FreeBind = podman
	n.DNS.Override = forgeHost
	if relay != "" {
		n.Relay = tcprelay.New(relay)
		n.Relay.FreeBind = podman
	}
	return n
}

// maxSockPath is the longest Unix socket path Linux accepts (108 with the NUL).
const maxSockPath = 107

// checkSockPaths verifies that the sockets of a job's socket directory fit in
// sockaddr_un.sun_path (108 bytes): the executor socket and, when the cache is
// enabled, the cache socket, which is one byte longer. It says so instead of the
// "invalid argument" of bind.
func checkSockPaths(dir string, cache bool) error {
	socks := []struct{ what, name string }{{"executor", sandbox.ContainerSocket}}
	if cache {
		socks = append(socks, struct{ what, name string }{"cache", sandbox.CacheSocket})
	}
	for _, s := range socks {
		p := filepath.Join(dir, s.name)
		if len(p) > maxSockPath {
			return fmt.Errorf("%s socket path %q is %d bytes, over the %d-byte limit of a Unix socket: use a shorter %s", s.what, p, len(p), maxSockPath, sandbox.EnvStateDir)
		}
	}
	return nil
}

// privilegedWarning is logged when the driver starts and again at every job
// start: a privileged container is host-root equivalent.
const privilegedWarning = "privileged container: host-root equivalent, network policy best effort"

// Mode implements sandbox.Driver.
func (d *Driver) Mode() sandbox.Mode { return sandbox.Container }

// Isolation implements sandbox.Driver.
func (d *Driver) Isolation() sandbox.Isolation {
	if d.privileged {
		return sandbox.IsolationContainerPrivileged
	}
	return sandbox.IsolationContainerSysbox
}

// Privileged reports whether jobs run privileged (no sysbox).
func (d *Driver) Privileged() bool { return d.privileged }

// Close implements sandbox.Driver.
func (d *Driver) Close() error {
	if d.net != nil {
		d.net.Close()
	}
	err := d.eng.Close()
	return errors.Join(err, d.lock.Release())
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
	return &containerSandbox{d: d, slot: slot, id: id}, nil
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

func (d *Driver) runDir(id string) string { return filepath.Join(d.cfg.StateDir, "containers", id) }

// sockDir holds only the executor socket: it is the one directory of the job
// that is bind-mounted (as /run/ermite), so the job never sees the scratch disk
// file, nor the other host paths of its own sandbox.
func (d *Driver) sockDir(id string) string { return filepath.Join(d.runDir(id), "sock") }

// scratchDir is where the loop-mounted scratch disk of a job is mounted.
func (d *Driver) scratchDir(id string) string { return filepath.Join(d.runDir(id), "scratch") }

// workDir is the job workspace, a directory of the scratch disk.
func (d *Driver) workDir(id string) string { return filepath.Join(d.scratchDir(id), "work") }

// Where the job workspace and its temporary files appear in the container.
const (
	containerWorkspace = "/home/runner/work"
	containerTmp       = "/tmp"
)

type containerSandbox struct {
	d       *Driver
	slot    int
	id      string
	gate    sandbox.BootGate
	timings map[string]int64

	mu        sync.Mutex
	started   bool
	stopped   bool
	leakErr   error
	netID     string
	container string
	mounted   bool
	listener  net.Listener
	cacheOn   bool
	conn      net.Conn
	iface     string
	gateway   netip.Addr
	attached  bool
}

// StartTimings returns a copy of the phase durations of a successful Start.
// It is valid once Start has returned, which writes them.
func (s *containerSandbox) StartTimings() map[string]int64 { return maps.Clone(s.timings) }

// ID implements sandbox.Sandbox.
func (s *containerSandbox) ID() string { return s.id }

// Start implements sandbox.Sandbox.
func (s *containerSandbox) Start(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	// A Stop from another goroutine aborts the boot through the gate.
	bootCtx, err := s.gate.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("container sandbox: %w", err)
	}
	defer s.gate.End()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return nil, errors.New("container sandbox: Start called twice or after Stop")
	}
	s.started = true
	d := s.d
	log := zerowrap.FromCtx(ctx)
	spec = d.cfg.Resolve(spec)
	if d.privileged {
		// At every job start, not just at driver start: the job log and the
		// operator's logs must say what kind of isolation this job got.
		log.Warn().Str("job", spec.JobID).Str("container", s.id).Msg(privilegedWarning)
	}
	if d.cfg.GuestForge != nil {
		log.Warn().Str("job", spec.JobID).Str("container", s.id).Msg(d.cfg.GuestForge.Warning())
	}
	start := time.Now()
	s.timings = make(map[string]int64)
	conn, err := s.run(bootCtx, spec)
	if err != nil {
		s.timings = nil
		var tail string
		if s.container != "" {
			tail, _ = d.eng.LogsTail(context.WithoutCancel(ctx), s.container, 40)
		}
		cerr := s.cleanupLocked(context.WithoutCancel(ctx))
		if tail != "" {
			err = fmt.Errorf("%w\ncontainer log tail:\n%s", err, tail)
		}
		return nil, errors.Join(err, cerr)
	}
	fields := map[string]any{"total_ms": time.Since(start).Milliseconds()}
	for k, v := range s.timings {
		fields[k] = v
	}
	log.Info().Str("container", s.container).Int("slot", s.slot).Str("job", spec.JobID).Bool("privileged", d.privileged).Fields(fields).Msg("container running")
	return conn, nil
}

// run runs the boot stages in order; each records what it created for cleanup.
func (s *containerSandbox) run(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	netw, err := netpolicy.Subnet(s.d.cfg.Network, s.slot)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	accepted, err := s.runSocket()
	if err != nil {
		return nil, err
	}
	s.timings["socket_ms"] = time.Since(start).Milliseconds()
	start = time.Now()
	if err := s.runCache(spec); err != nil {
		return nil, err
	}
	s.timings["cache_ms"] = time.Since(start).Milliseconds()
	start = time.Now()
	if err := s.runScratch(ctx, spec); err != nil {
		return nil, err
	}
	labels := map[string]string{sandbox.LabelJob: spec.JobID}
	s.timings["scratch_ms"] = time.Since(start).Milliseconds()
	start = time.Now()
	if err := s.runNetwork(ctx, netw, labels); err != nil {
		return nil, err
	}
	s.timings["net_ms"] = time.Since(start).Milliseconds()
	start = time.Now()
	exited, err := s.runContainer(ctx, spec, netw, labels)
	if err != nil {
		return nil, err
	}
	s.timings["container_ms"] = time.Since(start).Milliseconds()
	start = time.Now()
	conn, err := s.waitExecutor(ctx, accepted, exited)
	if err == nil {
		s.timings["connect_ms"] = time.Since(start).Milliseconds()
	}
	return conn, err
}

type acceptResult struct {
	c   net.Conn
	err error
}

// runSocket creates the per-job directory and listens on the executor socket in
// it. The directory exists before anything on the Docker side, so a crash
// leaves a recognisable one.
func (s *containerSandbox) runSocket() (<-chan acceptResult, error) {
	d := s.d
	for _, dir := range []string{d.runDir(s.id), d.sockDir(s.id)} {
		if err := os.MkdirAll(dir, 0o711); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o711); err != nil {
			return nil, err
		}
	}
	dir := d.sockDir(s.id)
	sock := filepath.Join(dir, sandbox.ContainerSocket)
	if err := checkSockPaths(dir, d.cfg.Cache != nil); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listen executor socket: %w", err)
	}
	s.listener = ln
	// The container's root may be a mapped (sysbox) user: it must be able to
	// connect. The path is private to the job (random id, 0711 directory).
	if err := os.Chmod(sock, 0o666); err != nil {
		return nil, err
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		accepted <- acceptResult{c, err}
	}()
	return accepted, nil
}

// runCache serves the cache on a second socket of the job's private
// directory, next to the executor socket: the guest relay of the container
// connects to /run/ermite/cache.sock. Only this container has that directory
// mounted, and the service accepts on it the tokens of this job only.
func (s *containerSandbox) runCache(spec sandbox.JobSpec) error {
	d := s.d
	if d.cfg.Cache == nil {
		return nil
	}
	sock := filepath.Join(d.sockDir(s.id), sandbox.CacheSocket)
	// The container's root may be a mapped (sysbox) user: it must be able to
	// connect, as for the executor socket.
	// No bandwidth limit (0): a container has no per-job network rate limit
	// (NetMbit is a micro-VM setting), and its cache traffic is a local socket.
	if err := d.cfg.ServeCache(s.id, sock, spec, 0, func(p string) error { return os.Chmod(p, 0o666) }); err != nil {
		return fmt.Errorf("serve the cache socket: %w", err)
	}
	s.cacheOn = true
	return nil
}

// runScratch creates the sparse ext4 scratch disk (ERMITE_RUNNER_JOB_DISK) and
// loop-mounts it; the workspace directory lives on it.
func (s *containerSandbox) runScratch(ctx context.Context, spec sandbox.JobSpec) error {
	d := s.d
	file := filepath.Join(d.runDir(s.id), "scratch.ext4")
	if err := disk.CreateScratch(ctx, d.opt.Run, file, spec.DiskMiB, -1); err != nil {
		return err
	}
	mnt := d.scratchDir(s.id)
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	if err := d.mnt.Mount(ctx, file, mnt); err != nil {
		return err
	}
	s.mounted = true
	// Workspace, temp and the nested dockerd's data root on the scratch disk. The
	// executor (phase G) sets the job layout and ownership inside the sandbox.
	for _, sub := range []string{"work", "tmp", "docker"} {
		if err := os.MkdirAll(filepath.Join(mnt, sub), 0o755); err != nil {
			return err
		}
	}
	return os.Chmod(filepath.Join(mnt, "tmp"), sandbox.TmpMode)
}

// runNetwork creates the job's bridge and installs the firewall policy.
func (s *containerSandbox) runNetwork(ctx context.Context, netw netpolicy.Net, labels map[string]string) error {
	d := s.d
	s.iface = fmt.Sprintf("%s%d", bridgePrefix, s.slot)
	s.gateway = netw.Gateway
	netID, err := d.eng.CreateNetwork(ctx, networkSpec{Name: "ermite-" + s.id, Bridge: s.iface, Subnet: netw.Prefix, Gateway: netw.Gateway, Labels: labels})
	if err != nil {
		return err
	}
	s.netID = netID
	s.attached = true
	return d.net.Attach(ctx, netpolicy.Guest{Iface: s.iface, Gateway: netw.Gateway, Addr: netw.Guest, ForgePort: d.cfg.GuestForge.TCPPort()})
}

// runContainer creates and starts the container.
func (s *containerSandbox) runContainer(ctx context.Context, spec sandbox.JobSpec, netw netpolicy.Net, labels map[string]string) (<-chan exitResult, error) {
	d := s.d
	entry := sandbox.GuestExecDir + "/" + sandbox.GuestExecName
	binds := []bindMount{
		{Source: d.exe, Target: entry, ReadOnly: true},
		{Source: d.sockDir(s.id), Target: sandbox.ContainerRunDir},
		{Source: d.scratchDir(s.id), Target: sandbox.GuestScratch},
		{Source: d.workDir(s.id), Target: containerWorkspace},
		{Source: filepath.Join(d.scratchDir(s.id), "tmp"), Target: containerTmp},
	}
	binds = append(binds, externalBinds(d.cfg.ExternalsOf())...)
	cid, err := d.eng.CreateContainer(ctx, containerSpec{
		Name:       "ermite-" + s.id,
		Image:      d.image,
		Entrypoint: []string{entry, "init"},
		Env:        []string{"ERMITE_CONNECT=unix:" + sandbox.ContainerRunDir + "/" + sandbox.ContainerSocket},
		Labels:     labels,
		Runtime:    d.runtime,
		Privileged: d.privileged,
		Network:    "ermite-" + s.id,
		IPv4:       netw.Guest,
		DNS:        netw.Gateway,
		Binds:      binds,
		MemoryMiB:  spec.MemoryMiB,
		CPUs:       spec.CPUs,
		Pids:       d.cfg.JobPids,
	})
	if err != nil {
		return nil, err
	}
	s.container = cid
	exited := d.eng.WaitExit(ctx, cid)
	if err := d.eng.StartContainer(ctx, cid); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	return exited, nil
}

// externalBinds are the read-only bind mounts of the host's external tools
// (Node.js) at /home/runner/externals/<name>, GitHub's layout. A container needs
// no more than the host path: it reads the tool directly.
func externalBinds(set []sandbox.External) []bindMount {
	var out []bindMount
	for _, e := range set {
		out = append(out, bindMount{Source: e.Dir, Target: sandbox.GuestExternalsDir + "/" + e.Name, ReadOnly: true})
	}
	return out
}

// waitExecutor waits for the executor to connect. Only ctx (the caller's
// context, or Stop through the boot gate), the container exiting, and the
// timeout end the wait.
func (s *containerSandbox) waitExecutor(ctx context.Context, accepted <-chan acceptResult, exited <-chan exitResult) (sandbox.Conn, error) {
	timer := time.NewTimer(s.d.opt.ConnectTimeout)
	defer timer.Stop()
	select {
	case r := <-accepted:
		if r.err != nil {
			return nil, fmt.Errorf("accept executor connection: %w", r.err)
		}
		s.conn = r.c
		return r.c, nil
	case ex := <-exited:
		if ex.Err != nil {
			return nil, fmt.Errorf("waiting for the container: %w", ex.Err)
		}
		return nil, fmt.Errorf("container exited (code %d) before the executor connected", ex.Code)
	case <-timer.C:
		return nil, fmt.Errorf("executor did not connect within %s", s.d.opt.ConnectTimeout)
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// Stop implements sandbox.Sandbox.
func (s *containerSandbox) Stop(ctx context.Context) error {
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

func (s *containerSandbox) releaseLocked() {
	s.stopped = true
	s.d.release(s.slot)
}

// cleanupLocked removes the container first; only when it is confirmed gone are
// the firewall policy, the network, the scratch mount and the files removed. A
// container that may still run keeps its policy and bridge: tearing the policy
// down under it would remove the isolation it relies on. Failures mark the
// sandbox as leaked (sandbox.ErrLeaked): its slot stays taken until Reap.
func (s *containerSandbox) cleanupLocked(ctx context.Context) error {
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
	if s.cacheOn {
		d.cfg.StopCache(s.id)
		s.cacheOn = false
	}
	if s.container != "" {
		if err := d.eng.RemoveContainer(ctx, s.container); err != nil {
			errs = append(errs, fmt.Errorf("remove container: %w", err))
		} else {
			s.container = ""
		}
	}
	if s.container == "" {
		if s.attached {
			if err := d.net.Detach(ctx, s.iface, s.gateway); err != nil {
				errs = append(errs, err)
			} else {
				s.attached = false
			}
		}
		if s.netID != "" {
			if err := d.eng.RemoveNetwork(ctx, s.netID); err != nil {
				errs = append(errs, fmt.Errorf("remove network: %w", err))
			} else {
				s.netID = ""
			}
		}
		if s.mounted {
			if err := d.mnt.Unmount(ctx, d.scratchDir(s.id)); err != nil {
				errs = append(errs, err)
			} else {
				s.mounted = false
			}
		}
		// Never delete files below a mount that is still there.
		if !s.mounted {
			if err := os.RemoveAll(d.runDir(s.id)); err != nil {
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
	s.leakErr = fmt.Errorf("%w: container sandbox %s: %w", sandbox.ErrLeaked, s.id, errors.Join(errs...))
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(s.leakErr).Str("job", s.id).Msg("container cleanup incomplete; call Reap to finish it")
	return s.leakErr
}

// Reap implements sandbox.Driver: remove every container and network carrying
// the job label, the nftables table, the scratch mounts and the per-job
// directories, and free the slots of leaked sandboxes. It returns ErrBusy while
// a sandbox is running.
func (d *Driver) Reap(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
	d.mu.Lock()
	busy := d.slots.Busy()
	d.mu.Unlock()
	if busy > 0 {
		return fmt.Errorf("%w (%d running)", sandbox.ErrBusy, busy)
	}
	var errs []error
	ids, err := d.eng.ListContainers(ctx, sandbox.LabelJob)
	if err != nil {
		errs = append(errs, fmt.Errorf("list containers: %w", err))
	}
	for _, id := range ids {
		log.Warn().Str("container", id).Msg("removing leftover job container")
		if err := d.eng.RemoveContainer(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	nets, err := d.eng.ListNetworks(ctx, sandbox.LabelJob)
	if err != nil {
		errs = append(errs, fmt.Errorf("list networks: %w", err))
	}
	for _, n := range nets {
		log.Warn().Str("network", n).Msg("removing leftover job network")
		if err := d.eng.RemoveNetwork(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	if err := d.net.Reset(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reset nftables table: %w", err))
	}
	// Unmount before deleting: a directory with a live mount below it must not
	// be removed (and EBUSY would be the best case).
	root := filepath.Join(d.cfg.StateDir, "containers")
	mounts, merr := d.mnt.MountPoints()
	if merr != nil {
		errs = append(errs, fmt.Errorf("read mounts: %w", merr))
	}
	stuck := map[string]bool{}
	for _, mp := range mounts {
		rel, ok := strings.CutPrefix(mp, root+"/")
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rel, "/")
		if !sandbox.ValidID(id) {
			continue
		}
		log.Warn().Str("mount", mp).Msg("unmounting leftover scratch disk")
		if err := d.mnt.Unmount(ctx, mp); err != nil {
			stuck[id] = true
			errs = append(errs, err)
		}
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if sandbox.ValidID(e.Name()) && !stuck[e.Name()] {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) == 0 {
		d.mu.Lock()
		d.slots.ReleaseLeaked()
		d.mu.Unlock()
	}
	return errors.Join(errs...)
}
