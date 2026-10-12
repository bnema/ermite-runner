// SPDX-License-Identifier: MIT

// Package sandboxtest has helpers shared by the tests of the sandbox drivers.
package sandboxtest

import (
	"context"
	"encoding/binary"
	"io"
	"net/netip"
	"os"
	"sync"
	"testing"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/netpolicy"
)

// NoMountRunner runs real commands except mount and umount, which succeed
// without doing anything: loop mounts need root, and the tests that use it
// replace them by plain directories (mkfs still runs).
type NoMountRunner struct{}

// Run implements cmdrun.Runner.
func (NoMountRunner) Run(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	if name == "mount" || name == "umount" {
		return nil, nil
	}
	return cmdrun.Exec{}.Run(ctx, in, name, args...)
}

// WriteStaticELF writes a minimal statically linked ELF64 file (one PT_LOAD, no
// PT_INTERP): enough for sandbox.CheckStatic, never executed.
func WriteStaticELF(t testing.TB, path string) {
	t.Helper()
	var b [64 + 56]byte
	copy(b[:], "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1                // 64-bit, little endian, version
	binary.LittleEndian.PutUint16(b[16:], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(b[18:], 62) // x86-64
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint64(b[32:], 64) // e_phoff
	binary.LittleEndian.PutUint16(b[52:], 64) // e_ehsize
	binary.LittleEndian.PutUint16(b[54:], 56) // e_phentsize
	binary.LittleEndian.PutUint16(b[56:], 1)  // e_phnum
	binary.LittleEndian.PutUint32(b[64:], 1)  // PT_LOAD
	if err := os.WriteFile(path, b[:], 0o755); err != nil {
		t.Fatal(err)
	}
}

// Plumbing is a netsetup.Plumbing that records what is attached.
type Plumbing struct {
	mu       sync.Mutex
	Attached map[string]netpolicy.Guest
	Events   []string
	Resets   int
	FailNext error
}

// NewPlumbing returns an empty Plumbing.
func NewPlumbing() *Plumbing { return &Plumbing{Attached: map[string]netpolicy.Guest{}} }

// EnableForwarding implements netsetup.Plumbing.
func (p *Plumbing) EnableForwarding() error { return nil }

// Attach implements netsetup.Plumbing.
func (p *Plumbing) Attach(_ context.Context, g netpolicy.Guest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.FailNext != nil {
		err := p.FailNext
		p.FailNext = nil
		return err
	}
	p.Attached[g.Iface] = g
	p.Events = append(p.Events, "attach "+g.Iface)
	return nil
}

// Detach implements netsetup.Plumbing.
func (p *Plumbing) Detach(_ context.Context, iface string, _ netip.Addr) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.Attached, iface)
	p.Events = append(p.Events, "detach "+iface)
	return nil
}

// Reset implements netsetup.Plumbing.
func (p *Plumbing) Reset(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Resets++
	p.Attached = map[string]netpolicy.Guest{}
	return nil
}

// Close implements netsetup.Plumbing.
func (p *Plumbing) Close() {}

// Live returns the number of attached guests.
func (p *Plumbing) Live() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Attached)
}
