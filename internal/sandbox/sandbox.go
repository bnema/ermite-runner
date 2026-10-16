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
	"net/http"
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

// Isolation is the effective isolation of a driver, reported by the host at
// registration and shown on the job page.
type Isolation string

// Effective isolations.
const (
	IsolationVM                  Isolation = "vm"
	IsolationContainerSysbox     Isolation = "container_sysbox"
	IsolationContainerPrivileged Isolation = "container_privileged"
)

// ErrCapacity is returned by Driver.New when every slot is in use.
var ErrCapacity = errors.New("sandbox: no free slot")

// ErrLeaked is wrapped by the error of Start or Stop when the cleanup of a
// sandbox is incomplete: something it created (process, network device,
// firewall rules, files, containers) may still exist and its slot stays taken.
// The host should stop assigning jobs to this runner slot, let running jobs
// finish and call Driver.Reap, which retries the cleanup and frees the slot.
var ErrLeaked = errors.New("sandbox: cleanup incomplete, resources leaked")

// ErrBusy is returned by Driver.Reap while sandboxes are running: reaping
// removes everything labelled as ours and would destroy them.
var ErrBusy = errors.New("sandbox: cannot reap while sandboxes are running")

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
	// Results, when not nil, is the results service of the job (artifacts,
	// package resultssvc), served on the sandbox's cache channel beside the
	// cache (Config.ServeCache).
	Results http.Handler
}

// Conn is the executor stream: bytes in both directions, framed by execstream.
// Closing it ends the session from the host side.
type Conn io.ReadWriteCloser

// Sandbox isolates one job. A Sandbox is single use: Start once, then Stop.
type Sandbox interface {
	// ID is the sandbox's identifier ("er" + 8 hex digits), assigned by
	// Driver.New. It names the sandbox's files and Docker objects, so logs and
	// leftover reports can be matched with them.
	ID() string
	// Start creates the sandbox, boots it and waits for the executor inside to
	// connect back. It aborts when ctx ends or when Stop is called from another
	// goroutine. On error everything created so far has been removed, or the
	// error wraps ErrLeaked.
	Start(ctx context.Context, spec JobSpec) (Conn, error)
	// Stop destroys the sandbox and everything it created (processes, network
	// devices, firewall rules, files). It is idempotent, safe after a failed or
	// never-made Start, runs to completion even when ctx is already done, and
	// aborts a Start in progress. When something could not be removed the error
	// wraps ErrLeaked.
	Stop(ctx context.Context) error
}

// Driver creates sandboxes of one mode and reaps what a previous run left.
type Driver interface {
	// Mode is VM or Container.
	Mode() Mode
	// Isolation is the effective isolation (vm, container_sysbox,
	// container_privileged), for registration with ermite.
	Isolation() Isolation
	// New returns a fresh Sandbox bound to a free slot (ErrCapacity if none).
	// The slot is released by Stop, unless the cleanup leaked (ErrLeaked).
	New() (Sandbox, error)
	// Reap removes everything a previous run of the runner left behind
	// (leftover processes, devices, firewall rules, containers, files) and
	// frees the slots of leaked sandboxes. Call it once at startup, before any
	// New, and after ErrLeaked when no sandbox is running; it returns ErrBusy
	// while one is.
	Reap(ctx context.Context) error
	// Close releases the driver's own resources (DNS forwarder, API client,
	// host lock). Sandboxes still running are not stopped.
	Close() error
}

// Slots hands out small integers, lowest first. Slots identify a sandbox's uid,
// network device and /30 subnet, so two live sandboxes never collide. A slot
// whose sandbox leaked stays taken (its device name and subnet may still be in
// use) until ReleaseLeaked.
type Slots struct {
	used   []bool
	leaked []bool
}

// NewSlots returns a pool of n slots.
func NewSlots(n int) *Slots { return &Slots{used: make([]bool, n), leaked: make([]bool, n)} }

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
		s.used[i], s.leaked[i] = false, false
	}
}

// MarkLeaked keeps slot i taken after a failed cleanup.
func (s *Slots) MarkLeaked(i int) {
	if i >= 0 && i < len(s.used) {
		s.used[i], s.leaked[i] = true, true
	}
}

// ReleaseLeaked frees every leaked slot: the caller has reaped what they held.
func (s *Slots) ReleaseLeaked() {
	for i := range s.used {
		if s.leaked[i] {
			s.used[i], s.leaked[i] = false, false
		}
	}
}

// InUse reports how many slots are taken, leaked ones included.
func (s *Slots) InUse() int {
	n := 0
	for _, u := range s.used {
		if u {
			n++
		}
	}
	return n
}

// Busy reports how many slots belong to a sandbox that is not leaked: running,
// starting, or handed out by New and not yet stopped.
func (s *Slots) Busy() int {
	n := 0
	for i, u := range s.used {
		if u && !s.leaked[i] {
			n++
		}
	}
	return n
}
