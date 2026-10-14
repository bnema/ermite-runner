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
// does not wait for its timeout, and the cleanup still finds the container
// that the daemon did create, by the label of the job.
func TestStalledCreateIsBoundedAndCleanedByLabel(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_create_after", "1") // created in the daemon, the CLI never returns
	start := time.Now()
	rec, res := ctrRun(t, f, ctrJob, "      - run: echo never\n", fastTimeouts)
	if res.Conclusion != ConclusionFailure || time.Since(start) > 20*time.Second {
		t.Fatalf("%+v after %s", res, time.Since(start))
	}
	log := logOf(rec.step(t, "Initialize containers"))
	if !strings.Contains(log, "##[error]The docker command timed out after 400ms: create --name ") {
		t.Errorf("init:\n%s", log)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "created")); strings.TrimSpace(string(b)) != "" {
		t.Errorf("container left in the daemon: %q", b)
	}
	if got := logOf(rec.step(t, "Stop containers")); !strings.Contains(got, "Remove container network: github_network_") {
		t.Errorf("stop:\n%s", got)
	}
}

// GO-003-R: options: may carry another --name (the runner passes it on). The
// container is then not named as recorded; it is found by the label.
func TestOverridingNameAndTimedOutCreateAreCleanedByLabel(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_create_after", "1")
	rec, _ := ctrRun(t, f, "    container:\n      image: ubuntu:24.04\n      options: --name my-own-name\n", "      - run: echo never\n", fastTimeouts)
	var ps, rm []string
	for _, c := range f.calls(t) {
		if c[0] == "ps" && strings.Contains(strings.Join(c, " "), "--quiet") {
			ps = c
		}
		if c[0] == "rm" {
			rm = c
		}
	}
	if ps == nil || !strings.Contains(strings.Join(ps, "\n"), "label=") {
		t.Errorf("no listing by label: %v", ps)
	}
	if len(rm) < 3 || !containerIDRe.MatchString(rm[2]) {
		t.Errorf("rm by id expected: %v", rm)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "created")); strings.TrimSpace(string(b)) != "" {
		t.Errorf("container left: %q", b)
	}
	_ = rec
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

// GO-001-R: the health inspection takes the cancellation too.
func TestCancelDuringHealthInspection(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_inspect", "1")
	cancel := NewCanceller()
	go func() { time.Sleep(500 * time.Millisecond); cancel.Cancel(CancelUser) }()
	start := time.Now()
	rec, res := ctrRunC(t, f, cancel, "    services:\n      s:\n        image: redis:7\n", "      - run: echo never\n", func(c *Config) {
		c.Actions.Containers.Command = time.Minute
	})
	if res.Conclusion != ConclusionCancelled || time.Since(start) > 15*time.Second {
		t.Fatalf("%+v after %s %v", res, time.Since(start), rec.names())
	}
	var health bool
	for _, c := range f.calls(t) {
		if c[0] == "inspect" && strings.Contains(strings.Join(c, " "), "Healthcheck") {
			health = true
		}
	}
	if !health {
		t.Errorf("the health inspection was not reached: %s", names(f.calls(t)))
	}
}

// GO-002-R: a failed removal is not a removal. It stays pending and the job's
// fallback tries again; "No such container" is a removal.
func TestFailedRemovalStaysPendingAndAbsenceCounts(t *testing.T) {
	count := func(f *fakeCtr, verb string) int {
		n := 0
		for _, c := range f.calls(t) {
			if c[0] == verb || (verb == "network rm" && c[0] == "network" && len(c) > 1 && c[1] == "rm") {
				n++
			}
		}
		return n
	}
	f := newFakeCtr(t)
	f.set(t, "rm_fail", "1")
	f.set(t, "netrm_fail", "1")
	rec, _ := ctrRun(t, f, ctrJob, "      - run: echo ok\n", fastTimeouts)
	if count(f, "rm") != 2 || count(f, "network rm") != 2 {
		t.Errorf("rm=%d network rm=%d, want the fallback to retry both once", count(f, "rm"), count(f, "network rm"))
	}
	if got := logOf(rec.step(t, "Stop containers")); !strings.Contains(got, "Docker rm fail with exit code 1") || !strings.Contains(got, "Docker network rm fail with exit code 1") {
		t.Errorf("warnings:\n%s", got)
	}

	f2 := newFakeCtr(t)
	f2.set(t, "rm_absent", "1")
	f2.set(t, "netrm_absent", "1")
	rec2, res2 := ctrRun(t, f2, ctrJob, "      - run: echo ok\n", fastTimeouts)
	if res2.Conclusion != ConclusionSuccess || count(f2, "rm") != 1 || count(f2, "network rm") != 1 {
		t.Errorf("absent: %+v rm=%d network rm=%d\n%s", res2, count(f2, "rm"), count(f2, "network rm"), logOf(rec2.step(t, "Stop containers")))
	}
}

// GO-007-R: the credentials directory is registered before the login; a panic
// in the login or in the pull leaves nothing behind.
func TestPanicDuringLoginOrPullLeavesNoCredentials(t *testing.T) {
	for _, during := range []string{" login ", " pull "} {
		f := newFakeCtr(t)
		var temp string
		func() {
			defer func() { _ = recover() }()
			ctrRun(t, f, "    container:\n      image: ghcr.io/o/i:1\n      credentials:\n        username: u\n        password: pw-secret\n", "      - run: echo\n", func(c *Config) {
				temp = c.Layout.Temp
				c.Reporter = &panicOnLog{Reporter: c.Reporter, contains: during}
			})
		}()
		if m, _ := filepath.Glob(filepath.Join(temp, ".docker_*")); len(m) != 0 {
			t.Errorf("panic during%s: credentials left: %v", during, m)
		}
	}
}

type panicOnLog struct {
	Reporter
	contains string
}

func (p *panicOnLog) Log(n int, at time.Time, line string) {
	if strings.HasPrefix(line, "##[command]") && strings.Contains(line, p.contains) {
		panic("boom")
	}
	p.Reporter.Log(n, at, line)
}

// GO-004-R3: a variable name that holds "=" or NUL, or is empty, is refused
// for create and exec, before docker is called.
func TestInvalidEnvNamesAreRefused(t *testing.T) {
	if checkEnvNames([]string{"A", "b_c", "A.B", "A-B"}) != nil {
		t.Error("valid names refused")
	}
	for _, k := range []string{"", "A=B", "HTTP_PROXY=http://x/?y", "A\x00B", "="} {
		if err := checkEnvNames([]string{"OK", k}); err == nil || !strings.Contains(err.Error(), "Invalid environment variable name") {
			t.Errorf("%q: %v", k, err)
		}
	}
	f := newFakeCtr(t)
	rec, res := ctrRun(t, f, "    container:\n      image: ubuntu:24.04\n      env:\n        \"HTTP_PROXY=http://x/?y\": v\n", "      - run: echo never\n", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.step(t, "Set up job")), "##[error]Invalid environment variable name 'HTTP_PROXY=http://x/?y'") {
		t.Errorf("container env: %+v", res)
	}
	rec, res = ctrRun(t, f, ctrJob, "      - name: s\n        env:\n          \"HTTP_PROXY=http://x/?y\": v\n        run: echo never\n", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.step(t, "s")), "##[error]Invalid environment variable name 'HTTP_PROXY=http://x/?y'") {
		t.Errorf("step env: %+v\n%s", res, logOf(rec.step(t, "s")))
	}
	if n := len(f.execCalls(t)); n != 0 {
		t.Errorf("docker exec called %d times", n)
	}
}
