// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const matcherSetup = `      - name: prepare
        run: |
          mkdir -p src
          git init -q .
          git remote add origin "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY"
          echo x > src/main.go
          cat > m.json <<'JSON'
          {"problemMatcher":[{"owner":"go","severity":"warning","pattern":[{"regexp":"^(.+):(\\d+):(\\d+): (error|warning)?: ?(.*)$","file":1,"line":2,"column":3,"severity":4,"message":5}]}]}
          JSON
`

// The workspace of runLocal is hello/hello under the home; the setup step
// runs in it, so the repository check of OutputManager.GetRepositoryPath finds
// .git/config there.
func TestProblemMatcherEndToEnd(t *testing.T) {
	needTools(t, "bash", "git")
	rr, res := runResults(t, wfHead+`    steps:
`+matcherSetup+`      - name: match
        run: |
          echo "::add-matcher::m.json"
          echo "src/main.go:12:5: error: bad thing"
          echo "src/main.go:7:1: : just a warning"
          echo "src/missing.go:1:1: error: no such file"
          echo "nothing here"
          echo "::remove-matcher owner=go::"
          echo "src/main.go:12:5: error: not matched"
`, nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	got := rr.results[3].Annotations
	want := []Annotation{
		{Level: LevelFailure, Message: "bad thing", File: "src/main.go", StartLine: 12, StartColumn: 5},
		{Level: LevelWarning, Message: "just a warning", File: "src/main.go", StartLine: 7, StartColumn: 1},
		{Level: LevelFailure, Message: "no such file", StartLine: 1, StartColumn: 1}, // the file does not exist: dropped
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("annotations\n got %+v\nwant %+v", got, want)
	}
	log := logOf(rr.step(t, "match"))
	for _, w := range []string{"##[error]src/main.go:12:5: error: bad thing", "##[warning]src/main.go:7:1: : just a warning", "\nnothing here\n", "\nsrc/main.go:12:5: error: not matched"} {
		if !strings.Contains(log+"\n", w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
	// the matched lines are not logged a second time as plain text
	_, body, _ := strings.Cut(log, "##[endgroup]\n")
	if strings.Count(body, "bad thing") != 1 {
		t.Errorf("a matched line is logged twice:\n%s", log)
	}
}

// AC6b through a whole step: the catastrophic matcher is retried 3 times (one
// second each), removed with the runner's warning, and the step goes on; the
// other matcher keeps working. OutputManager.Remove only edits the list of the
// step (OutputManager.cs:300-313) while the next step starts from the matchers
// of the job (ExecutionContext.GetMatchers), so the next step meets the
// matcher again and removes it again after 3 more timeouts.
func TestCatastrophicMatcherIsRemovedAfterThreeTimeouts(t *testing.T) {
	needTools(t, "bash")
	start := time.Now()
	rr, res := runResults(t, wfHead+`    steps:
      - name: prepare
        run: |
          cat > bad.json <<'JSON'
          {"problemMatcher":[{"owner":"catastrophic","pattern":[{"regexp":"^((a+)+)$","message":1}]}]}
          JSON
          cat > good.json <<'JSON'
          {"problemMatcher":[{"owner":"good","severity":"warning","pattern":[{"regexp":"^good: (.+)$","message":1}]}]}
          JSON
      - name: catastrophic
        run: |
          echo "::add-matcher::bad.json"
          echo "::add-matcher::good.json"
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!"
          echo "good: still alive"
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!"
      - name: next step
        run: |
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!"
          echo "good: next step"
