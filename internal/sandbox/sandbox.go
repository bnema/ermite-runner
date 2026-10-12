// SPDX-License-Identifier: MIT

// Package sandbox defines the isolation boundary of a CI job: one Sandbox per
// job, either a Firecracker micro-VM (package vm) or a container with its own
// Docker daemon (package container). Both run the same executable
// (`ermite-runner init`, then `ermite-runner exec`) and expose the same framed
// stream (package execstream) to the host process.
package sandbox

import (
	"context"
	"errors"
	"io"
)

// Mode is an isolation mode.
type Mode string

// Isolation modes. Auto is a setting only: a Driver always reports VM or
// Container.
const (
	Auto      Mode = "auto"
	VM        Mode = "vm"
	Container Mode = "container"
)

// ErrCapacity is returned by Driver.New when every slot is in use.
var ErrCapacity = errors.New("sandbox: no free slot")

// JobSpec describes the sandbox of one job. Zero resource fields take the
// configured defaults.
type JobSpec struct {
	// JobID identifies the job in logs, labels and leftover reports. It never
	// appears in file or interface names.
	JobID string
	// CPUs, MemoryMiB and DiskMiB size the sandbox (ERMITE_RUNNER_JOB_*).
	CPUs      int
	MemoryMiB int
	DiskMiB   int
}

// Conn is the executor stream: bytes in both directions, framed by execstream.
// Closing it ends the session from the host side.
type Conn io.ReadWriteCloser

// Sandbox isolates one job. A Sandbox is single use: Start once, then Stop.
type Sandbox interface {
	// Start creates the sandbox, boots it and waits for the executor inside to
	// connect back. On error everything created so far has been removed.
	Start(ctx context.Context, spec JobSpec) (Conn, error)
	// Stop destroys the sandbox and everything it created (processes, network
	// devices, firewall rules, files). It is idempotent, safe after a failed or
	// never-made Start, and runs to completion even when ctx is already done.
	Stop(ctx context.Context) error
}

// Driver creates sandboxes of one mode and reaps what a previous run left.
type Driver interface {
	// Mode is VM or Container.
	Mode() Mode
	// New returns a fresh Sandbox bound to a free slot (ErrCapacity if none).
	// The slot is released by Stop.
	New() (Sandbox, error)
	// Reap removes everything a previous run of the runner left behind
	// (leftover processes, devices, firewall rules, containers, files). It must
	// be called once at startup, before any New.
	Reap(ctx context.Context) error
	// Close releases the driver's own resources (DNS forwarder, API client).
	// Sandboxes still running are not stopped.
	Close() error
}

// Slots hands out small integers, lowest first. Slots identify a sandbox's uid,
// network device and /30 subnet, so two live sandboxes never collide.
type Slots struct {
	used []bool
}

// NewSlots returns a pool of n slots.
func NewSlots(n int) *Slots { return &Slots{used: make([]bool, n)} }

// Acquire returns the lowest free slot, or ErrCapacity. Callers serialise
// access (the drivers hold their mutex).
func (s *Slots) Acquire() (int, error) {
	for i, u := range s.used {
		if !u {
			s.used[i] = true
			return i, nil
		}
	}
	return 0, ErrCapacity
}

// Release frees a slot; releasing a free or unknown slot is a no-op.
func (s *Slots) Release(i int) {
	if i >= 0 && i < len(s.used) {
		s.used[i] = false
	}
}

// InUse reports how many slots are taken.
func (s *Slots) InUse() int {
	n := 0
	for _, u := range s.used {
		if u {
			n++
		}
	}
	return n
}
