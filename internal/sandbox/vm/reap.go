// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// Reap implements sandbox.Driver. It removes what a previous run of the runner
// left: VMM processes (Firecracker and jailer: matched by "--id <our id>" in
// their command line, never by program name), TAP devices named ertap<N>, the
// nftables table, VM directories and jailer cgroups. It also finishes the
// cleanup of sandboxes that reported ErrLeaked and frees their slots. It relies
// on the host-wide lock taken in New: no other runner can own what it removes.
// It returns ErrBusy while a sandbox is running.
func (d *Driver) Reap(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
	d.mu.Lock()
	busy := d.slots.Busy()
	d.mu.Unlock()
	if busy > 0 {
		return fmt.Errorf("%w (%d running)", sandbox.ErrBusy, busy)
	}
	var errs []error

	recs, err := d.store.list()
	if err != nil {
		errs = append(errs, err)
	}
	procs := vmmProcs(d.opt.ProcDir, d.scope())
	killed := map[int]bool{}
	killProc := func(pid int, id string) {
		if killed[pid] {
			return
		}
		killed[pid] = true
		log.Warn().Int("pid", pid).Str("vm", id).Msg("reaping leftover VMM process")
		if !d.opt.kill(pid, func() bool { return cmdlineID(d.opt.ProcDir, pid, d.scope()) == id }, 5*time.Second) {
			errs = append(errs, fmt.Errorf("vmm process %d (%s) did not exit", pid, id))
		}
	}
	// 1. Processes. A recorded pid is only trusted when /proc/<pid>/cmdline still
	// carries this record's id (pids are reused); then every other process of
	// ours found in /proc, which also covers a lost state directory.
	for _, r := range recs {
		if r.PID > 0 && cmdlineID(d.opt.ProcDir, r.PID, d.scope()) == r.ID {
			killProc(r.PID, r.ID)
		}
	}
	for pid, id := range procs {
		killProc(pid, id)
	}

	// 2. Network: the nft table first (it names interfaces), then every TAP.
	if err := d.net.Reset(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reset nftables table %s: %w", netpolicy.Table, err))
	}
	taps, err := leftoverTaps(ctx, d.opt.Run)
	if err != nil {
		errs = append(errs, fmt.Errorf("list interfaces: %w", err))
	}
	for _, t := range taps {
		log.Warn().Str("tap", t).Msg("removing leftover TAP device")
		if err := deleteTap(ctx, d.opt.Run, t); err != nil {
			errs = append(errs, err)
		}
	}

	// 3. Files and cgroups. The jailer's chroot base has one directory per
	// exec-file name.
	parents := []string{filepath.Join(d.cfg.StateDir, "vm")}
	jails, _ := os.ReadDir(filepath.Join(d.cfg.StateDir, "jail"))
	for _, j := range jails {
		if j.IsDir() {
			parents = append(parents, filepath.Join(d.cfg.StateDir, "jail", j.Name()))
		}
	}
	for _, parent := range parents {
		ents, _ := os.ReadDir(parent)
		for _, e := range ents {
			if sandbox.ValidID(e.Name()) {
				log.Warn().Str("dir", filepath.Join(parent, e.Name())).Msg("removing leftover VM directory")
				if err := removeAll(filepath.Join(parent, e.Name())); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	cg := filepath.Join(d.opt.CgroupRoot, filepath.Base(d.fcPath))
	cents, _ := os.ReadDir(cg)
	for _, e := range cents {
		if e.IsDir() && sandbox.ValidID(e.Name()) {
			if err := removeCgroup(d.opt.CgroupRoot, filepath.Base(d.fcPath), e.Name()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) == 0 {
		for _, r := range recs {
			if err := d.store.remove(r.ID); err != nil {
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

// cmdlineID returns the VM id in the command line of pid, or "" (not running,
// not ours, or recycled for another program).
func cmdlineID(procDir string, pid int, sc scope) string {
	dir := filepath.Join(procDir, strconv.Itoa(pid))
	b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(b) == 0 {
		return ""
	}
	return sc.match(dir, strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"))
}
