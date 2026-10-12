// SPDX-License-Identifier: MIT

package vm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// Command-line markers of a VMM started for a VM. A process is one when it has
// "--id <id>" (an id of the form sandbox.NewID produces) and at least one of
// them. One set for the driver's scan and for the integration tests' leak check.
const (
	// MarkerAPISock: Firecracker started without the jailer.
	MarkerAPISock = "--api-sock"
	// MarkerExecFile: the jailer.
	MarkerExecFile = "--exec-file"
	// MarkerStartTime: the Firecracker the jailer execs into its chroot; it gets
	// neither of the first two, only --id and the jailer's --start-time-* flags.
	MarkerStartTime = "--start-time-us"
)

// VMMMarkers lists every marker.
var VMMMarkers = []string{MarkerAPISock, MarkerExecFile, MarkerStartTime}

// vmmArgs is what the scan reads from a command line.
type vmmArgs struct {
	id         string
	markers    map[string]bool
	chrootBase string // the jailer's --chroot-base-dir
	apiSock    string // Firecracker's --api-sock value
}

func parseVMMArgs(args []string) vmmArgs {
	a := vmmArgs{markers: map[string]bool{}}
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--id":
			if i+1 < len(args) && sandbox.ValidID(args[i+1]) {
				a.id = args[i+1]
			}
		case args[i] == "--chroot-base-dir":
			if i+1 < len(args) {
				a.chrootBase = args[i+1]
			}
		case slices.Contains(VMMMarkers, args[i]):
			a.markers[args[i]] = true
			if args[i] == MarkerAPISock && i+1 < len(args) {
				a.apiSock = args[i+1]
			}
		}
	}
	return a
}

// IDFromCmdline returns the VM id of a command line that carries one of
// VMMMarkers, or "". It looks at the arguments only; the driver's scan narrows
// it to its own state directory (scope.match).
func IDFromCmdline(args []string) string {
	a := parseVMMArgs(args)
	if len(a.markers) == 0 {
		return ""
	}
	return a.id
}

// scope says which VMM processes are this driver's: the jailed ones must live in
// (or have been started for) its own jailer chroot base, <state dir>/jail.
type scope struct {
	jailBases []string // the base as configured, and with symlinks resolved
	vmBases   []string // <state dir>/vm (unjailed VMs), likewise
}

func newScope(stateDir string) scope {
	sc := scope{}
	for _, b := range []struct {
		dir string
		to  *[]string
	}{{jailBase(stateDir), &sc.jailBases}, {filepath.Join(stateDir, "vm"), &sc.vmBases}} {
		*b.to = []string{b.dir}
		if r, err := filepath.EvalSymlinks(b.dir); err == nil && r != b.dir {
			*b.to = append(*b.to, r)
		}
	}
	return sc
}

// apiSockIsOurs: an unjailed Firecracker's --api-sock must be a path below
// <state dir>/vm/<id>/ (compared lexically, after cleaning), the directory the
// driver puts it in. Another Firecracker with an id of our shape and a socket
// elsewhere is someone else's.
func (sc scope) apiSockIsOurs(sock, id string) bool {
	if sock == "" || !filepath.IsAbs(sock) {
		return false
	}
	sock = filepath.Clean(sock)
	for _, b := range sc.vmBases {
		if strings.HasPrefix(sock, filepath.Join(b, id)+"/") {
			return true
		}
	}
	return false
}

func (sc scope) isBase(p string) bool {
	return slices.Contains(sc.jailBases, filepath.Clean(p))
}

func (sc scope) under(p string) bool {
	for _, b := range sc.jailBases {
		if strings.HasPrefix(filepath.Clean(p), b+"/") {
			return true
		}
	}
	return false
}

