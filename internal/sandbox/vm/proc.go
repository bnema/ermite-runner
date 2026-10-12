// SPDX-License-Identifier: MIT

package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// firecrackerProcs scans a /proc tree for Firecracker processes started by this
// runner: argv[0] is "firecracker" and "--id" is followed by one of our ids
// (see newID). Processes of other users of Firecracker are never matched. It
// finds orphans even when the state directory was lost.
func firecrackerProcs(procDir string) map[int]string {
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
		b, err := os.ReadFile(filepath.Join(procDir, e.Name(), "cmdline"))
		if err != nil || len(b) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if filepath.Base(args[0]) != "firecracker" {
			continue
		}
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "--id" && sandbox.ValidID(args[i+1]) {
				out[pid] = args[i+1]
			}
		}
	}
	return out
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

// killAndWait sends SIGKILL to pid and waits until it is gone (zombies of
// children of this process are reaped by their own Wait).
func killAndWait(pid int, timeout time.Duration) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		// A zombie child still answers kill(0); check its state.
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
