// SPDX-License-Identifier: MIT

package executor

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-actions/expr"
)

// Texts recorded on github.com (ermite-conformance, scenarios match-basic).
func TestMatcherFileErrorsAreTheRunners(t *testing.T) {
	_, err := parseMatchersConfig([]byte("not json\n"))
	const want = "Unexpected character encountered while parsing value: n. Path '', line 0, position 0."
	if err == nil || err.Error() != want {
		t.Errorf("not JSON: %v", err)
	}
	cfg, err := parseMatchersConfig([]byte(`{"problemMatcher":[{"owner":"badregexp","pattern":[{"regexp":"^INVALID (.+$","message":1}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.validate()
	const wantRe = "Invalid pattern '^INVALID (.+$' at offset 13. Not enough )'s."
	if err == nil || err.Error() != wantRe {
		t.Errorf("regexp: %v", err)
	}
	// Json.NET accepts what RFC 8259 does not.
	if _, err := parseMatchersConfig([]byte(`{'problemMatcher':[],}`)); err != nil {
		t.Errorf("lenient JSON: %v", err)
	}
}

// cmd-add-path-disabled: ArgUtil.NotNullOrEmpty throws ArgumentNullException,
// whose message is "Value cannot be null. (Parameter 'path')".
func TestAddPathWithoutPathMessage(t *testing.T) {
	needTools(t, "bash")
	rec, _ := runLocal(t, wfHead+"    steps:\n      - name: p\n        env:\n          ACTIONS_ALLOW_UNSECURE_COMMANDS: 'true'\n        run: echo '::add-path::'\n", "j", nil)
	if got := logOf(rec.step(t, "p")); !strings.Contains(got, "##[error]Value cannot be null. (Parameter 'path')") || strings.Contains(got, "null or empty") {
		t.Error(got)
	}
}

// cmd-stop-commands: the exception of ValidateStopToken is not inside the try
// block of TryProcessCommand, so the step fails with the message alone (no
// "Unable to process command") and the output after it is not read.
func TestInvalidStopTokenFailsTheStepWithTheMessageOnly(t *testing.T) {
	needTools(t, "bash")
	for _, tok := range []string{"pause-logging", "", "warning", "PAUSE-LOGGING"} {
		rec, res := runLocal(t, wfHead+"    steps:\n      - name: s\n        run: |\n          echo '::stop-commands::"+tok+"'\n          echo '::warning::still processed'\n", "j", nil)
		got := logOf(rec.step(t, "s"))
		if strings.Contains(got, "Unable to process command") || strings.Contains(got, "still processed\n") && strings.Contains(got, "##[warning]") {
			t.Errorf("%q: %s", tok, got)
		}
		if !strings.Contains(got, "##[error]"+unsupportedStopTokenDisabled) || rec.step(t, "s").Conclusion != ConclusionFailure || res.Conclusion != ConclusionFailure {
			t.Errorf("%q: %s", tok, got)
		}
	}
}

// ann-levels, ann-error-fails, match-basic: GitHub reports end_line = line (and
// end_column = col) when the command gave only the start.
func TestAnnotationEndsWhereItStartsByDefault(t *testing.T) {
	j := &job{}
	j.masker.Store(newMasker())
	a := j.annotationOf(issue{Level: LevelWarning, Props: map[string]string{"line": "2", "col": "3"}}, "m")
	if a.StartLine != 2 || a.EndLine != 2 || a.StartColumn != 3 || a.EndColumn != 3 {
		t.Errorf("%+v", a)
	}
	a = j.annotationOf(issue{Level: LevelWarning, Props: map[string]string{"line": "2", "endline": "5"}}, "m")
	if a.EndLine != 5 || a.EndColumn != 0 {
		t.Errorf("%+v", a)
	}
}

// sum-env: the file of a step is still there in the next one.
func TestFileCommandsOfAStepSurviveTheNextStep(t *testing.T) {
	needTools(t, "bash")
	rec, _ := runLocal(t, wfHead+`    steps:
      - run: echo "$GITHUB_STEP_SUMMARY" > "$RUNNER_TEMP/p"
      - name: next
        run: test -e "$(cat "$RUNNER_TEMP/p")" && echo yes || echo no
`, "j", nil)
	if got := logOf(rec.step(t, "next")); !strings.HasSuffix(got, "\nyes") {
		t.Error(got)
	}
}

// ctr-*, svc-*, act-docker-image: a step made by the runner is in the steps
// context under a 32-hex id (a Guid in "N" format), with its outcome and
// conclusion and no outputs.
func TestRunnerStepsAreInTheStepsContext(t *testing.T) {
	st := newTestState(t)
	j := &job{state: st}
	j.recordRunnerStep(resSucceeded)
	v, ok := st.Values(nil)["steps"].(*expr.Dictionary)
	if !ok || len(v.Pairs()) != 1 {
		t.Fatalf("%v", v)
	}
	p := v.Pairs()[0]
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p.Key) {
		t.Errorf("id %q", p.Key)
	}
	d, _ := p.Value.(*expr.Dictionary)
	if d == nil || strings.Join(d.Keys(), ",") != "outputs,outcome,conclusion" {
		t.Errorf("%v", p.Value)
	}
	if o, _ := d.Get("outcome"); o != expr.String("success") {
		t.Errorf("outcome %v", o)
	}
}

// sum-too-large: a summary of exactly 1 MiB is accepted (the runner tests the
// size of the file), 1 MiB plus one byte is refused with "got 1024k", 2 MiB
// with "got 2048k".
func TestStepSummarySizeLimit(t *testing.T) {
	needTools(t, "bash")
	rr, _ := runResults(t, wfHead+`    steps:
      - name: exact
        run: head -c $((1024 * 1024)) /dev/zero | tr '\0' b >> "$GITHUB_STEP_SUMMARY"
      - name: plus1
        run: head -c $((1024 * 1024 + 1)) /dev/zero | tr '\0' c >> "$GITHUB_STEP_SUMMARY"
      - name: two
        run: head -c $((2 * 1024 * 1024)) /dev/zero | tr '\0' d >> "$GITHUB_STEP_SUMMARY"
`, nil)
	const tail = ", supports content up to a size of 1024k, got "
	if got := logOf(rr.step(t, "exact")); strings.Contains(got, "upload aborted") {
		t.Errorf("exact: %.200s", got)
	}
	if r := rr.results[2]; !r.HasSummary || len(r.Summary) != stepSummaryLimit {
		t.Errorf("exact: has=%v len=%d", r.HasSummary, len(r.Summary))
	}
	if got := logOf(rr.step(t, "plus1")); !strings.Contains(got, "##[error]$GITHUB_STEP_SUMMARY upload aborted"+tail+"1024k.") {
		t.Errorf("plus1: %.300s", got)
	}
	if got := logOf(rr.step(t, "two")); !strings.Contains(got, "##[error]$GITHUB_STEP_SUMMARY upload aborted"+tail+"2048k.") {
		t.Errorf("two: %.300s", got)
	}
}

// An invalid stop-commands token ends the process: the step does not wait for
// the script (here 600 s) and fails with the endToken message.
func TestInvalidStopTokenStopsTheProcess(t *testing.T) {
	needTools(t, "bash", "sleep")
	start := time.Now()
	rec, res := runLocal(t, wfHead+"    steps:\n      - name: s\n        run: |\n          echo '::stop-commands::warning'\n          sleep 600\n          echo after\n", "j", nil)
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("the step took %v", d)
	}
	got := logOf(rec.step(t, "s"))
	if !strings.Contains(got, "##[error]"+unsupportedStopTokenDisabled) || strings.Contains(got, "The operation was canceled.") {
		t.Error(got)
	}
	for _, l := range rec.step(t, "s").Log {
		if strings.TrimSpace(l) == "after" {
			t.Errorf("output after the fatal line was logged: %q", l)
		}
	}
	if rec.step(t, "s").Conclusion != ConclusionFailure || res.Conclusion != ConclusionFailure {
		t.Errorf("%v %v", rec.step(t, "s").Conclusion, res.Conclusion)
	}
}

// Output written after the fatal line, a lot of it, does not block the process
// or the executor.
func TestInvalidStopTokenDrainsHighVolumeOutput(t *testing.T) {
	needTools(t, "bash", "yes", "head")
	done := make(chan struct{})
	var rec *recorder
	go func() {
		defer close(done)
		rec, _ = runLocal(t, wfHead+"    steps:\n      - name: s\n        run: |\n          echo '::stop-commands::'\n          yes '::warning::x' | head -c 200000000\n          sleep 600\n", "j", nil)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock")
	}
	got := logOf(rec.step(t, "s"))
	if !strings.Contains(got, "##[error]"+unsupportedStopTokenDisabled) || strings.Contains("\n"+got+"\n", "\n::warning::x\n") || strings.Contains(got, "##[warning]x") {
		t.Errorf("%.300s", got)
	}
}
