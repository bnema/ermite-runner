// SPDX-License-Identifier: MIT

package executor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
)

const wfHead = "on: push\njobs:\n  j:\n    runs-on: x\n"

func logOf(s *recStep) string { return strings.Join(s.Log, "\n") }

func TestImplicitStepsAndNumbering(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+"    steps:\n      - run: echo hi\n      - name: second\n        run: echo two\n", "j", nil)
	if res.Conclusion != ConclusionSuccess || !rec.done {
		t.Fatalf("%+v", res)
	}
	want := []string{"Set up job", "Run echo hi", "second", "Complete job"}
	if got := rec.names(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("steps %v", got)
	}
	for i, s := range rec.steps {
		if s.Num != i+1 || s.Conclusion != ConclusionSuccess {
			t.Errorf("%+v", s)
		}
	}
	setup := logOf(rec.steps[0])
	for _, w := range []string{"Current runner version: 'test'", "Prepare workflow directory", "Prepare all required actions", "Complete job name: j"} {
		if !strings.Contains(setup, w) {
			t.Errorf("Set up job lacks %q:\n%s", w, setup)
		}
	}
	if got := logOf(rec.steps[3]); got != "Cleaning up orphan processes" {
		t.Errorf("Complete job: %q", got)
	}
	if got := logOf(rec.steps[1]); !strings.HasSuffix(got, "##[endgroup]\nhi") {
		t.Errorf("log %q", got)
	}
}

func TestFailureSkipsAndStatusFunctions(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+`    steps:
      - id: boom
        run: exit 3
      - name: skipped by default success()
        run: echo NO
      - name: always
        if: always()
        run: echo ran-always
      - name: failure
        if: failure()
        run: echo ran-failure
      - name: cancelled
        if: cancelled()
        run: echo NO
      - name: outcome
        if: always() && steps.boom.outcome == 'failure' && steps.boom.conclusion == 'failure'
        run: echo ran-outcome
`, "j", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	for name, c := range map[string]Conclusion{
		"Run exit 3": ConclusionFailure, "skipped by default success()": ConclusionSkipped, "always": ConclusionSuccess,
		"failure": ConclusionSuccess, "cancelled": ConclusionSkipped, "outcome": ConclusionSuccess,
	} {
		if got := rec.step(t, name).Conclusion; got != c {
			t.Errorf("%s: %s, want %s", name, got, c)
		}
	}
	if got := logOf(rec.step(t, "Run exit 3")); !strings.HasSuffix(got, "##[error]Process completed with exit code 3.") {
		t.Errorf("%q", got)
	}
	if len(rec.step(t, "skipped by default success()").Log) != 0 {
		t.Error("a skipped step logs nothing")
	}
}

func TestEnvPrecedenceAndExpressions(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, `on: push
env:
  A: wf
  B: ${{ github.event_name }}
jobs:
  j:
    runs-on: x
    env:
      A: job
      C: ${{ github.ref_type }}-c
    steps:
      - env:
          D: ${{ env.A }}-${{ env.C }}
          A: step
        run: echo "A=$A B=$B C=$C D=$D"
      - run: echo "A=$A D=${D-unset}"
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	// The step env sees the job env (env.A is "job", not "step"), and the
	// workflow and job env are evaluated before the steps.
	if got := logOf(rec.steps[1]); !strings.HasSuffix(got, "A=step B=push C=branch-c D=job-branch-c") {
		t.Errorf("first step:\n%s", got)
	}
	if got := logOf(rec.steps[2]); !strings.HasSuffix(got, "A=job D=unset") {
		t.Errorf("second step:\n%s", got)
	}
}

func TestWorkflowCommands(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+`    steps:
      - id: s
        run: |
          echo "::group::My group"
          echo inside
          echo "::endgroup::"
          echo "::warning file=a.txt,line=1::careful"
          echo "::error::broken"
          echo "::debug::hidden"
          echo "::set-output name=old::v"
          echo "::add-mask::secret"
          echo "::not-a-command::x"
      - run: echo "old=${{ steps.s.outputs.old }}"
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	got := logOf(rec.steps[1])
	for _, w := range []string{"##[group]My group\ninside\n##[endgroup]", "##[warning]careful", "##[error]broken", "##[warning]The `set-output` command is deprecated", "::not-a-command::x"} {
		if !strings.Contains(got, w) {
			t.Errorf("log lacks %q:\n%s", w, got)
		}
	}
	_, body, _ := strings.Cut(got, "##[endgroup]\n") // after the header, which echoes the script
	if strings.Contains(body, "hidden") || strings.Contains(body, "::add-mask") || strings.Contains(body, "secret") {
		t.Errorf("debug and add-mask must not show:\n%s", body)
	}
	if !strings.HasSuffix(logOf(rec.steps[2]), "old=v") {
		t.Errorf("legacy output not visible: %s", logOf(rec.steps[2]))
	}
}

