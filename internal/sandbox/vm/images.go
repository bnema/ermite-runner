// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// ensureExecDisk builds (or reuses) the small read-only ext4 disk that carries
// the runner's own executable: the guest root during boot, and /opt/ermite
// afterwards. It is named after the executable's hash, so a runner upgrade
// builds a new one and no image rebuild is needed; older ones are removed.
// mkfs.ext4 -d needs no root.
func ensureExecDisk(ctx context.Context, run cmdrun.Runner, exe, stateDir string) (string, error) {
	sum, size, err := hashFile(exe)
	if err != nil {
		return "", fmt.Errorf("hash executable: %w", err)
	}
	cache := filepath.Join(stateDir, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	name := "execdisk-" + sum[:16] + ".ext4"
	dest := filepath.Join(cache, name)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	// Drop disks of other executable versions.
	if olds, _ := filepath.Glob(filepath.Join(cache, "execdisk-*")); len(olds) > 0 {
		for _, o := range olds {
			_ = os.Remove(o)
		}
	}

	tmp, err := os.MkdirTemp(cache, "execroot-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, d := range []string{"proc", "sys", "dev", "mnt/base", "mnt/scratch", "mnt/newroot"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			return "", err
		}
	}
	if err := copyFile(exe, filepath.Join(tmp, sandbox.GuestExecName)); err != nil {
		return "", fmt.Errorf("copy executable: %w", err)
	}
	if err := os.Chmod(filepath.Join(tmp, sandbox.GuestExecName), 0o755); err != nil {
		return "", err
	}

	part := dest + ".part"
	_ = os.Remove(part)
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	// The executable, ext4 overhead and some room; the disk is read-only.
	mib := size/(1<<20)*115/100 + 16
	if err := f.Truncate(mib << 20); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	if _, err := run.Run(ctx, nil, "mkfs.ext4", "-q", "-F", "-L", "ermite-exec",
		"-O", "^has_journal", "-E", "root_owner=0:0", "-d", tmp, part); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("build exec disk: %w", err)
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func hashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
