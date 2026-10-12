// SPDX-License-Identifier: MIT

package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
}

func newScope(stateDir string) scope {
	base := jailBase(stateDir)
	sc := scope{jailBases: []string{base}}
	if r, err := filepath.EvalSymlinks(base); err == nil && r != base {
		sc.jailBases = append(sc.jailBases, r)
	}
	return sc
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
// A Firecracker started with --api-sock (no jailer) is ours by its id alone.
func (sc scope) match(dir string, args []string) string {
	a := parseVMMArgs(args)
	if a.id == "" || len(a.markers) == 0 {
		return ""
	}
	switch {
	case a.markers[MarkerAPISock]:
		return a.id
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

// killAndWait sends SIGKILL to pid and waits until it is gone (or a zombie that
// its parent will reap). It reports whether the process is gone.
func killAndWait(pid int, timeout time.Duration) bool {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(timeout)
	for {
		if !pidAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
