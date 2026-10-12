// SPDX-License-Identifier: MIT

package disk

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

// Mounter loop-mounts and unmounts scratch disks. It needs root (loop devices),
// like the rest of the container mode.
type Mounter struct {
	Run cmdrun.Runner
	// Mountinfo is /proc/self/mountinfo; tests point it at a fixture.
	Mountinfo string
}

// Mount loop-mounts the ext4 file at dir (which must exist). The mount is
// deliberately not nosuid/nodev: the nested dockerd keeps its overlay2 layers
// here, and overlayfs opens the real files of a layer through the layer's own
// mount, so device nodes and setuid binaries of container images would stop
// working on a nodev/nosuid backing mount.
func (m Mounter) Mount(ctx context.Context, file, dir string) error {
	if _, err := m.Run.Run(ctx, nil, "mount", "-o", "loop,noatime", "-t", "ext4", file, dir); err != nil {
		return fmt.Errorf("loop-mount scratch disk: %w", err)
	}
	return nil
}

// Unmount unmounts dir; the loop device is released with it (mount -o loop sets
// autoclear). A directory that is not a mount point is not an error.
func (m Mounter) Unmount(ctx context.Context, dir string) error {
	mounted, err := m.IsMounted(dir)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if _, err := m.Run.Run(ctx, nil, "umount", dir); err != nil {
		return fmt.Errorf("unmount scratch disk: %w", err)
	}
	return nil
}

// IsMounted reports whether dir is a mount point.
func (m Mounter) IsMounted(dir string) (bool, error) {
	mp, err := m.MountPoints()
	if err != nil {
		return false, err
	}
	for _, p := range mp {
		if p == dir {
			return true, nil
		}
	}
	return false, nil
}

// MountPoints lists every mount point of the mount namespace.
func (m Mounter) MountPoints() ([]string, error) {
	path := m.Mountinfo
	if path == "" {
		path = "/proc/self/mountinfo"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if mp, ok := mountPointOf(sc.Text()); ok {
			out = append(out, mp)
		}
	}
	return out, sc.Err()
}

// mountPointOf extracts the mount point (5th field) of a mountinfo line, with
// the kernel's octal escapes (\040 for a space) decoded.
func mountPointOf(line string) (string, bool) {
	f := strings.Fields(line)
	if len(f) < 5 {
		return "", false
	}
	return unescapeMountinfo(f[4]), true
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, ok := octal3(s[i+1 : i+4]); ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octal3(s string) (byte, bool) {
	if len(s) != 3 {
		return 0, false
	}
	var v int
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v*8 + int(c-'0')
	}
	return byte(v), v < 256
}