// match returns the VM id of the process whose command line is args and whose
// /proc directory is dir, or "". Besides IDFromCmdline:
//   - a jailer (--exec-file) must have been given our --chroot-base-dir;
//   - a Firecracker that carries only the jailer's --start-time-us (no
//     --api-sock) is the jailed VMM: its root must be the chroot of that id
//     below our jail base (rootIsOurs). Anything else with that flag and an id
//     of our shape belongs to someone else's jailer and is never touched.
//
// A Firecracker started with --api-sock (no jailer) is ours when the socket is
// in <state dir>/vm/<id>/ (apiSockIsOurs).
func (sc scope) match(dir string, args []string) string {
	a := parseVMMArgs(args)
	if a.id == "" || len(a.markers) == 0 {
		return ""
	}
	switch {
	case a.markers[MarkerAPISock]:
		if sc.apiSockIsOurs(a.apiSock, a.id) {
			return a.id
		}
	case a.markers[MarkerExecFile]:
		if sc.isBase(a.chrootBase) {
			return a.id
		}
	case a.markers[MarkerStartTime]:
		if sc.rootIsOurs(dir, a.id) {
			return a.id
		}
	}
	return ""
}

// rootIsOurs reports whether the process's root directory is the chroot the
// jailer made for id below our jail base. The jailer pivot_roots into a new
// mount namespace, so readlink(/proc/<pid>/root) says "/" and cannot be used:
// stat follows the magic link into the VM's root, which is the very directory
// <base>/<exec name>/<id>/root of the host (same device and inode). A root
// whose path is lexically below our base is accepted too.
func (sc scope) rootIsOurs(dir, id string) bool {
	if root, err := os.Readlink(filepath.Join(dir, "root")); err == nil && sc.under(root) {
		return true
	}
	rootFI, err := os.Stat(filepath.Join(dir, "root"))
	if err != nil {
		return false
	}
	for _, b := range sc.jailBases {
		execDirs, _ := os.ReadDir(b)
		for _, e := range execDirs {
			if fi, err := os.Stat(filepath.Join(b, e.Name(), id, "root")); err == nil && os.SameFile(fi, rootFI) {
				return true
			}
		}
	}
	return false
}

// vmmProcs scans a /proc tree for processes started by this runner for a VM:
// the Firecracker process and the jailer. A process is ours when its command
// line has "--id <id>" with an id of the form sandbox.NewID produces and one of
// the VMMMarkers, and it passes scope.match. The program name is not looked at:
// the binary may be renamed or versioned (firecracker-v1.17), and a jailer in a
// PID namespace leaves a Firecracker whose argv[0] is whatever the jailer
// chose. Processes of other users of Firecracker (other ids, other jail bases)
// are never matched. It finds orphans even when the state directory's records
// were lost.
func vmmProcs(procDir string, sc scope) map[int]string {
	out := map[int]string{}
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		dir := filepath.Join(procDir, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(b) == 0 {
			continue
		}
		if id := sc.match(dir, strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")); id != "" {
			out[pid] = id
		}
	}
	return out
}

// pidAlive reports whether pid exists and is not a zombie.
func pidAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// ring keeps the last N bytes written to it: the VMM's stdout/stderr, which
// carries the guest serial console, for diagnostics when a boot fails.
type ring struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.max:]...)
	}
	return len(p), nil
}

// String returns the retained tail, trimmed.
func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(bytes.TrimSpace(r.buf))
}

// killAndWait kills the process that was identified as pid, through a pidfd, and
// waits for it to exit. It reports whether that process is gone.
//
// A pid read from /proc a moment ago may belong to another process by now (the
// VMM exited and the number was recycled). The pidfd pins the process; then
// still() re-runs the identity check (command line and root scope) on the pid,
// and only if the pidfd is still not readable (the process had not exited at
// that point, so the pid could not have been recycled while still() read it) the
// signal goes out through the pidfd, which can only reach that process. A
// mismatch means the process we meant is gone: nothing is signalled and true is
// returned. Where pidfds are not available the process is not touched and false
// is returned (the sandbox is then reported as leaked, never killed blindly).
func killAndWait(pid int, still func() bool, timeout time.Duration) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return errors.Is(err, unix.ESRCH) // no such process: gone
	}
	defer func() { _ = unix.Close(fd) }()
	ours := still()
	if exited, _ := pidfdWait(fd, 0); exited {
		return true // it exited before or while we checked: nothing to kill
	}
	if !ours {
		return true // alive, but not the process we meant
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return false
	}
	exited, _ := pidfdWait(fd, timeout)
	return exited
}

// pidfdWait waits up to timeout for the process behind fd to exit (a pidfd is
// readable once it has, zombie included).
func pidfdWait(fd int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left < 0 {
			left = 0
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left.Milliseconds()))
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return false, err
		case n > 0:
			return true, nil
		}
		return false, nil
	}
}