func TestCommandFailureFailsTheStep(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+"    steps:\n      - run: |\n          echo '::set-env name=X::1'\n", "j", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	got := logOf(rec.steps[1])
	if !strings.Contains(got, "##[error]Unable to process command '::set-env name=X::1' successfully.") || !strings.Contains(got, "The `set-env` command is disabled.") {
		t.Errorf("%s", got)
	}
}

func TestJobOutputs(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, `on: push
jobs:
  j:
    runs-on: x
    outputs:
      a: ${{ steps.one.outputs.v }}
      empty: ${{ steps.one.outputs.none }}
      lit: fixed
      env: ${{ env.E }}
    env:
      E: from-env
    steps:
      - id: one
        run: echo "v=1" >> "$GITHUB_OUTPUT"
      - run: exit 1
`, "j", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	// Outputs are collected even after a failure; empty ones are dropped.
	if len(res.Outputs) != 3 || res.Outputs["a"] != "1" || res.Outputs["lit"] != "fixed" || res.Outputs["env"] != "from-env" {
		t.Fatalf("%v", res.Outputs)
	}
	_ = rec
}

// Reusable workflows stay refused; container: and services: are supported.
func TestReusableWorkflowNotSupportedYet(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, "on: push\njobs:\n  j:\n    uses: ./.github/workflows/x.yml\n", "j", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.steps[0]), "not supported yet") {
		t.Errorf("%+v %v", res, rec.names())
	}
}

