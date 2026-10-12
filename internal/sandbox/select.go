// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/bnema/zerowrap"
)

// Factory builds the driver of one mode. It is injected so that this package
// does not import the drivers (which import it).
type Factory func(ctx context.Context, cfg Config) (Driver, error)

// Factories are the available drivers.
type Factories struct {
	VM        Factory
	Container Factory
}

// Prober checks the host. The zero value probes the real host; tests replace
// fields.
type Prober struct {
	// OpenKVM opens the KVM device read-write and closes it.
	OpenKVM func(path string) error
	// LookPath finds a binary in PATH.
	LookPath func(name string) (string, error)
	// Stat reports whether a path exists.
	Stat func(path string) error
}

func (p Prober) withDefaults() Prober {
	if p.OpenKVM == nil {
		p.OpenKVM = func(path string) error {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}
	}
	if p.LookPath == nil {
		p.LookPath = exec.LookPath
	}
	if p.Stat == nil {
		p.Stat = func(path string) error { _, err := os.Stat(path); return err }
	}
	return p
}

// VMUnavailable lists why the vm mode cannot run on this host: an empty result
// means every prerequisite (readable-writable KVM, Firecracker, jailer unless
// disabled, guest kernel, base image) is there.
func VMUnavailable(cfg Config, p Prober) []string {
	p = p.withDefaults()
	var why []string
	if err := p.OpenKVM(cfg.KVMPath); err != nil {
		why = append(why, fmt.Sprintf("%s not usable read-write: %v", cfg.KVMPath, err))
	}
	if cfg.Firecracker != "" {
		if err := p.Stat(cfg.Firecracker); err != nil {
			why = append(why, fmt.Sprintf("firecracker %s: %v", cfg.Firecracker, err))
		}
	} else if _, err := p.LookPath("firecracker"); err != nil {
		why = append(why, "firecracker not found in PATH")
	}
	if !cfg.JailerOff {
		if cfg.Jailer != "" {
			if err := p.Stat(cfg.Jailer); err != nil {
				why = append(why, fmt.Sprintf("jailer %s: %v", cfg.Jailer, err))
			}
		} else if _, err := p.LookPath("jailer"); err != nil {
			why = append(why, "jailer not found in PATH")
		}
	}
	if err := p.Stat(cfg.KernelPath()); err != nil {
		why = append(why, fmt.Sprintf("guest kernel %s: %v", cfg.KernelPath(), err))
	}
	if err := p.Stat(cfg.BaseImagePath()); err != nil {
		why = append(why, fmt.Sprintf("base image %s: %v", cfg.BaseImagePath(), err))
	}
	return why
}

// Select picks the isolation mode and builds its driver. For auto it chooses vm
// when VMUnavailable is empty, container otherwise. For vm it fails when a
// prerequisite is missing; for container it never probes the VM side. The
// chosen mode and the reasons are logged with the logger of ctx. A driver
// factory error is returned as is (auto does not mask a broken vm setup by
// silently running jobs with weaker isolation).
func Select(ctx context.Context, cfg Config, f Factories, p Prober) (Driver, error) {
	log := zerowrap.FromCtx(ctx)
	mode := cfg.Isolation
	var reasons []string
	switch mode {
	case VM, Auto:
		reasons = VMUnavailable(cfg, p)
		if mode == VM && len(reasons) > 0 {
			return nil, fmt.Errorf("isolation vm is not available: %s", strings.Join(reasons, "; "))
		}
		if len(reasons) == 0 {
			mode = VM
		} else {
			mode = Container
		}
	case Container:
	default:
		return nil, fmt.Errorf("unknown isolation %q", mode)
	}

	build := f.VM
	if mode == Container {
		build = f.Container
	}
	if build == nil {
		return nil, fmt.Errorf("isolation %s: no driver compiled in", mode)
	}
	ev := log.Info().Str("configured", string(cfg.Isolation)).Str("mode", string(mode))
	switch {
	case cfg.Isolation == Auto && mode == VM:
		ev = ev.Str("reason", "KVM, firecracker, kernel and base image are available")
	case cfg.Isolation == Auto:
		ev = ev.Strs("reasons", reasons)
	default:
		ev = ev.Str("reason", "set by "+EnvIsolation)
	}
	ev.Msg("isolation mode chosen")
	d, err := build(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("isolation %s: %w", mode, err)
	}
	return d, nil
}

// CheckStatic fails unless the executable at path is a statically linked ELF
// binary: it runs as PID 1 on a root disk that holds nothing else, and in an
// image of another distribution.
func CheckStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s is not an ELF executable (%w); build the runner with CGO_ENABLED=0", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New(path + " is dynamically linked; build the runner with CGO_ENABLED=0")
		}
	}
	return nil
}
