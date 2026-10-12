// SPDX-License-Identifier: MIT

package vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// layout is where the files of one VM live and how Firecracker names them.
//
// With the jailer, Firecracker sees only its chroot: <jailBase>/firecracker/<id>/root,
// so shared files are hard-linked into it and named "/name"; per-job files are
// created there and handed to the jailed uid. Without the jailer (development
// and tests), Firecracker sees the real filesystem: shared files are used in
// place and per-job files live in <stateDir>/vm/<id>.
type layout struct {
	jailed bool
	root   string // host path of the VM's working directory
	dir    string // the directory removed whole at cleanup
	uid    int
}

// newLayout: execName is the base name of the Firecracker binary, which the
// jailer uses as the first directory below its chroot base.
func newLayout(stateDir, execName, id string, jailed bool, uid int) layout {
	if jailed {
		dir := filepath.Join(stateDir, "jail", execName, id)
		return layout{jailed: true, root: filepath.Join(dir, "root"), dir: dir, uid: uid}
	}
	dir := filepath.Join(stateDir, "vm", id)
	return layout{root: dir, dir: dir, uid: uid}
}

// jailBase is the --chroot-base-dir given to the jailer.
func jailBase(stateDir string) string { return filepath.Join(stateDir, "jail") }

// host returns the host path of a per-job file called name.
func (l layout) host(name string) string { return filepath.Join(l.root, name) }

// fc returns the path Firecracker must be given for a per-job file.
func (l layout) fc(name string) string {
	if l.jailed {
		return "/" + name
	}
	return l.host(name)
}

// share makes the read-only shared file src available to Firecracker and
// returns its path for Firecracker. In a jail it is hard-linked, which requires
// the same filesystem as the state directory, and src must be readable by the
// jailed uid (the link shares the inode: it is never chowned).
func (l layout) share(src, name string) (string, error) {
	if !l.jailed {
		return src, nil
	}
	dst := l.host(name)
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return "", fmt.Errorf("cannot link %s into the jail: it must be on the same filesystem as the state directory (hard link across filesystems)", src)
		}
		return "", fmt.Errorf("link %s into the jail: %w", src, err)
	}
	return "/" + name, nil
}

// own hands a per-job file in the jail to the jailed uid; no-op without jailer.
// It never follows a symlink (Lchown): the jail is writable by the jailed uid
// once Firecracker runs, so a link planted there must not redirect the chown.
func (l layout) own(name string) error {
	if !l.jailed {
		return nil
	}
	return os.Lchown(l.host(name), l.uid, l.uid)
}

// checkReadable verifies that other users can read the shared file, which the
// jailed uid needs. Called once per file at driver start for jailed mode.
func checkWorldReadable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o004 == 0 {
		return fmt.Errorf("%s is not world-readable (mode %v): the jailed uid could not open it", path, st.Mode().Perm())
	}
	return nil
}

// copyFile copies src to dst (mode 0600) and fsyncs it.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