// Without a resolver (a host without the feature) an action fails "Set up job"
// before any step, with the runner's message.
func TestActionWithoutResolverFailsSetUp(t *testing.T) {
	rec, res := runLocal(t, wfHead+"    steps:\n      - uses: actions/checkout@v4\n      - run: echo NO\n", "j", nil)
	if res.Conclusion != ConclusionFailure || len(rec.steps) != 1 {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := logOf(rec.steps[0]); !strings.Contains(got, "##[error]Failed to resolve action download info.") {
		t.Fatalf("%s", got)
	}
}

func TestInvalidWorkflowAndUnknownJob(t *testing.T) {
	rec, res := runLocal(t, "on: push\njobs: [", "j", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(res.Error, "The workflow is not valid") || len(rec.steps) != 1 || !rec.done {
		t.Fatalf("%+v %v", res, rec.names())
	}
	_, res = runLocal(t, wfHead+"    steps: [{run: a}]\n", "nope", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(res.Error, "no job 'nope'") {
		t.Fatalf("%+v", res)
	}
}

func TestShellVariants(t *testing.T) {
	needTools(t, "bash", "sh", "python3")
	rec, res := runLocal(t, wfHead+`    steps:
      - shell: python3 {0}
        run: print("py-ok")
      - shell: bash {0}
        run: echo "no-e $-"
      - shell: sh
        run: echo sh-ok
      - shell: nonexistent-shell {0}
        run: echo NO
`, "j", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if !strings.HasSuffix(logOf(rec.steps[1]), "py-ok") || !strings.Contains(logOf(rec.steps[2]), "shell: ") {
		t.Errorf("%s | %s", logOf(rec.steps[1]), logOf(rec.steps[2]))
	}
	last := rec.steps[4]
	if last.Conclusion != ConclusionFailure || !strings.Contains(logOf(last), "##[error]nonexistent-shell: command not found") {
		t.Errorf("%+v", last)
	}
}

func TestGithubPathAndEnvReachNextSteps(t *testing.T) {
	needTools(t, "bash")
	rec, _ := runLocal(t, wfHead+`    steps:
      - run: |
          mkdir -p "$RUNNER_TEMP/bin1" "$RUNNER_TEMP/bin2"
          printf '#!/bin/sh\necho tool-1\n' > "$RUNNER_TEMP/bin1/tool-ermite"; chmod +x "$RUNNER_TEMP/bin1/tool-ermite"
          printf '#!/bin/sh\necho tool-2\n' > "$RUNNER_TEMP/bin2/tool-ermite"; chmod +x "$RUNNER_TEMP/bin2/tool-ermite"
          echo "$RUNNER_TEMP/bin1" >> "$GITHUB_PATH"
          echo "$RUNNER_TEMP/bin2" >> "$GITHUB_PATH"
          echo "MULTI<<X" >> "$GITHUB_ENV"; echo "a" >> "$GITHUB_ENV"; echo "b" >> "$GITHUB_ENV"; echo "X" >> "$GITHUB_ENV"
      - run: |
          tool-ermite
          printf 'MULTI=<%s>\n' "$MULTI"
          echo "ci=$CI actions=$GITHUB_ACTIONS ws=${GITHUB_WORKSPACE##*/} cwd=${PWD##*/}"
`, "j", nil)
	got := logOf(rec.steps[2])
	if !strings.HasSuffix(got, "tool-2\nMULTI=<a\nb>\nci=true actions=true ws=hello cwd=hello") {
		t.Errorf("%s", got)
	}
}

// ---- cancellation and timeouts

func TestCancelSendsSIGINTThenStopsTheJob(t *testing.T) {
	needTools(t, "bash")
	c := NewCanceller()
	started := make(chan struct{})
	// The script prints "ready" after it installed its trap: the cancel can come
	// at once.
	go func() {
		<-started
		c.Cancel(CancelUser)
	}()
	rec := newRecorder()
	rep := &signalReporter{recorder: rec, started: started, match: "ready"}
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	res := Run(c, Config{
		WorkflowYAML: []byte(wfHead + `    steps:
      - run: |
          trap 'echo got-int; exit 130' INT
          echo ready
          while :; do sleep 0.05; done
      - name: after
        run: echo NO
      - name: cleanup
        if: always()
        run: echo cleanup-ran
`), JobKey: "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rep,
		BaseEnv:   []string{"PATH=" + envPath(), "HOME=" + home},
		TermAfter: 300 * time.Millisecond, KillAfter: 300 * time.Millisecond,
	})
	if res.Conclusion != ConclusionCancelled || res.RunnerShutdown {
		t.Fatalf("%+v", res)
	}
	run := rec.steps[1]
	if run.Conclusion != ConclusionCancelled || !strings.Contains(logOf(run), "got-int") || !strings.Contains(logOf(run), "##[error]The operation was canceled.") {
		t.Fatalf("%s %s", run.Conclusion, logOf(run))
	}
	if rec.step(t, "after").Conclusion != ConclusionSkipped {
		t.Error("after must be skipped")
	}
	if got := rec.step(t, "cleanup"); got.Conclusion != ConclusionSuccess || !strings.HasSuffix(logOf(got), "cleanup-ran") {
		t.Errorf("always() step must run after a cancel: %+v", got)
	}
}

// signalReporter closes started when a log line equals match.
type signalReporter struct {
	*recorder
	started chan struct{}
	match   string
	once    bool
}

func (r *signalReporter) Log(n int, at time.Time, line string) {
	r.recorder.Log(n, at, line)
	if !r.once && line == r.match {
		r.once = true
		close(r.started)
	}
}

func TestCancelEscalatesToTermAndKill(t *testing.T) {
	needTools(t, "bash")
	c := NewCanceller()
	var mu = make(chan struct{}, 8)
	var got []syscall.Signal
	started := make(chan struct{})
	rec := newRecorder()
	rep := &signalReporter{recorder: rec, started: started, match: "ready"}
	go func() { <-started; c.Cancel(CancelUser) }()
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	t0 := time.Now()
	var times []time.Duration
	res := Run(c, Config{
		WorkflowYAML: []byte(wfHead + "    steps:\n      - run: |\n          trap '' INT TERM\n          echo ready\n          while :; do sleep 0.05; done\n"),
		JobKey:       "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rep,
		BaseEnv:   []string{"PATH=" + envPath(), "HOME=" + home},
		TermAfter: 250 * time.Millisecond, KillAfter: 250 * time.Millisecond,
		Signal: func(pid int, sig syscall.Signal) {
			got = append(got, sig)
			times = append(times, time.Since(t0))
			_ = syscall.Kill(pid, sig)
			mu <- struct{}{}
		},
	})
	if res.Conclusion != ConclusionCancelled {
		t.Fatalf("%+v", res)
	}
	if len(got) != 3 || got[0] != syscall.SIGINT || got[1] != syscall.SIGTERM || got[2] != syscall.SIGKILL {
		t.Fatalf("signals %v", got)
	}
	if d := times[2] - times[0]; d < 450*time.Millisecond || d > 2*time.Second {
		t.Errorf("kill came %v after the interrupt, want about 500ms", d)
	}
}

func TestShutdownCancelSkipsRemainingSteps(t *testing.T) {
	needTools(t, "bash")
	c := NewCanceller()
	started := make(chan struct{})
	rec := newRecorder()
	rep := &signalReporter{recorder: rec, started: started, match: "ready"}
	go func() { <-started; c.Cancel(CancelShutdown) }()
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	res := Run(c, Config{
		WorkflowYAML: []byte(wfHead + "    steps:\n      - run: |\n          echo ready\n          sleep 30\n      - if: always()\n        run: echo NO\n"),
		JobKey:       "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rep,
		BaseEnv: []string{"PATH=" + envPath(), "HOME=" + home}, TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond,
	})
	if res.Conclusion != ConclusionCancelled || !res.RunnerShutdown {
		t.Fatalf("%+v", res)
	}
	// On runner shutdown conditions are not evaluated (StepsRunner.cs:205-208):
	// even always() steps are skipped.
	if got := rec.steps[2]; got.Conclusion != ConclusionSkipped {
		t.Errorf("%+v", got)
	}
}

func TestJobTimeout(t *testing.T) {
	needTools(t, "bash")
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	rec := newRecorder()
	cfg := Config{
		WorkflowYAML: []byte(wfHead + "    steps:\n      - run: sleep 30\n"),
		JobKey:       "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rec,
		BaseEnv: []string{"PATH=" + envPath(), "HOME=" + home}, TermAfter: 100 * time.Millisecond, KillAfter: 100 * time.Millisecond,
	}
	c := NewCanceller()
	// A one-minute job timeout cannot be waited for: fire it as the timer does.
	time.AfterFunc(300*time.Millisecond, func() { c.Cancel(CancelTimeout) })
	res := Run(c, cfg)
	if res.Conclusion != ConclusionTimedOut || !strings.Contains(res.Error, "maximum execution time of 360 minutes") {
		t.Fatalf("%+v", res)
	}
	if rec.steps[1].Conclusion != ConclusionCancelled {
		t.Errorf("%+v", rec.steps[1])
	}
}

func TestStartErrorsAndOrphans(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+`    steps:
      - run: (sleep 60 &) ; echo started-background
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	last := logOf(rec.steps[len(rec.steps)-1])
	if !strings.Contains(last, "Cleaning up orphan processes") || !strings.Contains(last, "Terminate orphan process: pid (") || !strings.Contains(last, ") (sleep)") {
		t.Errorf("orphan cleanup: %q", last)
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(l string) { got = append(got, l) }}
	for _, chunk := range []string{"a\nb\r", "\nc\rd", "\ne\xff", "f\n", "tail"} {
		_, _ = w.Write([]byte(chunk))
	}
	w.Flush()
	want := []string{"a", "b", "c", "d", "e\ufffdf", "tail"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}

func TestParseCommands(t *testing.T) {
	reg := func(s string) bool { return s == "set-output" || s == "group" || s == "warning" }
	c, ok := parseCommandV2("::set-output name=a%2Cb,x=y::v%0Aw%25", reg)
	if !ok || c.Name != "set-output" || c.prop("name") != "a,b" || c.Data != "v\nw%" {
		t.Fatalf("%+v %v", c, ok)
	}
	if c, ok = parseCommandV2("  ::group::title", reg); !ok || c.Data != "title" {
		t.Fatalf("leading space: %+v", c)
	}
	if _, ok = parseCommandV2("::unknown::x", reg); ok {
		t.Fatal("unregistered command")
	}
	if _, ok = parseCommandV2("x ::group::t", reg); ok {
		t.Fatal("V2 must start the line")
	}
	c, ok = parseCommandV1("text ##[set-output name=n;k=v%3Bz]data%5D", reg)
	if !ok || c.prop("name") != "n" || c.prop("k") != "v;z" || c.Data != "data]" {
		t.Fatalf("v1: %+v %v", c, ok)
	}
}

func TestStopCommands(t *testing.T) {
	needTools(t, "bash")
	rec, _ := runLocal(t, wfHead+`    steps:
      - run: |
          echo "::stop-commands::tok12345"
          echo "::warning::not-processed"
          echo "::tok12345::"
          echo "::warning::processed"
`, "j", nil)
	got := logOf(rec.steps[1])
	if !strings.Contains(got, "::warning::not-processed") || !strings.Contains(got, "##[warning]processed") || !strings.Contains(got, "::stop-commands::tok12345") {
		t.Errorf("%s", got)
	}
}

func TestJobOutputsOverOneMiBFailTheJob(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, `on: push
jobs:
  j:
    runs-on: x
    outputs:
      big: ${{ steps.s.outputs.big }}
      other: ${{ steps.s.outputs.big }}
    steps:
      - id: s
        run: echo "big=$(head -c 600000 /dev/zero | tr '\0' x)" >> "$GITHUB_OUTPUT"
`, "j", nil)
	if res.Conclusion != ConclusionFailure || len(res.Outputs) != 0 {
		t.Fatalf("%+v", res.Conclusion)
	}
	last := rec.steps[len(rec.steps)-1]
	if last.Name != CompleteJobName || last.Conclusion != ConclusionFailure || !strings.Contains(logOf(last), "##[error]The job outputs are larger than 1 MiB in total.") {
		t.Fatalf("%s %s", last.Conclusion, logOf(last))
	}
}

// A step that plants a link where the executor writes event.json (before the
// directory exists, or between two steps) must not make it write elsewhere.
func TestEventJSONIsNeverWrittenThroughALink(t *testing.T) {
	needTools(t, "bash")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, res := runLocal(t, wfHead+`    steps:
      - run: |
          rm -f "$RUNNER_TEMP/_github_workflow/event.json"
          ln -s "`+victim+`" "$RUNNER_TEMP/_github_workflow/event.json"
      - run: cat "$GITHUB_EVENT_PATH"
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %s", res, logOf(rec.steps[2]))
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the target of the link was overwritten: %q", b)
	}
	if got := logOf(rec.steps[2]); !strings.Contains(got, `"ref": "refs/heads/scn/x/r1"`) {
		t.Fatalf("the second step does not see a fresh event.json:\n%s", got)
	}
}

func TestLineWriterBoundsALineWithoutEnd(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(l string) { got = append(got, l) }}
	// 2.5 MiB of two-byte characters without a line feed, in odd-sized writes
	// that cut characters in two.
	blob := []byte(strings.Repeat("é", (5<<20)/4))
	for i := 0; i < len(blob); i += 7777 {
		_, _ = w.Write(blob[i:min(i+7777, len(blob))])
	}
	w.Flush()
	total := 0
	for _, l := range got {
		if len(l) > maxLineBytes {
			t.Fatalf("a line of %d bytes", len(l))
		}
		if strings.ContainsRune(l, '\ufffd') {
			t.Fatal("a character was cut in two")
		}
		total += len(l)
	}
	if len(got) < 3 || total != len(blob) {
		t.Fatalf("%d lines, %d bytes of %d", len(got), total, len(blob))
	}
}

func TestEnvFlagReadsTheStepEnvironmentNotTheExecutors(t *testing.T) {
	// ACTIONS_ALLOW_UNSECURE_COMMANDS is read from the environment the steps
	// get (BaseEnv), not from the process of the executor.
	t.Setenv("ACTIONS_ALLOW_UNSECURE_COMMANDS", "true")
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+"    steps:\n      - run: echo '::set-env name=X::1'\n", "j", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.steps[1]), "The `set-env` command is disabled.") {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	rec, res = runLocal(t, wfHead+"    steps:\n      - run: echo '::set-env name=X::1'\n      - run: echo \"X=$X\"\n", "j", func(c *Config) {
		c.BaseEnv = append(c.BaseEnv, "ACTIONS_ALLOW_UNSECURE_COMMANDS=true")
	})
	if res.Conclusion != ConclusionSuccess || !strings.HasSuffix(logOf(rec.steps[2]), "X=1") {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
}

// SIGINT goes to the process the step started, like the official runner
// (ProcessInvoker.SendSignal: kill(_proc.Id, SIGINT)), not to its group: bash
// is interrupted but the foreground sleep it waits for is not signalled by it.
func TestCancelSignalsTheShellNotTheGroup(t *testing.T) {
	needTools(t, "bash")
	c := NewCanceller()
	started := make(chan struct{})
	rec := newRecorder()
	rep := &signalReporter{recorder: rec, started: started, match: "ready"}
	go func() { <-started; c.Cancel(CancelUser) }()
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	marker := filepath.Join(home, "child-survived")
	var sigs []syscall.Signal
	res := Run(c, Config{
		WorkflowYAML: []byte(wfHead + "    steps:\n      - run: |\n          trap 'echo got-int' INT\n          echo ready\n          sleep 1 && touch " + marker + "\n"),
		JobKey:       "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rep,
		BaseEnv:   []string{"PATH=" + envPath(), "HOME=" + home},
		TermAfter: 5 * time.Second, KillAfter: 5 * time.Second,
		Signal: func(pid int, sig syscall.Signal) {
			sigs = append(sigs, sig)
			if err := syscall.Kill(pid, sig); err != nil {
				t.Error(err)
			}
		},
	})
	if res.Conclusion != ConclusionCancelled || len(sigs) != 1 || sigs[0] != syscall.SIGINT {
		t.Fatalf("%+v signals %v", res, sigs)
	}
	// bash runs its trap after the foreground child ends: the sleep was not killed.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the foreground child was signalled with the shell: %v", err)
	}
}

// Deadline is what is left of the timeout when the host sent the job: the
// executor must not grant the whole timeout again.
func TestDeadlineOverridesTheTimeout(t *testing.T) {
	needTools(t, "bash")
	start := time.Now()
	rec, res := runLocal(t, wfHead+"    steps:\n      - run: sleep 30\n", "j", func(c *Config) {
		c.TimeoutMinutes = 60
		c.Deadline = 500 * time.Millisecond
	})
	if res.Conclusion != ConclusionTimedOut || !strings.Contains(res.Error, "60 minutes") {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > 10*time.Second || rec.steps[1].Conclusion != ConclusionCancelled {
		t.Fatalf("took %v: %+v", time.Since(start), rec.steps[1])
	}
}

// The env of a step reads its own id in github.action (StepsRunner.cs:118 sets
// it before the env is evaluated), and the file command paths of the step
// before it, as the recordings of github.com show for the dump step.
func TestGithubActionInStepEnvIsTheStepsOwnID(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+`    steps:
      - run: echo one
      - id: named
        run: echo "$A|$GITHUB_ACTION|$GITHUB_ARTIFACTS" | sed 's#/[^|]*/##'
        env:
          A: ${{ github.action }}
      - run: echo "$A"
        env:
          A: ${{ github.action }}
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := rec.steps[2].Log[len(rec.steps[2].Log)-1]; !strings.HasPrefix(got, "named|named|artifacts_") {
		t.Errorf("named step: %q", got)
	}
	if got := rec.steps[3].Log[len(rec.steps[3].Log)-1]; got != "__run_2" {
		t.Errorf("third step: %q", got)
	}
}

// Without a matrix the matrix context is null, as on github.com (toJSON prints
// null), and matrix.x is still readable (null, so an empty string).
func TestMatrixContextWithoutMatrixIsNull(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+`    steps:
      - run: echo "json=$J x=[$X] eq=$E"
        env:
          J: ${{ toJSON(matrix) }}
          X: ${{ matrix.x }}
          E: ${{ matrix == null }}
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, logOf(rec.steps[1]))
	}
	if got := rec.steps[1].Log[len(rec.steps[1].Log)-1]; got != "json=null x=[] eq=true" {
		t.Errorf("%q", got)
	}
}

