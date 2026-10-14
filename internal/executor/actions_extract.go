// SPDX-License-Identifier: MIT

package executor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits of an action archive. The runner has none (it runs tar); these keep a
// hostile archive from filling the sandbox. They are variables for the tests.
var (
	maxArchiveEntries = 200_000
	// maxArchiveBytes bounds the sizes of the regular files; maxStreamBytes the
	// whole decompressed tar stream, headers and PAX/GNU long-name records
	// included, which the entry and size counters do not see.
	maxArchiveBytes = int64(2 << 30)
	maxStreamBytes  = maxArchiveBytes + 64<<20
)

// errArchive is returned for an archive that is not safe or not valid. Its text
// is ermite's: GitHub's runner runs tar, which accepts absolute paths and links
// out of the tree (Runner.Worker/ActionManager.cs:1358-1394).
type errArchive string

func (e errArchive) Error() string { return string(e) }

// capReader fails once more than n bytes were read.
type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errArchive("the archive is too large once unpacked")
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// extractTarball unpacks a gzip-compressed tar into dir, which must exist and
// be empty, and strips the single top directory of the archive (GitHub's
// archives have exactly one, ActionManager.cs:1380-1393): the content lands in
// dir itself, so no file moves afterwards and a link that is valid here stays
// valid. Every name is checked: no absolute path, no "..", one top directory,
// no link whose target leaves dir, and everything is written through an
// os.Root, so that a link planted by an earlier entry cannot redirect a later
// one. The decompressed stream is capped, and ctx is checked between entries.
// Links are validated at install time: a running step can change the action
// tree like any file of its own sandbox, so this is not a confinement guarantee
// for what happens later.
// owner, when set, is the uid and gid of the files.
func extractTarball(ctx context.Context, r io.Reader, dir string, owner *[2]int) error {
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
	tr := tar.NewReader(&capReader{r: zr, n: maxStreamBytes})
	entries, total, top := 0, int64(0), ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return verifyLinks(ctx, root, dir)
		}
		if err != nil {
			var ea errArchive
			if errors.As(err, &ea) {
				return ea
			}
			return errArchive("the archive is not a valid tar file: " + err.Error())
		}
		if entries++; entries > maxArchiveEntries {
			return errArchive("the archive has too many entries")
		}
		if total += h.Size; total > maxArchiveBytes {
			return errArchive("the archive is too large once unpacked")
		}
		if err := extractEntry(root, tr, h, owner, &top); err != nil {
			return err
		}
	}
}

// stripTop validates the name of an entry, drops its top directory and returns
// the rest, clean and relative to the final root ("" for the top directory
// itself). All entries must share one top directory.
func stripTop(name string, top *string) (string, error) {
	c, err := cleanEntry(name)
	if err != nil {
		return "", err
	}
	if c == "" {
		return "", nil
	}
	first, rest, hasRest := strings.Cut(c, "/")
	if *top == "" {
		*top = first
	} else if first != *top {
		return "", errArchive(fmt.Sprintf("the archive has more than one top-level directory: %q and %q", *top, first))
	}
	if !hasRest {
		return "", nil
	}
	return rest, nil
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

func extractEntry(root *os.Root, tr *tar.Reader, h *tar.Header, owner *[2]int, top *string) error {
	switch h.Typeflag {
	case tar.TypeXGlobalHeader, tar.TypeXHeader:
		return nil // git archive's pax_global_header: the commit id
	}
	name, err := stripTop(h.Name, top)
	if err != nil {
		return err
	}
	if name == "" {
		if h.Typeflag != tar.TypeDir {
			return errArchive(fmt.Sprintf("the archive has a file next to its top-level directory: %q", h.Name))
		}
		return nil
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
		// Only the shape is checked here: where the link leads depends on the links
		// that come before and after it, so verifyLinks resolves every symlink of
		// the finished tree.
		if h.Linkname == "" || strings.HasPrefix(h.Linkname, "/") || strings.ContainsRune(h.Linkname, 0) {
			return errArchive(fmt.Sprintf("the archive has a link that points outside of it: %q -> %q", name, h.Linkname))
		}
		_ = root.Remove(name)
		if err := root.Symlink(h.Linkname, name); err != nil {
			return err
		}
		return chown()
	case tar.TypeLink:
		tgt, err := stripTop(h.Linkname, top)
		if err != nil || tgt == "" || !linkInside(name, tgt, true) {
			return errArchive(fmt.Sprintf("the archive has a link that points outside of it: %q -> %q", name, h.Linkname))
		}
		_ = root.Remove(name)
		return root.Link(tgt, name)
	}
	return nil // devices, fifos: skipped
}

// maxLinkHops bounds the symbolic links followed to resolve one link (Linux
// stops at 40).
const maxLinkHops = 40

// verifyLinks resolves every symbolic link of the extracted tree component by
// component, following the links met on the way, and rejects the archive when
// one leads out of the tree or loops. A lexical check is not enough: with
// "a -> ." a link "link -> a/../outside" looks like it stays in the tree, and
// leaves it. A link to something that does not exist is allowed (git archives
// have such links); the rest of its target is still resolved.
func verifyLinks(ctx context.Context, root *os.Root, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		target, err := root.Readlink(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if err := resolveInside(root, path.Dir(filepath.ToSlash(rel)), target); err != nil {
			return errArchive(fmt.Sprintf("the archive has a link that points outside of it: %q -> %q (%v)", filepath.ToSlash(rel), target, err))
		}
		return nil
	})
}

// resolveInside resolves target, relative to the directory dirRel of the tree
// (slash separated, "." for the root), the way the kernel would, and fails when
// the path leaves the root, is absolute, or needs more than maxLinkHops links.
func resolveInside(root *os.Root, dirRel, target string) error {
	var cur []string // resolved directory components, none of them a link
	if dirRel != "." && dirRel != "" {
		cur = strings.Split(dirRel, "/")
	}
	pending := strings.Split(target, "/")
	hops := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return errors.New("leaves the tree")
			}
			cur = cur[:len(cur)-1]
			continue
		}
		p := strings.Join(append(append([]string(nil), cur...), c), "/")
		fi, err := root.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			cur = append(cur, c)
			continue
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink == 0:
			cur = append(cur, c)
			continue
		}
		if hops++; hops > maxLinkHops {
			return errors.New("too many levels of links")
		}
		next, err := root.Readlink(p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(next, "/") || next == "" {
			return errors.New("an absolute or empty link")
		}
		pending = append(strings.Split(next, "/"), pending...)
	}
	return nil
}
