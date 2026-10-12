// SPDX-License-Identifier: MIT

// Package disk creates the per-job scratch disks (sparse ext4 files) and, for
// the container mode, loop-mounts them. Both isolation modes size a job's
// writable space the same way: a sparse file of ERMITE_RUNNER_JOB_DISK.
package disk

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

// CreateScratch creates the sparse scratch disk of sizeMiB at path and formats
// it. The file is created exclusively without following symlinks and handed to
// owner (fchown on the open descriptor, never by path) when owner >= 0.
func CreateScratch(ctx context.Context, run cmdrun.Runner, path string, sizeMiB, owner int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if owner >= 0 {
		if err := f.Chown(owner, owner); err != nil {
			_ = f.Close()
			return fmt.Errorf("chown scratch disk: %w", err)
		}
	}
	if err := f.Truncate(int64(sizeMiB) << 20); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// No journal and no reserved blocks: it is a throw-away disk. Lazy table
	// initialisation keeps creation instant and the file sparse.
	if _, err := run.Run(ctx, nil, "mkfs.ext4", "-q", "-F", "-L", "ermite-scratch", "-m", "0",
		"-O", "^has_journal,^resize_inode", "-E", "lazy_itable_init=1,nodiscard", path); err != nil {
		return fmt.Errorf("format scratch disk: %w", err)
	}
	return nil
}

var mkfsVersionRe = regexp.MustCompile(`(?m)^mke2fs (\d+)\.(\d+)(?:\.(\d+))?`)

// MinMkfs is the oldest e2fsprogs the runner accepts: mkfs.ext4 -d (populate
// from a directory, without root) appeared in 1.43. scripts/build-image.sh needs
// 1.47.1 to populate from a tarball.
const MinMkfs = "1.43"

// CheckMkfs fails when mkfs.ext4 is missing or older than MinMkfs.
func CheckMkfs(ctx context.Context, run cmdrun.Runner) error {
	out, err := run.Run(ctx, nil, "mkfs.ext4", "-V")
	// mke2fs prints its version on stderr and exits 0 or 1 depending on the
	// release: judge by the output, not by the exit status.
	maj, min, ok := parseMkfsVersion(string(out))
	if !ok {
		if err != nil {
			return fmt.Errorf("mkfs.ext4 not usable (install e2fsprogs >= %s): %w", MinMkfs, err)
		}
		return fmt.Errorf("cannot read the mkfs.ext4 version from %q (need e2fsprogs >= %s)", strings.TrimSpace(string(out)), MinMkfs)
	}
	if maj < 1 || (maj == 1 && min < 43) {
		return fmt.Errorf("mkfs.ext4 is e2fsprogs %d.%d, need >= %s for mkfs.ext4 -d", maj, min, MinMkfs)
	}
	return nil
}

func parseMkfsVersion(s string) (maj, min int, ok bool) {
	m := mkfsVersionRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	maj, _ = strconv.Atoi(m[1])
	min, _ = strconv.Atoi(m[2])
	return maj, min, true
}