func setupLog(t *testing.T, yaml, job string, tweak func(*Config)) string {
	t.Helper()
	rec, res := runLocal(t, yaml, job, tweak)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	return logOf(rec.steps[0])
}

func TestSetUpJobMatchesGitHubHosted(t *testing.T) {
	needTools(t, "bash")
	want := strings.Join([]string{"Current runner version: 'test'", "##[group]GITHUB_TOKEN Permissions", "Contents: read", "Metadata: read",
		"Packages: read", "##[endgroup]", "Secret source: Actions", "Cache mode: write", "Prepare workflow directory",
		"Prepare all required actions", "Complete job name: j"}, "\n")
	if got := setupLog(t, wfHead+"    steps:\n      - run: echo hi\n", "j", nil); got != want {
		t.Errorf("github-hosted:\n%s", got)
	}
}

func TestSetUpJobSelfHostedNamesTheRunner(t *testing.T) {
	needTools(t, "bash")
	got := setupLog(t, wfHead+"    steps:\n      - run: echo hi\n", "j", func(c *Config) { c.Runner.Environment = "self-hosted" })
	host, _ := os.Hostname()
	if !strings.Contains(got, "Runner name: 'test-runner'\nMachine name: '"+host+"'\n##[group]GITHUB_TOKEN") {
		t.Errorf("self-hosted:\n%s", got)
	}
}

