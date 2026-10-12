// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestSplitLine(t *testing.T) {
	if got := SplitLine("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := SplitLine("short", 10); len(got) != 1 || got[0] != "short" {
		t.Fatalf("%q", got)
	}
	// Cuts at character boundaries and joins back exactly.
	in := strings.Repeat("é€😀a", 40) // 2, 3, 4, 1 bytes
	for _, max := range []int{4, 5, 7, 10, 33} {
		parts := SplitLine(in, max)
		var b strings.Builder
		for _, p := range parts {
			if len(p) > max || !utf8.ValidString(p) || p == "" {
				t.Fatalf("max %d: bad piece %q", max, p)
			}
			b.WriteString(p)
		}
		if b.String() != in {
			t.Fatalf("max %d: pieces do not rebuild the line", max)
		}
	}
	// Invalid UTF-8 is replaced before cutting.
	if got := SplitLine("a\xffb", 10); got[0] != "a\ufffdb" {
		t.Fatalf("%q", got)
	}
	big := SplitLine(strings.Repeat("x", 3*MaxLogLine+5), MaxLogLine)
	if len(big) != 4 || len(big[0]) != MaxLogLine || len(big[3]) != 5 {
		t.Fatalf("%d pieces", len(big))
	}
}

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
			if len(l.Text) > MaxLogLine || (pieces > 1) != l.Continued {
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
