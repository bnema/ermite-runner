// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// Reap implements sandbox.Driver. It removes what a previous run of the runner
// left: Firecracker processes (by state record and by scanning /proc for our
// "--id erXXXXXXXX"), TAP devices named ertap<N>, the nftables table, and VM
// directories. It relies on the state-directory lock taken in New: no other
// runner instance can own what it removes.
func (d *Driver) Reap(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
	var errs []error

	recs, err := d.store.list()
	if err != nil {
		errs = append(errs, err)
	}
	procs := firecrackerProcs(d.opt.ProcDir)
	killed := map[int]bool{}
	killProc := func(pid int, id string) {
		if killed[pid] {
			return
		}
		killed[pid] = true
		log.Warn().Int("pid", pid).Str("vm", id).Msg("reaping leftover firecracker process")
		killAndWait(pid, 5*time.Second)
	}
	// 1. Processes: by record, but only if the pid is still one of ours (pids
	// are reused), then any other orphan found in /proc.
	for _, r := range recs {
		if r.PID > 0 && procs[r.PID] == r.ID {
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

	// 3. Files: directories of recorded VMs, then any directory with one of our
	// ids that has no record (crash before the record was written is
	// impossible, but a deleted state dir is not).
	// The jailer's chroot base has one directory per exec-file name.
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
	if len(errs) == 0 {
		for _, r := range recs {
			if err := d.store.remove(r.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
