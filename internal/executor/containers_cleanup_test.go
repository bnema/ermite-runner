// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fastTimeouts(c *Config) {
	c.Actions.Containers = ContainerTimeouts{Command: 400 * time.Millisecond, Pull: 400 * time.Millisecond, CleanupCommand: 400 * time.Millisecond, CleanupBudget: 3 * time.Second}
}

func names(l [][]string) string {
	var v []string
	for _, c := range l {
		v = append(v, c[0])
	}
	return strings.Join(v, " ")
}

// GO-001: a docker command that stalls is stopped by its own limit, the job
// does not wait for its timeout, and the cleanup still finds the container by
// the name chosen before the create.
func TestStalledCreateIsBoundedAndCleanedByName(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_create", "1")
	start := time.Now()
	rec, res := ctrRun(t, f, ctrJob, "      - run: echo never\n", fastTimeouts)
	if res.Conclusion != ConclusionFailure || time.Since(start) > 20*time.Second {
		t.Fatalf("%+v after %s", res, time.Since(start))
	}
	log := logOf(rec.step(t, "Initialize containers"))
	if !strings.Contains(log, "##[error]The docker command timed out after 400ms: create --name ") {
		t.Errorf("init:\n%s", log)
	}
	var rm []string
	for _, c := range f.calls(t) {
		if c[0] == "rm" {
			rm = c
		}
	}
	if len(rm) < 3 || !strings.Contains(rm[2], "_ubuntu2404_") || strings.Contains(rm[2], strings.Repeat("0", 20)) {
		t.Errorf("rm by name expected: %v", rm)
	}
	if got := logOf(rec.step(t, "Stop containers")); !strings.Contains(got, "Remove container network: github_network_") {
		t.Errorf("stop:\n%s", got)
	}
}

// GO-001: the cancellation of the job reaches every docker call of the init.
func TestCancelDuringInitStopsTheCommandAndCleans(t *testing.T) {
	for _, verb := range []string{"version", "inspect", "port", "start", "ps"} {
		f := newFakeCtr(t)
		f.set(t, "hang_"+verb, "1")
		cancel := NewCanceller()
		go func() { time.Sleep(500 * time.Millisecond); cancel.Cancel(CancelUser) }()
		start := time.Now()
		rec, res := ctrRunC(t, f, cancel, "    container: ubuntu:24.04\n    services:\n      s:\n        image: redis:7\n", "      - run: echo never\n", func(c *Config) {
			c.Actions.Containers.Command = time.Minute
		})
		if res.Conclusion != ConclusionCancelled || time.Since(start) > 15*time.Second {
			t.Fatalf("%s: %+v after %s %v", verb, res, time.Since(start), rec.names())
		}
		if got := rec.concl("Stop containers"); got != ConclusionSuccess {
			t.Errorf("%s: Stop containers %s", verb, got)
		}
	}
}

// GO-002: one removal that stalls does not keep the others and the network from
// being removed; the logs of a service never prevent its removal; the
// failures are reported by the step.
func TestCleanupAttemptsEverythingWithItsOwnBudget(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_logs", "1")
	f.set(t, "hang_for", "rm --force "+strings.Repeat("0", 63)+"1") // the rm of the job container (first id)
	cancel := NewCanceller()
	rec, res := ctrRunC(t, f, cancel, "    container: ubuntu:24.04\n    services:\n      a:\n        image: redis:7\n      b:\n        image: redis:7\n", "      - run: echo ok\n", fastTimeouts)
	if res.Conclusion == ConclusionSuccess {
		t.Fatalf("a failed cleanup must be visible: %+v", res)
	}
	stop := logOf(rec.step(t, "Stop containers"))
	for _, w := range []string{"Stop and remove container: ", "Print service container logs: ", "Remove container network: github_network_", "##[error]The docker command timed out after 400ms: rm --force ", "##[warning]Docker logs failed: The docker command timed out"} {
		if !strings.Contains(stop, w) {
			t.Errorf("stop lacks %q:\n%s", w, stop)
		}
	}
	rms, nets := 0, 0
	for _, c := range f.calls(t) {
		if c[0] == "rm" {
			rms++
		}
		if c[0] == "network" && len(c) > 1 && c[1] == "rm" {
			nets++
		}
	}
	// 3 removals, and the fallback of the job tries the one that timed out again.
	if rms < 3 || rms > 4 || nets != 1 {
		t.Errorf("rm=%d network rm=%d (want 3 or 4 and 1):\n%s", rms, nets, stop)
	}
	if rec.concl("Stop containers") != ConclusionFailure {
		t.Errorf("Stop containers: %s", rec.concl("Stop containers"))
	}
}