// The expected groups of the whole-token cases, as recorded on github.com
// (recordings/permissions-display.json: p-read-all, p-write-all).
const (
	recordedReadAll = `Actions: read
ArtifactMetadata: read
Attestations: read
Checks: read
CodeQuality: read
Contents: read
Deployments: read
Discussions: read
Drives: read
Issues: read
Metadata: read
Models: read
Packages: read
Pages: read
PullRequests: read
RepositoryProjects: read
SecurityEvents: read
Statuses: read
VulnerabilityAlerts: read`
	recordedWriteAll = `Actions: write
ArtifactMetadata: write
Attestations: write
Checks: write
CodeQuality: write
Contents: write
CopilotRequests: write
Deployments: write
Discussions: write
Drives: write
Issues: write
Metadata: read
Models: read
Packages: write
Pages: write
PullRequests: write
RepositoryProjects: write
SecurityEvents: write
Statuses: write
VulnerabilityAlerts: read`
)

// The GITHUB_TOKEN Permissions group follows the effective permissions of the
// job (the job's replace the workflow's), as recorded on github.com
// (recordings/permissions-display.json).
func TestSetUpJobPrintsTheTokenPermissionsGitHubGrants(t *testing.T) {
	needTools(t, "bash")
	steps := "    steps:\n      - run: echo hi\n"
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"no key", wfHead + steps, "Contents: read\nMetadata: read\nPackages: read"},
		{"workflow {}", "permissions: {}\n" + wfHead + steps, "Metadata: read"},
		{"job {}", "permissions: write-all\n" + wfHead + "    permissions: {}\n" + steps, "Metadata: read"},
		{"workflow map", "permissions:\n  packages: read\n  contents: write\n" + wfHead + steps,
			"Contents: write\nMetadata: read\nPackages: read"},
		{"none and id-token are not listed", wfHead + "    permissions:\n      issues: none\n      id-token: write\n      checks: read\n" + steps,
			"Checks: read\nMetadata: read"},
		{"job replaces workflow", "permissions:\n  contents: write\n  issues: write\n" + wfHead + "    permissions:\n      pull-requests: read\n" + steps,
			"Metadata: read\nPullRequests: read"},
		{"job read-all over workflow map", "permissions:\n  contents: write\n" + wfHead + "    permissions: read-all\n" + steps, recordedReadAll},
		{"workflow write-all, job narrows", "permissions: write-all\n" + wfHead + "    permissions:\n      contents: read\n" + steps,
			"Contents: read\nMetadata: read"},
		{"read-all", "permissions: read-all\n" + wfHead + steps, recordedReadAll},
		{"write-all", "permissions: write-all\n" + wfHead + steps, recordedWriteAll},
		{"metadata is not asked for", wfHead + "    permissions:\n      copilot-requests: write\n      security-events: read\n      pull-requests: write\n" + steps,
			"CopilotRequests: write\nMetadata: read\nPullRequests: write\nSecurityEvents: read"},
	} {
		want := strings.Join([]string{"Current runner version: 'test'", "##[group]GITHUB_TOKEN Permissions", tc.want,
			"##[endgroup]", "Secret source: Actions", "Cache mode: write", "Prepare workflow directory",
			"Prepare all required actions", "Complete job name: j"}, "\n")
		if got := setupLog(t, tc.yaml, "j", nil); got != want {
			t.Errorf("%s:\ngot\n%s\nwant\n%s", tc.name, got, want)
		}
	}
}

