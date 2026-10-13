// SPDX-License-Identifier: MIT

package externals

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits on what an archive may unpack to. They are far above a Node.js
// release (about 6,500 entries and 220 MiB) and exist to bound a corrupt or
// hostile archive that still has the right checksum.
const (
	maxEntries = 200_000
	maxUnpack  = 2 << 30
)

// extractTarGz unpacks the .tar.gz at archive into the existing directory dest.
// With stripTop the first path component is dropped (the nodejs.org archives
// hold one top directory, node-vX-linux-x64); without it the names are taken as
// they are (the actions/alpine_nodejs archives start at ./bin/node). It refuses what could write or point outside dest:
//
//   - absolute names and names with ".." components;
//   - anything but directories, regular files, symbolic links and hard links;
//   - symbolic links that are absolute or whose target climbs with ".." anywhere
//     but at the start (so that the target cannot walk back out through another
//     link), and entries below a symbolic link;
//   - hard links to something that is not already unpacked.
//
// Writes go through os.Root, which also refuses to follow a link out of dest.
// File modes are reduced to 0755 / 0644: the owner of the files is whoever runs
// the runner, and the sandboxes only read and execute them.
func extractTarGz(archive, dest string, stripTop bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip file: %w", err)
	}
	defer func() { _ = gz.Close() }()
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(gz)
	links := map[string]bool{} // unpacked symbolic links
	var entries int
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return openDirs(dest)
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if entries++; entries > maxEntries {
			return fmt.Errorf("archive has more than %d entries", maxEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := cleanName(h.Name, stripTop)
		if err != nil {
			return err
		}
		if name == "" { // the top directory itself, or "./"
			continue
		}
		if under(links, name) {
			return fmt.Errorf("entry %q is below a symbolic link", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			if total += h.Size; total > maxUnpack {
				return fmt.Errorf("archive unpacks to more than %d bytes", int64(maxUnpack))
			}
			err = writeFile(root, name, tr, h.Mode)
		case tar.TypeSymlink:
			if err = checkLinkTarget(name, h.Linkname); err == nil {
				if err = root.MkdirAll(path.Dir(name), 0o755); err == nil {
					if err = root.Symlink(h.Linkname, name); err == nil {
						links[name] = true
					}
				}
			}
		case tar.TypeLink:
			var old string
			if old, err = cleanName(h.Linkname, stripTop); err == nil {
				if old == "" || under(links, old) {
					err = fmt.Errorf("hard link %q points at %q", h.Name, h.Linkname)
				} else if err = root.MkdirAll(path.Dir(name), 0o755); err == nil {
					err = root.Link(old, name)
				}
			}
		default:
			err = fmt.Errorf("entry %q has the unsupported type %q", h.Name, string(h.Typeflag))
		}
		if err != nil {
			return fmt.Errorf("unpack %q: %w", h.Name, err)
		}
	}
}

func writeFile(root *os.Root, name string, r io.Reader, mode int64) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil { // the umask may have removed bits
		_ = f.Close()
		return err
	}
	return f.Close()
}

// cleanName validates an archive path and returns it relative to the tree: "./"
// prefixes and empty or "." components are dropped, and with stripTop so is the
// first component. It returns "" for the top of the tree itself.
func cleanName(name string, stripTop bool) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("invalid entry name %q", name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("entry %q has an absolute path", name)
	}
	var parts []string
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".":
		case "..":
			return "", fmt.Errorf("entry %q has a .. component", name)
		default:
			parts = append(parts, p)
		}
	}
	if stripTop {
		if len(parts) <= 1 {
			return "", nil
		}
		parts = parts[1:]
	}
	return strings.Join(parts, "/"), nil
}

// checkLinkTarget accepts a relative target whose ".." components all come
// first and that stays inside the tree. name is the cleaned path of the link.
// The lexical rule is exact because no entry is ever unpacked below a symbolic
// link.
func checkLinkTarget(name, target string) error {
	if target == "" || strings.ContainsRune(target, 0) || strings.HasPrefix(target, "/") {
		return fmt.Errorf("symbolic link %q has the target %q: absolute or empty", name, target)
	}
	// depth is how many directories lie between the tree's top and the link.
	depth := 0
	if dir := path.Dir(name); dir != "." {
		depth = strings.Count(dir, "/") + 1
	}
	up, seenName := 0, false
	for _, p := range strings.Split(target, "/") {
		switch p {
		case "", ".":
		case "..":
			if seenName {
				return fmt.Errorf("symbolic link %q: target %q climbs after a name", name, target)
			}
			up++
		default:
			seenName = true
		}
	}
	if up > depth {
		return fmt.Errorf("symbolic link %q: target %q leaves the archive", name, target)
	}
	return nil
}

// under reports whether name, or one of its parents, is an unpacked symbolic link.
func under(links map[string]bool, name string) bool {
	for p := name; p != "." && p != "/" && p != ""; p = path.Dir(p) {
		if links[p] {
			return true
		}
	}
	return false
}

// openDirs gives every directory the mode 0755, whatever the umask of the
// runner was when it created them: the sandboxes read the tree as another user.
func openDirs(dest string) error {
	return filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o755)
	})
}
