// SPDX-License-Identifier: MIT

package executor

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Limits of an action archive. The runner has none (it runs tar); these keep a
// hostile archive from filling the sandbox.
const (
	maxArchiveEntries = 200_000
	maxArchiveBytes   = 2 << 30 // unpacked
)

// errArchive is returned for an archive that is not safe or not valid. Its text
// is ermite's: GitHub's runner runs tar, which accepts absolute paths and links
// out of the tree (Runner.Worker/ActionManager.cs:1358-1394).
type errArchive string

func (e errArchive) Error() string { return string(e) }

// extractTarball unpacks a gzip-compressed tar into dir, which must exist and
// be empty. Every name is checked: no absolute path, no "..", no link whose
// target leaves dir, and everything is written through an os.Root, so that a
// link planted by an earlier entry cannot redirect a later one. owner, when
// set, is the uid and gid of the files.
func extractTarball(r io.Reader, dir string, owner *[2]int) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return errArchive("the archive is not a gzip file: " + err.Error())
	}
	defer func() { _ = zr.Close() }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(zr)
	entries, total := 0, int64(0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errArchive("the archive is not a valid tar file: " + err.Error())
		}
		if entries++; entries > maxArchiveEntries {
			return errArchive("the archive has too many entries")
		}
		if total += h.Size; total > maxArchiveBytes {
			return errArchive("the archive is too large once unpacked")
		}
		if err := extractEntry(root, tr, h, owner); err != nil {
			return err
		}
	}
}

// cleanEntry validates the name of an entry and returns it clean and relative.
func cleanEntry(name string) (string, error) {
	if strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", errArchive(fmt.Sprintf("the archive has an absolute or invalid path: %q", name))
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", errArchive(fmt.Sprintf("the archive has a path that leaves its directory: %q", name))
		}
	}
	c := path.Clean(name)
	if c == "." {
		return "", nil
	}
	return c, nil
}

// linkInside reports whether a link at name pointing to target stays in the
// tree: target is relative to the directory of name for a symlink, and to the
// root for a hard link.
func linkInside(name, target string, hard bool) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return false
	}
	base := path.Dir(name)
	if hard {
		base = "."
	}
	t := path.Join(base, target)
	return t != ".." && !strings.HasPrefix(t, "../")
}

func extractEntry(root *os.Root, tr *tar.Reader, h *tar.Header, owner *[2]int) error {
	switch h.Typeflag {
	case tar.TypeXGlobalHeader, tar.TypeXHeader:
		return nil // git archive's pax_global_header: the commit id
	}
	name, err := cleanEntry(h.Name)
	if err != nil || name == "" {
		return err
	}
	perm := os.FileMode(h.Mode) & 0o777
	chown := func() error {
		if owner == nil {
			return nil
		}
		return root.Lchown(name, owner[0], owner[1])
	}
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return errArchive(fmt.Sprintf("cannot create the directory of %q: %v", name, err))
		}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(name, 0o755); err != nil {
			return err
		}
		if err := root.Chmod(name, perm|0o700); err != nil {
			return err
		}
		return chown()
	case tar.TypeReg:
		_ = root.Remove(name)
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm|0o600)
		if err != nil {
			return errArchive(fmt.Sprintf("cannot create %q: %v", name, err))
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return errArchive("the archive is not a valid tar file: " + err.Error())
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := root.Chmod(name, perm|0o600); err != nil {
			return err
		}
		return chown()
	case tar.TypeSymlink:
		if !linkInside(name, h.Linkname, false) {
			return errArchive(fmt.Sprintf("the archive has a link that points outside of it: %q -> %q", name, h.Linkname))
		}
		_ = root.Remove(name)
		if err := root.Symlink(h.Linkname, name); err != nil {
			return err
		}
		return chown()
	case tar.TypeLink:
		tgt, err := cleanEntry(h.Linkname)
		if err != nil || tgt == "" || !linkInside(name, tgt, true) {
			return errArchive(fmt.Sprintf("the archive has a link that points outside of it: %q -> %q", name, h.Linkname))
		}
		_ = root.Remove(name)
		return root.Link(tgt, name)
	}
	return nil // devices, fifos: skipped
}