func TestSetUpJobNamesTheJobLikeGitHub(t *testing.T) {
	needTools(t, "bash")
	// recordings/matrix-job-name.json
	yaml := `on: push
jobs:
  default-name:
    runs-on: x
    strategy:
      matrix:
        os: [a, b]
        n: [1, 2]
    steps:
      - run: echo hi
  custom-name:
    name: "custom ${{ matrix.os }} / ${{ matrix.n }} / ${{ github.event_name }}"
    runs-on: x
    strategy:
      matrix:
        os: [a, b]
        n: [1]
    steps:
      - run: echo hi
  static-name:
    name: same name for every leg
    runs-on: x
    strategy:
      matrix:
        n: [1, 2]
    steps:
      - run: echo hi
  plain:
    name: "plain ${{ github.event_name }}"
    runs-on: x
    steps:
      - run: echo hi
`
	for _, tc := range []struct{ job, matrix, strategy, want string }{
		{"default-name", `{"os":"b","n":1}`, `{"job-index":2}`, "default-name (b, 1)"},
		{"default-name", `{"os":"a","n":2}`, `{"job-index":1}`, "default-name (a, 2)"},
		{"default-name", `{"os":"b","n":2}`, ``, "default-name (b, 2)"},
		{"custom-name", `{"os":"b","n":1}`, `{"job-index":1}`, "custom b / 1 / push"},
		{"static-name", `{"n":2}`, `{"job-index":1}`, "same name for every leg (2)"},
		{"plain", `{}`, ``, "plain push"},
		{"static-name", `{"n":9}`, ``, "static-name"}, // not one of the combinations: the job key
		// job-index points to another combination: it is not trusted.
		{"default-name", `{"os":"a","n":2}`, `{"job-index":0}`, "default-name (a, 2)"},
		{"default-name", `{"os":"b","n":2}`, `{"job-index":7}`, "default-name (b, 2)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got := setupLog(t, yaml, tc.job, func(c *Config) {
				c.MatrixJSON = tc.matrix
				ctx := testGitHubContext(tc.job)
				ctx = strings.TrimSuffix(ctx, "}")
				ctx += `,"matrix":` + tc.matrix
				if tc.strategy != "" {
					ctx += `,"strategy":` + tc.strategy
				}
				c.ContextsJSON = ctx + "}"
			})
			if !strings.HasSuffix(got, "Complete job name: "+tc.want) {
				t.Errorf("got:\n%s", got)
			}
		})
	}
}

