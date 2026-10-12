// SPDX-License-Identifier: MIT

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// S3: one runner per host, whatever its state directory.
func TestHostLockIsExclusiveAndReleasable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run", "ermite-runner.lock")
	a, err := AcquireHostLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireHostLock(p); err == nil || !strings.Contains(err.Error(), "another ermite-runner is running on this host") {
		t.Fatalf("a second runner must be refused with a clear message, got %v", err)
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(); err != nil {
		t.Fatalf("double release: %v", err)
	}
	b, err := AcquireHostLock(p)
	if err != nil {
		t.Fatalf("the lock must be free after Release: %v", err)
	}
	_ = b.Release()
	var nilLock *HostLock
	if err := nilLock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestHostLockErrorNamesTheOverride(t *testing.T) {
	_, err := AcquireHostLock("/proc/nope/ermite.lock")
	if err == nil || !strings.Contains(err.Error(), EnvLockFile) {
		t.Fatalf("got %v", err)
	}
}

func TestConfigNewSettings(t *testing.T) {
	c, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.JobPids != 32768 || c.DenyPrivileged || c.LockFile != "/run/ermite-runner.lock" ||
		c.DiskBandwidthMiB != 400 || c.DiskIOPS != 20000 || c.NetMbit != 1000 {
		t.Fatalf("%+v", c)
	}
	c, err = LoadConfig(env(map[string]string{
		EnvJobPids: "1000", EnvDenyPrivileged: "1", EnvLockFile: "/tmp/x.lock",
		EnvJobDiskBW: "0", EnvJobDiskIOPS: "5000", EnvJobNetBandwidth: "100",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.JobPids != 1000 || !c.DenyPrivileged || c.LockFile != "/tmp/x.lock" || c.DiskBandwidthMiB != 0 || c.DiskIOPS != 5000 || c.NetMbit != 100 {
		t.Fatalf("%+v", c)
	}
	for name, m := range map[string]map[string]string{
		"pids low":    {EnvJobPids: "3"},
		"deny":        {EnvDenyPrivileged: "maybe"},
		"lock rel":    {EnvLockFile: "run/x.lock"},
		"bw negative": {EnvJobDiskBW: "-1"},
		"net not int": {EnvJobNetBandwidth: "fast"},
	} {
		if _, err := LoadConfig(env(m)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// C8: leaked slots stay taken until ReleaseLeaked; Busy ignores them.
func TestSlotsLeakedLifecycle(t *testing.T) {
	s := NewSlots(3)
	a, _ := s.Acquire()
	b, _ := s.Acquire()
	s.MarkLeaked(a)
	if s.InUse() != 2 || s.Busy() != 1 {
		t.Fatalf("InUse=%d Busy=%d", s.InUse(), s.Busy())
	}
	c, _ := s.Acquire()
	if c == a || c == b {
		t.Fatalf("a leaked slot must not be handed out again: %d", c)
	}
	s.Release(a) // a normal release also clears the leak flag
	if s.InUse() != 2 {
		t.Fatal(s.InUse())
	}
	s.MarkLeaked(b)
	s.ReleaseLeaked()
	if s.InUse() != 1 || s.Busy() != 1 {
		t.Fatalf("after ReleaseLeaked InUse=%d Busy=%d", s.InUse(), s.Busy())
	}
}

func TestErrLeakedIsMatchable(t *testing.T) {
	err := errors.Join(errors.New("remove container: busy"), ErrLeaked)
	if !errors.Is(err, ErrLeaked) {
		t.Fatal("errors.Is must see ErrLeaked through a join")
	}
}

// C7: Abort cancels a boot in progress and forbids a later one.
func TestBootGate(t *testing.T) {
	var g BootGate
	ctx, err := g.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(20 * time.Millisecond); g.Abort() }()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrAborted) {
			t.Fatalf("cause %v", context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Abort did not cancel the boot context")
	}
	g.End()
	if _, err := g.Begin(context.Background()); !errors.Is(err, ErrAborted) {
		t.Fatalf("Begin after Abort: %v", err)
	}
	// Parent cancellation still works, and End releases the context.
	var g2 BootGate
	parent, cancel := context.WithCancel(context.Background())
	c2, _ := g2.Begin(parent)
	cancel()
	<-c2.Done()
	g2.End()
	g2.End()
}

func TestWarnIfLowSpace(t *testing.T) {
	var buf bytes.Buffer
	ctx := logCtx(&buf)
	WarnIfLowSpace(ctx, "/x", 2, 64*1024, func(string) (int64, error) { return 10 * 1024, nil })
	if !strings.Contains(buf.String(), "less free space") || !strings.Contains(buf.String(), `"worst_case_mib":131072`) {
		t.Fatalf("expected a warning, got %q", buf.String())
	}
	buf.Reset()
	WarnIfLowSpace(ctx, "/x", 2, 1024, func(string) (int64, error) { return 10 * 1024, nil })
	if buf.Len() != 0 {
		t.Fatalf("no warning expected: %q", buf.String())
	}
	WarnIfLowSpace(ctx, "/x", 1, 1, func(string) (int64, error) { return 0, errors.New("statfs failed") })
	if !strings.Contains(buf.String(), "cannot check") {
		t.Fatalf("a statfs error must be logged: %q", buf.String())
	}
	if n, err := FreeMiB(t.TempDir()); err != nil || n <= 0 {
		t.Fatalf("FreeMiB: %d %v", n, err)
	}
}
