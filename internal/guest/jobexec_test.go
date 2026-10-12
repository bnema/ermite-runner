// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"net"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// runJobSession starts Exec on a unix socket and drives it with a job.
func runJobSession(t *testing.T, yaml string, cancelAfter time.Duration, tweak func(*ExecConfig)) (steps []string, logs []*execpb.Log, res *execpb.JobResult, exit *execpb.Exit) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_TOOL_CACHE", filepath.Join(home, "toolcache"))
	t.Setenv("ERMITE_CONNECT", "unix:"+sock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cfg := ExecConfig{Target: Target{"unix", sock}, Sandbox: "container", Version: "test", RunTimeout: 5 * time.Second,
			TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond, WaitDelay: 300 * time.Millisecond}
		if tweak != nil {
			tweak(&cfg)
		}
		_, _ = Exec(context.Background(), cfg)
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var cancel chan *execpb.Cancel
	if cancelAfter > 0 {
		cancel = make(chan *execpb.Cancel, 1)
		time.AfterFunc(cancelAfter, func() { cancel <- &execpb.Cancel{Reason: "test", Kind: execpb.CancelKind_CANCEL_KIND_USER} })
	}
	var mu sync.Mutex
	_, exit, err = execstream.DriveJob(conn, &execpb.Run{Job: &execpb.JobSpec{
		WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: yaml, JobKey: "j", Repo: "hello", RunnerName: "r", RunnerVersion: "v",
		Contexts: `{"github":{"repository":"octo/hello","event_name":"push","job":"j"}}`, Matrix: "{}",
	}}, cancel, execstream.JobHandler{
		StepStarted: func(s *execpb.StepStarted) {
			mu.Lock()
			steps = append(steps, s.Name)
			mu.Unlock()
		},
		Log:       func(l *execpb.Log) { logs = append(logs, l) },
		JobResult: func(r *execpb.JobResult) { res = r },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	return
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := lookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
}

func TestExecRunsAJob(t *testing.T) {
	requireBash(t)
	steps, logs, res, exit := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    outputs:\n      o: ${{ steps.s.outputs.v }}\n    steps:\n      - id: s\n        run: |\n          echo hello-from-job\n          echo v=42 >> \"$GITHUB_OUTPUT\"\n", 0, nil)
	if res == nil || res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || res.Outputs["o"] != "42" || exit.Code != 0 {
		t.Fatalf("res=%v exit=%v", res, exit)
	}
	if strings.Join(steps, "|") != "Set up job|Run echo hello-from-job|Complete job" {
		t.Fatalf("steps %v", steps)
	}
	found := false
	for _, l := range logs {
		if l.Text == "hello-from-job" && l.Step == 2 && l.UnixNano > 0 && !l.Continued {
			found = true
		}
	}
	if !found {
		t.Fatalf("log line missing: %d lines", len(logs))
	}
}

func TestExecJobFailureExitsNonZero(t *testing.T) {
	requireBash(t)
	_, _, res, exit := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: exit 4\n", 0, nil)
	if res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_FAILURE || exit.Code == 0 {
		t.Fatalf("res=%v exit=%v", res, exit)
	}
}

func TestExecJobLongLinesAreContinued(t *testing.T) {
	requireBash(t)
	_, logs, res, _ := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: head -c 150000 /dev/zero | tr '\\0' y; echo\n", 0, nil)
	if res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS {
		t.Fatalf("%v", res)
	}
	var total int
	var pieces int
	for _, l := range logs {
		if strings.HasPrefix(l.Text, "yyyy") {
			pieces++
			total += len(l.Text)
			if len(l.Text) > execstream.MaxLogLine || (pieces > 1) != l.Continued {
				t.Fatalf("piece %d: len %d continued %v", pieces, len(l.Text), l.Continued)
			}
		}
	}
	if pieces != 3 || total != 150000 {
		t.Fatalf("pieces=%d total=%d", pieces, total)
	}
}

func TestExecJobCancel(t *testing.T) {
	requireBash(t)
	start := time.Now()
	_, _, res, _ := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: |\n          echo ready\n          sleep 30\n", 700*time.Millisecond, nil)
	if res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_CANCELLED || res.RunnerShutdown {
		t.Fatalf("%v", res)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("cancel too slow")
	}
}

func TestExecJobHostGoneIsAShutdown(t *testing.T) {
	requireBash(t)
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TOOL_CACHE", filepath.Join(t.TempDir(), "toolcache"))
	done := make(chan int, 1)
	go func() {
		c, _ := Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, RunTimeout: 5 * time.Second,
			TermAfter: 100 * time.Millisecond, KillAfter: 100 * time.Millisecond, WaitDelay: 200 * time.Millisecond})
		done <- c
	}()
	conn, _ := ln.Accept()
	c := execstream.NewConn(conn)
	if _, err := c.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = c.Send(&execpb.Frame{Body: &execpb.Frame_Run{Run: &execpb.Run{Job: &execpb.JobSpec{
		WorkflowYaml: "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: sleep 60\n", JobKey: "j", Repo: "hello",
		Contexts: `{"github":{"repository":"o/hello"}}`,
	}}}})
	time.Sleep(700 * time.Millisecond)
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the executor kept running without a host")
	}
}