func TestJobDisplayNameReadsNeedsVarsAndIgnoresRunnerKeys(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  j:
    name: "${{ needs.a.outputs.o }}-${{ vars.V }}-${{ github.workspace }}-${{ github.secret_source }}-${{ github.token }}-${{ github.event_name }}"
    runs-on: x
    steps:
      - run: echo hi
`
	got := setupLog(t, yaml, "j", func(c *Config) {
		c.ContextsJSON = `{"github":{"repository":"octo/hello","event_name":"push","job":"j"},"needs":{"a":{"outputs":{"o":"out"}}},"vars":{"V":"v1"}}`
	})
	// workspace, secret_source and token are added by the runner: empty for the name.
	if !strings.HasSuffix(got, "Complete job name: out-v1----push") {
		t.Errorf("got:\n%s", got)
	}
}

func TestJobDisplayNameIncludeOnlyMatrix(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  j:
    runs-on: x
    strategy:
      matrix:
        include:
          - os: a
            n: 1
          - os: b
            n: 2
    steps:
      - run: echo hi
`
	for matrix, want := range map[string]string{`{"os":"b","n":2}`: "j (b, 2)", `{"os":"a","n":1}`: "j (a, 1)"} {
		got := setupLog(t, yaml, "j", func(c *Config) {
			c.MatrixJSON = matrix
			c.ContextsJSON = strings.TrimSuffix(testGitHubContext("j"), "}") + `,"matrix":` + matrix + `}`
		})
		if !strings.HasSuffix(got, "Complete job name: "+want) {
			t.Errorf("%s:\n%s", matrix, got)
		}
	}
}

func TestJobDisplayNameFallsBackToTheKeyWithAWarning(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  j:
    runs-on: x
    strategy:
      matrix:
        n: [1, 2]
    steps:
      - run: echo hi
`
	var logs bytes.Buffer
	got := setupLog(t, yaml, "j", func(c *Config) {
		c.MatrixJSON = `{"n":5}`
		c.Log = zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs})
	})
	if !strings.HasSuffix(got, "Complete job name: j") {
		t.Errorf("got:\n%s", got)
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) || !strings.Contains(logs.String(), "using the job key") {
		t.Errorf("no warning: %q", logs.String())
	}
}
