// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"
)

// FreeMiB returns the space available to unprivileged users on the filesystem
// of path, in MiB.
func FreeMiB(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * st.Bsize >> 20, nil
}

// WarnIfLowSpace logs a warning when the filesystem of dir has less free space
// than capacity sandboxes of diskMiB would need if they all filled their sparse
// scratch disk. It is only a warning: scratch disks are sparse and jobs rarely
// fill them, so a smaller filesystem works until it does not. free is FreeMiB in
// production.
func WarnIfLowSpace(ctx context.Context, dir string, capacity, diskMiB int, free func(string) (int64, error)) {
	if free == nil {
		free = FreeMiB
	}
	log := zerowrap.FromCtx(ctx)
	have, err := free(dir)
	if err != nil {
		log.Warn().Err(err).Str("dir", dir).Msg("cannot check the free space of the state directory")
		return
	}
	need := int64(capacity) * int64(diskMiB)
	if have < need {
		log.Warn().Str("dir", dir).Int64("free_mib", have).Int64("worst_case_mib", need).
			Int("capacity", capacity).Int("job_disk_mib", diskMiB).
			Msg("the state directory has less free space than capacity x job disk: jobs that fill their scratch disk will fail with 'no space left on device'")
	}
}
