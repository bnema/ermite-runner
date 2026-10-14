// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ctrJob = "    container: ubuntu:24.04\n"

func TestPullRetriesThenFailsAndStillCleansUp(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "pull_fail", "9")
	rec, res := ctrRun(t, f, "    container: no-such-image:nope\n", "      - name: never\n        run: echo never\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if got := strings.Join(rec.names(), "|"); got != "Set up job|Initialize containers|never|Stop containers|Complete job" {
		t.Fatalf("steps %s", got)
	}
	if c := rec.concl("never"); c != ConclusionSkipped {
		t.Errorf("never: %s", c)
	}
	log := logOf(rec.step(t, "Initialize containers"))
	if n := strings.Count(log, "##[command]"+f.bin+" pull no-such-image:nope"); n != 3 {
		t.Errorf("%d pulls:\n%s", n, log)
	}
	for _, w := range []string{"##[warning]Docker pull failed with exit code 1, back off 0.001 seconds before retry.", "##[error]Docker pull failed with exit code 1"} {
		if !strings.Contains(log, w) {
			t.Errorf("lacks %q:\n%s", w, log)
		}
	}
	// Nothing was created: the stop step has only the network to remove.
	stop := logOf(rec.step(t, "Stop containers"))
	if strings.Contains(stop, "Stop and remove container") || !strings.Contains(stop, "Remove container network: github_network_") {
		t.Errorf("stop:\n%s", stop)
	}
}

func TestCreateFailureMessage(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "create_fail", "1")
	rec, res := ctrRun(t, f, "    container:\n      image: ubuntu:24.04\n      options: --not-a-docker-flag\n", "      - run: echo never\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	log := logOf(rec.step(t, "Initialize containers"))
	if !strings.Contains(log, "##[error]Exit code 125 returned from process: file name '"+f.bin+"', arguments 'create --name ") ||
		!strings.Contains(log, " --not-a-docker-flag -e \"HOME=/github/home\"") {
		t.Errorf("log:\n%s", log)
	}
	if got := logOf(rec.step(t, "Stop containers")); !strings.Contains(got, "Remove container network") {
		t.Errorf("stop:\n%s", got)
	}
}

func TestUnhealthyServiceFailsInit(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "health", "starting\nstarting\nunhealthy\n")
	rec, res := ctrRun(t, f, "    services:\n      broken:\n        image: redis:7\n", "      - run: echo never\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	log := logOf(rec.step(t, "Initialize containers"))
	for _, w := range []string{
		"##[group]Service container broken failed.\n##[command]" + f.bin + " logs --details ",
		"##[error]Failed to initialize container redis:7\n##[endgroup]\n##[error]One or more containers failed to start.",
	} {
		if !strings.Contains(log, w) {
			t.Errorf("lacks %q:\n%s", w, log)
		}
	}
	// A service that failed to initialise has its logs printed once, not twice.
	stop := logOf(rec.step(t, "Stop containers"))
	if strings.Contains(stop, "Print service container logs") || !strings.Contains(stop, "Stop and remove container: ") {
		t.Errorf("stop:\n%s", stop)
	}
}

func TestRegistryCredentialsAreMaskedAndRemoved(t *testing.T) {
	f := newFakeCtr(t)
	var temp string
	rec, res := ctrRun(t, f, "    container:\n      image: ghcr.io/o/img:1\n      credentials:\n        username: bob\n        password: hunter2-secret\n", "      - run: echo ok\n", func(c *Config) { temp = c.Layout.Temp })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	all := ""
	for _, s := range rec.steps {
		all += logOf(s)
	}
	if strings.Contains(all, "hunter2-secret") {
		t.Errorf("password in the log")
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "login_stdin")); string(b) != "hunter2-secret\n" {
		t.Errorf("password not passed through stdin: %q", b)
	}
	log := logOf(rec.step(t, "Initialize containers"))
	if !strings.Contains(log, " login ghcr.io -u bob --password-stdin") || !strings.Contains(log, " pull ghcr.io/o/img:1") || !strings.Contains(log, " --config ") {
		t.Errorf("log:\n%s", log)
	}
	if m, _ := filepath.Glob(filepath.Join(temp, ".docker_*")); len(m) != 0 {
		t.Errorf("config left behind: %v", m)
	}
}

func TestCancelDuringStepStillStopsContainers(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_exec", "1")
	cancel := NewCanceller()
	go func() { time.Sleep(600 * time.Millisecond); cancel.Cancel(CancelUser) }()
	rec, res := ctrRunC(t, f, cancel, ctrJob, "      - name: long\n        run: sleep 60\n", nil)
	if res.Conclusion != ConclusionCancelled {
		t.Fatalf("%+v %v", res, rec.names())
	}
	stop := rec.step(t, "Stop containers")
	if got := logOf(stop); !strings.Contains(got, "Stop and remove container: ") || !strings.Contains(got, "Remove container network: ") {
		t.Errorf("stop:\n%s", got)
	}
	if c := rec.concl("Stop containers"); c != ConclusionSuccess {
		t.Errorf("Stop containers: %s", c)
	}
	if v := f.verbs(t); v[len(v)-1] != "network rm" || v[len(v)-2] != "rm" {
		t.Errorf("last calls %v", v)
	}
}

func TestStepTimeoutStopsContainersAndIsAFailure(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_exec", "1")
	rec, res := ctrRun(t, f, ctrJob, "      - name: long\n        timeout-minutes: 1\n        run: sleep 60\n", func(c *Config) {
		c.StepTimeoutSkew = -59*time.Second - 700*time.Millisecond
	})
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := logOf(rec.step(t, "long")); !strings.Contains(got, "##[error]The action 'long' has timed out after 1 minutes.") {
		t.Errorf("long:\n%s", got)
	}
	if got := logOf(rec.step(t, "Stop containers")); !strings.Contains(got, "Remove container network: ") {
		t.Errorf("stop:\n%s", got)
	}
}

func TestJobTimeoutStopsContainers(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "hang_exec", "1")
	rec, res := ctrRun(t, f, ctrJob, "      - name: long\n        run: sleep 60\n", func(c *Config) { c.Deadline = 700 * time.Millisecond })
	if res.Conclusion != ConclusionTimedOut {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := logOf(rec.step(t, "Stop containers")); !strings.Contains(got, "Stop and remove container: ") || !strings.Contains(got, "Remove container network: ") {
		t.Errorf("stop:\n%s", got)
	}
}

func TestStopContainersRunsAfterAFailedStepAndPostStepsRunInTheContainer(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "exec_exit", "1") // the first exec fails
	rec, res := ctrRun(t, f, ctrJob, "      - name: bad\n        run: exit 1\n      - name: after\n        if: always()\n        run: echo after\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if got := strings.Join(rec.names(), "|"); got != "Set up job|Initialize containers|bad|after|Stop containers|Complete job" {
		t.Errorf("steps %s", got)
	}
	if len(f.execCalls(t)) != 2 {
		t.Errorf("both steps run through docker exec: %v", f.execCalls(t))
	}
}
