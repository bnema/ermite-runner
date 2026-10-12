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

// vmmProcs scans a /proc tree for processes started by this runner for a VM:
// the Firecracker process and the jailer. A process is ours when its command
// line has "--id <id>" with an id of the form sandbox.NewID produces and also
// "--api-sock" (Firecracker) or "--exec-file" (jailer). The program name is not
// looked at: the binary may be renamed or versioned (firecracker-v1.17), and a
// jailer in a PID namespace leaves a Firecracker whose argv[0] is whatever the
// jailer chose. Processes of other users of Firecracker (other ids) are never
// matched. It finds orphans even when the state directory was lost.
func vmmProcs(procDir string) map[int]string {
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
		if id := vmIDFromCmdline(strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")); id != "" {
			out[pid] = id
		}
	}
	return out
}

// vmIDFromCmdline returns the VM id of a command line, or "".
func vmIDFromCmdline(args []string) string {
	var id string
	var marker bool
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--id":
			if i+1 < len(args) && sandbox.ValidID(args[i+1]) {
				id = args[i+1]
			}
		case "--api-sock", "--exec-file":
			marker = true
		}
	}
	if marker {
		return id
	}
	return ""
}

// readPidFile returns the pid the jailer wrote for a VM run in a PID namespace.
func readPidFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
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