`, nil)
	took := time.Since(start)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if took > 15*time.Second {
		t.Errorf("the step stalled: %v", took)
	}
	log := logOf(rr.step(t, "catastrophic"))
	const removing = "##[warning]Removing issue matcher 'catastrophic'. Matcher failed 3 times. Error: The Regex engine has timed out while trying to match a pattern to an input string."
	if strings.Count(log, removing) != 1 {
		t.Errorf("removal message missing or repeated:\n%s", log)
	}
	if !strings.Contains(log, "\naaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!\n##[warning]good: still alive") && !strings.Contains(log, "##[warning]good: still alive") {
		t.Errorf("the good matcher stopped working:\n%s", log)
	}
	// Only one removal: the second long line did not time out again.
	if n := len(rr.results[3].Annotations); n != 2 {
		t.Errorf("%d annotations: %+v", n, rr.results[3].Annotations)
	}
	next := logOf(rr.step(t, "next step"))
	if strings.Count(next, "Removing issue matcher 'catastrophic'") != 1 || !strings.Contains(next, "##[warning]good: next step") {
		t.Errorf("next step:\n%s", next)
	}
}

func TestMatcherTimeoutFromEnvironment(t *testing.T) {
	needTools(t, "bash")
	start := time.Now()
	rr, _ := runResults(t, wfHead+`    steps:
      - name: fast
        run: |
          echo '{"problemMatcher":[{"owner":"c","pattern":[{"regexp":"^((a+)+)$","message":1}]}]}' > bad.json
          echo "::add-matcher::bad.json"
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!"
`, func(c *Config) {
		c.Getenv = func(k string) string { return map[string]string{matcherTimeoutKey: "0:0:0.05"}[k] }
	})
	if time.Since(start) > 2500*time.Millisecond {
		t.Errorf("the timeout of the environment was not used: %v", time.Since(start))
	}
	if !strings.Contains(logOf(rr.step(t, "fast")), "Removing issue matcher 'c'") {
		t.Error(logOf(rr.step(t, "fast")))
	}
}

func TestAddMatcherErrors(t *testing.T) {
	needTools(t, "bash")
	rr, res := runResults(t, wfHead+`    steps:
      - name: bad
        run: |
          echo "::add-matcher::"
          echo "::remove-matcher::"
          echo "::remove-matcher owner=x::file.json"
          echo "::add-matcher::missing.json"
      - name: invalid
        if: always()
        run: |
          echo '{"problemMatcher":[{"owner":"m","pattern":[{"regexp":"(","message":1}]}]}' > b.json
          echo "::add-matcher::b.json"
`, nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	log := logOf(rr.step(t, "bad"))
	for _, w := range []string{
		"##[warning]File path must be specified.",
		"##[warning]Either an owner name or a file path must be specified in ##[remove-matcher] command.",
		"##[warning]Either specify an owner name or a file path in ##[remove-matcher] command. Both values cannot be set.",
		"##[error]Unable to process command '::add-matcher::missing.json' successfully.",
		"##[error]Could not find file '",
	} {
		if !strings.Contains(log, w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
	if !strings.Contains(logOf(rr.step(t, "invalid")), "##[error]Unable to process command '::add-matcher::b.json' successfully.") {
		t.Error(logOf(rr.step(t, "invalid")))
	}
}

func TestStepSummary(t *testing.T) {
	needTools(t, "bash")
	rr, _ := runResults(t, wfHead+`    steps:
      - name: write
        run: |
          echo "::add-mask::sekret-value"
          printf '# Title\nvalue sekret-value\r\nlast line without newline' >> "$GITHUB_STEP_SUMMARY"
      - name: empty
        run: 'true'
      - name: huge
        run: head -c 1048577 /dev/zero | tr '\0' x >> "$GITHUB_STEP_SUMMARY"
      - name: exactly one MiB
        run: head -c 1048576 /dev/zero | tr '\0' x >> "$GITHUB_STEP_SUMMARY"
`, nil)
	if r := rr.results[2]; !r.HasSummary || r.Summary != "# Title\nvalue ***\nlast line without newline\n" {
		t.Errorf("summary %q %v", r.Summary, r.HasSummary)
	}
	if rr.results[3].HasSummary {
		t.Error("no file content, no summary")
	}
	if rr.results[4].HasSummary {
		t.Error("an oversized summary is not sent")
	}
	if !strings.Contains(logOf(rr.step(t, "huge")), "##[error]$GITHUB_STEP_SUMMARY upload aborted, supports content up to a size of 1024k, got 1024k.") {
		t.Errorf("%s", logOf(rr.step(t, "huge"))[:200])
	}
	// 1 MiB of x plus one line feed per line is over the limit once rewritten:
	// the runner adds the "\n"; the contract allows 1 MiB.
	if r := rr.results[5]; r.HasSummary && len(r.Summary) > stepSummaryLimit {
		t.Errorf("a summary above the contract limit is sent: %d", len(r.Summary))
	}
}

func TestSummaryOffWithoutTheFeature(t *testing.T) {
	needTools(t, "bash")
	rr, _ := runResults(t, wfHead+"    steps:\n      - run: echo hi >> \"$GITHUB_STEP_SUMMARY\"\n", func(c *Config) { c.SendSummaries = false })
	if rr.results[2].HasSummary {
		t.Error("summary sent without ci.summaries")
	}
}

func TestStepOutcome(t *testing.T) {
	needTools(t, "bash")
	rr, res := runResults(t, wfHead+`    steps:
      - run: exit 1
        continue-on-error: true
      - run: 'true'
      - run: exit 2
`, nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatal(res)
	}
	if r := rr.results[2]; r.Outcome != ConclusionFailure || rr.steps[1].Conclusion != ConclusionSuccess {
		t.Errorf("continue-on-error: outcome %v conclusion %v", r.Outcome, rr.steps[1].Conclusion)
	}
	if r := rr.results[3]; r.Outcome != ConclusionSuccess {
		t.Errorf("%+v", r)
	}
	if r := rr.results[4]; r.Outcome != ConclusionFailure {
		t.Errorf("%+v", r)
	}
}

func TestStepTimeout(t *testing.T) {
	needTools(t, "bash")
	skew := -(60*time.Second - 700*time.Millisecond) // one minute becomes 0.7 s
	rr, res := runResults(t, wfHead+`    steps:
      - name: slow
        id: slow
        timeout-minutes: 1
        run: |
          trap 'echo got-int; exit 130' INT
          echo "before=written" >> "$GITHUB_OUTPUT"
          sleep 30 & wait
      - name: after success
        run: echo "ran after the timeout"
      - name: after failure
        if: failure()
        run: echo "outcome=${{ steps.slow.outcome }} conclusion=${{ steps.slow.conclusion }} status=${{ job.status }} out=${{ steps.slow.outputs.before }}"
      - name: cancelled()
        if: cancelled()
        run: echo "cancelled is true"
      - name: soft
        id: soft
        timeout-minutes: 1
        continue-on-error: true
        if: always()
        run: sleep 30
      - name: results
        if: always()
        run: echo "soft=${{ steps.soft.outcome }}/${{ steps.soft.conclusion }}"
      - name: fractional
        timeout-minutes: 0.9
        run: sleep 1
      - name: expression
        timeout-minutes: ${{ fromJSON('0') }}
        run: echo no timeout
`, func(c *Config) { c.StepTimeoutSkew = skew })
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	slow := rr.step(t, "slow")
	if slow.Conclusion != ConclusionFailure {
		t.Errorf("conclusion %v", slow.Conclusion)
	}
	log := logOf(slow)
	if !strings.Contains(log, "##[error]The action 'slow' has timed out after 1 minutes.") || strings.Contains(log, "The operation was canceled.") {
		t.Errorf("log:\n%s", log)
	}
	if !strings.Contains(log, "got-int") {
		t.Errorf("the step did not get SIGINT first:\n%s", log)
	}
	if a := rr.results[2].Annotations; len(a) == 0 || a[len(a)-1].Message != "The action 'slow' has timed out after 1 minutes." {
		t.Errorf("annotations %+v", a)
	}
	if rr.results[2].Outcome != ConclusionFailure {
		t.Error("outcome")
	}
	if rr.step(t, "after success").Conclusion != ConclusionSkipped {
		t.Error("success() is false after a timeout")
	}
	if l := logOf(rr.step(t, "after failure")); !strings.HasSuffix(l, "outcome=failure conclusion=failure status=failure out=written") {
		t.Errorf("after failure: %s", l)
	}
	if rr.step(t, "cancelled()").Conclusion != ConclusionSkipped {
		t.Error("a step timeout is not a cancellation")
	}
	if l := logOf(rr.step(t, "results")); !strings.HasSuffix(l, "soft=failure/success") {
		t.Errorf("results: %s", l)
	}
	if rr.step(t, "fractional").Conclusion != ConclusionSkipped && rr.step(t, "fractional").Conclusion != ConclusionSuccess {
		t.Error("fractional")
	}
}

func TestStepTimeoutExpressionError(t *testing.T) {
	needTools(t, "bash")
	rr, res := runResults(t, wfHead+`    steps:
      - name: bad
        env:
          LIMIT: abc
        timeout-minutes: ${{ env.LIMIT }}
        run: echo ran
`, nil)
	log := logOf(rr.step(t, "bad"))
	if !strings.Contains(log, "##[error]An error occurred when attempting to determine the step timeout.") || !strings.Contains(log, "\nran") {
		t.Errorf("%s", log)
	}
	if res.Conclusion != ConclusionFailure && res.Conclusion != ConclusionSuccess {
		t.Fatal(res)
	}
}

// save-state keeps the state per step, for the post phase of an action (F);
// names are case-insensitive (IntraActionState uses OrdinalIgnoreCase).
func TestSaveStateIsKeptPerStep(t *testing.T) {
	j := &job{}
	j.saveState("2", "Foo", "bar")
	j.saveState("2", "FOO", "baz")
	j.saveState("3", "x", "y")
	if got := j.intraActionState("2"); len(got) != 1 || got["foo"] != "baz" {
		t.Errorf("%v", got)
	}
	if got := j.intraActionState("4"); len(got) != 0 {
		t.Errorf("%v", got)
	}
}
