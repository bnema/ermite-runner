// SPDX-License-Identifier: MIT

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// HostLock is the host-wide single-instance lock. Reaping removes every process,
// device, firewall rule and Docker object labelled as the runner's own, so it is
// only safe when no other runner of the same host is alive: the lock lives at a
// fixed path of the host (DefaultLockFile), not in a state directory, which two
// runners could choose differently, and it is taken by both isolation modes.
type HostLock struct{ f *os.File }

// AcquireHostLock takes the lock at path (the default when empty) without
// waiting. The kernel releases it when the process dies, so a crash never leaves
// it stuck.
func AcquireHostLock(path string) (*HostLock, error) {
	if path == "" {
		path = DefaultLockFile
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("host lock %s: %w (set %s for another path)", path, err, EnvLockFile)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("host lock %s: %w (set %s for another path)", path, err, EnvLockFile)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another ermite-runner is running on this host (lock %s): only one runner per host is supported, because reaping removes everything the runner owns: %w", path, err)
	}
	return &HostLock{f: f}, nil
}

// Release drops the lock; releasing twice is fine.
func (l *HostLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