func TestStepEnv(t *testing.T) {
	got := stepEnv([]string{"PATH=/bin", "HOME=/root", "ERMITE_CONNECT=x", "USER=root", "RUNNER_TOOL_CACHE=/x", "FOO=1"}, StepUser{Name: "runner", Home: "/home/runner"})
	want := "PATH=/bin FOO=1 HOME=/home/runner USER=runner LOGNAME=runner"
	if strings.Join(got, " ") != want {
		t.Fatalf("%v", got)
	}
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }

func TestMissingStepUserFailsTheJobUnlessTestOptionSet(t *testing.T) {
	none := func(string) (*user.User, error) { return nil, user.UnknownUserError("runner") }
	// As root with no such user: refuse.
	if _, err := lookupStepUser(0, none, "runner", false); err == nil || !strings.Contains(err.Error(), `no user "runner"`) || !strings.Contains(err.Error(), "refusing to run them as root") {
		t.Fatalf("%v", err)
	}
	// The test option allows root.
	if su, err := lookupStepUser(0, none, "runner", true); err != nil || su.Name != "root" || su.Credential != nil {
		t.Fatalf("%+v %v", su, err)
	}
	// Not root: the steps run as the current user, whatever the name.
	if su, err := lookupStepUser(1000, none, "runner", false); err != nil || su.Credential != nil {
		t.Fatalf("%+v %v", su, err)
	}
	// As root with the user: switch to it.
	found := func(string) (*user.User, error) {
		return &user.User{Uid: "1001", Gid: "1001", Username: "runner", HomeDir: "/home/runner"}, nil
	}
	su, err := lookupStepUser(0, found, "runner", false)
	if err != nil || su.Credential == nil || su.Credential.Uid != 1001 || su.Credential.Gid != 1001 || su.Home != "/home/runner" {
		t.Fatalf("%+v %v", su, err)
	}
}

// The failure is reported like a failed "Set up job".
func TestFailSetupReportsAFailedFirstStep(t *testing.T) {
	host, guest := net.Pipe()
	got := make(chan []string, 1)
	go func() {
		var frames []string
		c := execstream.NewConn(host)
		for {
			f, err := c.Recv()
			if err != nil {
				got <- frames
				return
			}
			switch b := f.Body.(type) {
			case *execpb.Frame_StepStarted:
				frames = append(frames, "start:"+b.StepStarted.Name)
			case *execpb.Frame_Log:
				frames = append(frames, "log:"+b.Log.Text)
			case *execpb.Frame_StepCompleted:
				frames = append(frames, "done:"+b.StepCompleted.Conclusion.String())
			case *execpb.Frame_JobResult:
				frames = append(frames, "result:"+b.JobResult.Conclusion.String())
			}
		}
	}()
	if code := failSetup(execstream.NewConn(guest), "no user"); code != 1 {
		t.Fatal(code)
	}
	_ = guest.Close()
	want := "start:Set up job|log:##[error]no user|done:STEP_CONCLUSION_FAILURE|result:STEP_CONCLUSION_FAILURE"
	if g := strings.Join(<-got, "|"); g != want {
		t.Fatalf("%s", g)
	}
}
