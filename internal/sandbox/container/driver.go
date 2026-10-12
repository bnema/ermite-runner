// SPDX-License-Identifier: MIT

// Package container is the container sandbox, for hosts without KVM: one
// container per job with its own Docker daemon inside (the sysbox runtime when
// the daemon lists it, else a privileged container), on its own bridge network
// and with its own scratch volume. Everything Docker-side carries the label
// sandbox.LabelJob so that Reap finds it after a crash.
//
// The executor stream is a Unix socket in a per-job directory of the runner's
// state directory, bind-mounted into the container. The runner executable is
// bind-mounted read-only. Both paths are host paths given to the Docker daemon:
// the daemon must run on the same host (and mount namespace) as the runner.
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
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/netsetup"
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
	// allowRootless lets tests exercise the Engine API against the rootless
	// daemon of a workstation. Never set outside tests: a nested dockerd cannot
	// work in a rootless daemon.
	allowRootless bool
	// ConnectTimeout bounds container start + init + executor connection
	// (default 90 s).
	ConnectTimeout time.Duration
}

// Driver implements sandbox.Driver for containers.
type Driver struct {
	cfg        sandbox.Config
	opt        Options
	eng        engine
	net        netsetup.Plumbing
	image      string
	runtime    string // "" when privileged
	privileged bool
	exe        string

	mu    sync.Mutex
	slots *sandbox.Slots
}