// GO-003: a shutdown skips the queued post steps (StepsRunner); the job-level
// fallback removes the containers and the network all the same, and does not
// run twice what "Stop containers" did.
func TestShutdownStillCleansUpThroughTheFallback(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_exec", "1")
	cancel := NewCanceller()
	go func() { time.Sleep(600 * time.Millisecond); cancel.Cancel(CancelShutdown) }()
	rec, res := ctrRunC(t, f, cancel, ctrJob, "      - name: long\n        run: sleep 60\n", nil)
	_ = res
	if rec.concl("Stop containers") == ConclusionSuccess {
		t.Skip("the runner ran Stop containers on shutdown")
	}
	got := names(f.calls(t))
	if !strings.HasSuffix(got, "rm network") {
		t.Errorf("fallback did not remove the container and the network: %s", got)
	}
}

func TestFallbackDoesNothingWhenStopContainersDidItAll(t *testing.T) {
	f := newFakeCtr(t)
	_, res := ctrRun(t, f, ctrJob, "      - run: echo ok\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	n := 0
	for _, c := range f.calls(t) {
		if c[0] == "rm" || (c[0] == "network" && len(c) > 1 && c[1] == "rm") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d removals, want exactly 2 (container, network): %s", n, names(f.calls(t)))
	}
}

// GO-003: a panic of the job still cleans up.
func TestPanicStillCleansUp(t *testing.T) {
	f := newFakeCtr(t)
	defer func() {
		_ = recover()
		got := names(f.calls(t))
		if !strings.Contains(got, "rm") || !strings.HasSuffix(got, "rm network") {
			t.Errorf("calls after the panic: %s", got)
		}
	}()
	ctrRun(t, f, ctrJob, "      - run: echo ok\n", func(c *Config) {
		c.Reporter = &panicOnStep{Reporter: c.Reporter, name: "Run echo ok"}
	})
}

type panicOnStep struct {
	Reporter
	name string
}

func (p *panicOnStep) StepStarted(n int, name string, at time.Time) {
	if name == p.name {
		panic("boom")
	}
	p.Reporter.StepStarted(n, name, at)
}

// GO-007: the credentials directory is removed even when the pull fails; a
// removal that fails is reported and left to the fallback.
func TestRegistryConfigRemovedAfterFailedPullAndRemovalFailureIsReported(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "pull_fail", "9")
	var temp string
	rec, _ := ctrRun(t, f, "    container:\n      image: ghcr.io/o/i:1\n      credentials:\n        username: u\n        password: pw-secret\n", "      - run: echo\n", func(c *Config) { temp = c.Layout.Temp })
	if m, _ := filepath.Glob(filepath.Join(temp, ".docker_*")); len(m) != 0 {
		t.Errorf("config left after a failed pull: %v", m)
	}
	_ = rec

	if os.Geteuid() == 0 {
		t.Skip("root removes anything")
	}
	f2 := newFakeCtr(t)
	f2.set(t, "lock_cfg", "1")
	rec2, res2 := ctrRun(t, f2, "    container:\n      image: ghcr.io/o/i:1\n      credentials:\n        username: u\n        password: pw-secret\n", "      - run: echo\n", func(c *Config) { temp = c.Layout.Temp })
	defer func() {
		m, _ := filepath.Glob(filepath.Join(temp, ".docker_*", "sub"))
		for _, d := range m {
			_ = os.Chmod(d, 0o755)
		}
	}()
	if res2.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res2)
	}
	if got := logOf(rec2.step(t, "Initialize containers")); !strings.Contains(got, "##[error]Failed to remove directory containing Docker client credentials:") {
		t.Errorf("init:\n%s", got)
	}
}
