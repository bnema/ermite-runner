// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

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

// ensureExternalsDisk builds (or reuses) the read-only ext4 disk that carries
// the host's external tools (Node.js): one directory per tool at its root, so
// that mounted at /home/runner/externals it gives the GitHub layout
// externals/node24/bin/node. It is named after the content of the set
// (sandbox.ExternalsKey), so a new Node.js version builds a new disk and leaves
// the exec disk and the base image alone. It returns "" for an empty set. The
// caller holds the driver's externals lock, so that two jobs do not build the
// same disk.
func ensureExternalsDisk(ctx context.Context, run cmdrun.Runner, set []sandbox.External, stateDir string) (string, error) {
	if len(set) == 0 {
		return "", nil
	}
	cache := filepath.Join(stateDir, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	name := "extdisk-" + sandbox.ExternalsKey(set) + ".ext4"
	dest := filepath.Join(cache, name)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	// Drop disks of other contents, once they are an hour old: without the
	// jailer a VM that is about to start may still be about to open the previous
	// one in place (with the jailer it holds a hard link).
	olds, _ := filepath.Glob(filepath.Join(cache, "extdisk-*"))
	for _, o := range olds {
		if fi, err := os.Stat(o); err == nil && time.Since(fi.ModTime()) > time.Hour {
			_ = os.Remove(o)
		}
	}

	tmp, err := os.MkdirTemp(cache, "extroot-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var size, files int64
	for _, e := range set {
		sz, n, err := cloneTree(e.Dir, filepath.Join(tmp, e.Name))
		if err != nil {
			return "", fmt.Errorf("stage %s: %w", e.Name, err)
		}
		size, files = size+sz, files+n
	}

	part := dest + ".part"
	_ = os.Remove(part)
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	// The files, ext4 overhead (inodes, directories, block rounding of many
	// small files) and some room; the disk is read-only and sparse.
	mib := size/(1<<20)*125/100 + files*4096/(1<<20) + 16
	if err := f.Truncate(mib << 20); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	if _, err := run.Run(ctx, nil, "mkfs.ext4", "-q", "-F", "-L", "ermite-ext", "-m", "0",
		"-O", "^has_journal", "-N", strconv.FormatInt(files*13/10+256, 10),
		"-E", "root_owner=0:0", "-d", tmp, part); err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("build externals disk: %w", err)
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// cloneTree recreates the tree src at dst (directories, regular files, symbolic
// links) for mkfs.ext4 -d, hard-linking files when the filesystem allows and
// copying them otherwise. It returns the bytes of file content and the number
// of entries.
func cloneTree(src, dst string) (size, entries int64, err error) {
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		entries++
		switch {
		case d.IsDir():
			if err := os.MkdirAll(to, 0o755); err != nil {
				return err
			}
			return os.Chmod(to, 0o755) // whatever the umask
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			size += fi.Size()
			if err := os.Link(p, to); err == nil {
				return nil
			}
			if err := copyFile(p, to); err != nil {
				return err
			}
			return os.Chmod(to, fi.Mode().Perm())
		}
		return fmt.Errorf("%s: unsupported file type %v", p, d.Type())
	})
	return size, entries, err
}