// New connects to the Docker daemon (DOCKER_HOST honoured) and checks it: not
// rootless, the job image present (pulled if missing), the runtime decided.
func New(ctx context.Context, cfg sandbox.Config, opt Options) (*Driver, error) {
	if opt.Run == nil {
		opt.Run = cmdrun.Exec{}
	}
	if opt.ConnectTimeout == 0 {
		opt.ConnectTimeout = 90 * time.Second
	}
	eng := opt.engine
	if eng == nil {
		e, err := newDockerEngine()
		if err != nil {
			return nil, err
		}
		eng = e
	}
	d := &Driver{cfg: cfg, opt: opt, eng: eng, image: cfg.OCIImage(), slots: sandbox.NewSlots(cfg.Capacity)}
	fail := func(err error) (*Driver, error) { _ = eng.Close(); return nil, err }

	exe, err := cfg.ExecutablePath()
	if err != nil {
		return fail(err)
	}
	if err := sandbox.CheckStatic(exe); err != nil {
		return fail(err)
	}
	d.exe = exe

	info, err := eng.Info(ctx)
	if err != nil {
		return fail(fmt.Errorf("cannot reach the Docker daemon (DOCKER_HOST): %w", err))
	}
	if info.Rootless && !opt.allowRootless {
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
		d.privileged = true
		log.Warn().Msg("container isolation: sysbox-runc not available, jobs run in PRIVILEGED containers, which is host-root equivalent; their network policy is best effort. Prefer vm isolation or install sysbox")
	}
	if err := eng.EnsureImage(ctx, d.image); err != nil {
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

// Mode implements sandbox.Driver.
func (d *Driver) Mode() sandbox.Mode { return sandbox.Container }

// Privileged reports whether jobs run privileged (no sysbox).
func (d *Driver) Privileged() bool { return d.privileged }

// Close implements sandbox.Driver.
func (d *Driver) Close() error {
	if d.net != nil {
		d.net.Close()
	}
	return d.eng.Close()
}

// New implements sandbox.Driver.
func (d *Driver) New() (sandbox.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	slot, err := d.slots.Acquire()
	if err != nil {
		return nil, err
	}
	return &containerSandbox{d: d, slot: slot}, nil
}

func (d *Driver) release(slot int) {
	d.mu.Lock()
	d.slots.Release(slot)
	d.mu.Unlock()
}

func (d *Driver) runDir(id string) string { return filepath.Join(d.cfg.StateDir, "containers", id) }

type containerSandbox struct {
	d    *Driver
	slot int

	mu        sync.Mutex
	started   bool
	stopped   bool
	id        string
	netID     string
	volume    string
	container string
	listener  net.Listener
	conn      net.Conn
	iface     string
	gateway   netip.Addr
	attached  bool
}

// Start implements sandbox.Sandbox.
func (s *containerSandbox) Start(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return nil, errors.New("container sandbox: Start called twice or after Stop")
	}
	s.started = true
	d := s.d
	log := zerowrap.FromCtx(ctx)
	spec = d.cfg.Resolve(spec)

	id, err := sandbox.NewID()
	if err != nil {
		s.releaseLocked()
		return nil, err
	}
	s.id = id
	conn, err := s.run(ctx, spec)
	if err != nil {
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
	log.Info().Str("container", s.container).Int("slot", s.slot).Str("job", spec.JobID).Bool("privileged", d.privileged).Msg("container running")
	return conn, nil
}

func (s *containerSandbox) run(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	d := s.d
	netw, err := netpolicy.Subnet(d.cfg.Network, s.slot)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{sandbox.LabelJob: spec.JobID}
	name := "ermite-" + s.id

	// Per-job directory with the executor socket. It is created before anything
	// on the Docker side so that a crash leaves a recognisable directory.
	dir := d.runDir(s.id)
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o711); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, sandbox.ContainerSocket)
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
	accepted := make(chan struct {
		c   net.Conn
		err error
	}, 1)
	go func() {
		c, err := ln.Accept()
		accepted <- struct {
			c   net.Conn
			err error
		}{c, err}
	}()

	s.iface = fmt.Sprintf("%s%d", bridgePrefix, s.slot)
	s.gateway = netw.Gateway
	netID, err := d.eng.CreateNetwork(ctx, networkSpec{Name: name, Bridge: s.iface, Subnet: netw.Prefix, Gateway: netw.Gateway, Labels: labels})
	if err != nil {
		return nil, err
	}
	s.netID = netID
	s.attached = true
	if err := d.net.Attach(ctx, netpolicy.Guest{Iface: s.iface, Gateway: netw.Gateway, Addr: netw.Guest}); err != nil {
		return nil, err
	}

	s.volume = name
	if err := d.eng.CreateVolume(ctx, name, labels); err != nil {
		s.volume = ""
		return nil, fmt.Errorf("create volume: %w", err)
	}

	entry := sandbox.GuestExecDir + "/" + sandbox.GuestExecName
	cid, err := d.eng.CreateContainer(ctx, containerSpec{
		Name:       name,
		Image:      d.image,
		Entrypoint: []string{entry, "init"},
		Env:        []string{"ERMITE_CONNECT=unix:" + sandbox.ContainerRunDir + "/" + sandbox.ContainerSocket},
		Labels:     labels,
		Runtime:    d.runtime,
		Privileged: d.privileged,
		Network:    name,
		IPv4:       netw.Guest,
		DNS:        netw.Gateway,
		Binds: []bindMount{
			{Source: d.exe, Target: entry, ReadOnly: true},
			{Source: dir, Target: sandbox.ContainerRunDir},
		},
		Volumes:   []volumeMount{{Name: name, Target: sandbox.GuestScratch}},
		MemoryMiB: spec.MemoryMiB,
		CPUs:      spec.CPUs,
	})
	if err != nil {
		return nil, err
	}
	s.container = cid
	exited := d.eng.WaitExit(ctx, cid)
	if err := d.eng.StartContainer(ctx, cid); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	timer := time.NewTimer(d.opt.ConnectTimeout)
	defer timer.Stop()
	select {
	case r := <-accepted:
		if r.err != nil {
			return nil, fmt.Errorf("accept executor connection: %w", r.err)
		}
		s.conn = r.c
		return r.c, nil
	case ex := <-exited:
		return nil, fmt.Errorf("container exited (code %d) before the executor connected", ex.Code)
	case <-timer.C:
		return nil, fmt.Errorf("executor did not connect within %s", d.opt.ConnectTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Stop implements sandbox.Sandbox.
func (s *containerSandbox) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
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

// cleanupLocked removes the container, then its network, volume, firewall rules
// and directory, continuing after errors. The slot is released only when
// everything is gone (so its bridge name and subnet are not reused while the
// old ones still exist); leftovers are reaped by label at the next start.
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
	if s.container != "" {
		if err := d.eng.RemoveContainer(ctx, s.container); err != nil {
			errs = append(errs, fmt.Errorf("remove container: %w", err))
		} else {
			s.container = ""
		}
	}
	if s.attached {
		if err := d.net.Detach(ctx, s.iface, s.gateway); err != nil {
			errs = append(errs, err)
		} else {
			s.attached = false
		}
	}
	if s.netID != "" && s.container == "" {
		if err := d.eng.RemoveNetwork(ctx, s.netID); err != nil {
			errs = append(errs, fmt.Errorf("remove network: %w", err))
		} else {
			s.netID = ""
		}
	}
	if s.volume != "" && s.container == "" {
		if err := d.eng.RemoveVolume(ctx, s.volume); err != nil {
			errs = append(errs, fmt.Errorf("remove volume: %w", err))
		} else {
			s.volume = ""
		}
	}
	if err := os.RemoveAll(d.runDir(s.id)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		s.releaseLocked()
		return nil
	}
	s.stopped = true
	err := errors.Join(errs...)
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(err).Str("job", s.id).Msg("container cleanup incomplete; the next runner start will retry")
	return err
}

// Reap implements sandbox.Driver: remove every container, network and volume
// carrying the job label, the nftables table, and the per-job directories.
func (d *Driver) Reap(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
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
	vols, err := d.eng.ListVolumes(ctx, sandbox.LabelJob)
	if err != nil {
		errs = append(errs, fmt.Errorf("list volumes: %w", err))
	}
	for _, v := range vols {
		log.Warn().Str("volume", v).Msg("removing leftover job volume")
		if err := d.eng.RemoveVolume(ctx, v); err != nil {
			errs = append(errs, err)
		}
	}
	if err := d.net.Reset(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reset nftables table: %w", err))
	}
	root := filepath.Join(d.cfg.StateDir, "containers")
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if sandbox.ValidID(e.Name()) {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
