// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
	"time"
)

const testToken = "ghs_testtoken123"

func tokenContexts() string {
	return strings.Replace(testGitHubContext("main"), `"github":{`, `"github":{"token":"`+testToken+`",`, 1)
}

// The name of a step that holds the token is masked before it reaches the
// server (StepStarted and StepCompleted).
func TestStepNameIsMasked(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    steps:
      - name: leak ${{ github.token }}
        run: echo hi
      - name: leak2 ${{ secrets.GITHUB_TOKEN }} ${{ env.X }}
        env:
          X: x
        run: echo hi
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) { c.ContextsJSON = tokenContexts() })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	for _, s := range rec.steps {
		if strings.Contains(s.Name, testToken) {
			t.Errorf("step name %q holds the token", s.Name)
		}
	}
	rec.step(t, "leak ***")
	rec.step(t, "leak2 *** x")
}

// A job output that holds the token is dropped with GitHub's warning; the
// other outputs stay.
func TestJobOutputWithTokenIsDropped(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    outputs:
      leak: ${{ steps.s.outputs.v }}
      fine: ${{ steps.s.outputs.w }}
    steps:
      - id: s
        env:
          T: ${{ github.token }}
        run: |
          echo "v=prefix-$T-suffix" >> "$GITHUB_OUTPUT"
          echo "w=ok" >> "$GITHUB_OUTPUT"
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) { c.ContextsJSON = tokenContexts() })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if _, has := res.Outputs["leak"]; has || res.Outputs["fine"] != "ok" {
		t.Errorf("outputs %v", res.Outputs)
	}
	for _, v := range res.Outputs {
		if strings.Contains(v, testToken) {
			t.Errorf("the token is in an output: %q", v)
		}
	}
	if log := logOf(rec.step(t, CompleteJobName)); !strings.Contains(log, "##[warning]Skip output 'leak' since it may contain secret.") {
		t.Errorf("warning missing:\n%s", log)
	}
}

// Everything the executor reports goes through the masker: the names of the
// steps, the log, and the error of the job (no real run puts a token there, so
// the reporter is exercised directly).
func TestMaskReporter(t *testing.T) {
	rec := newRecorder()
	j := &job{}
	j.masker.Store(newMasker(testToken))
	r := &maskReporter{j: j, next: rec}
	r.StepStarted(1, "name "+testToken, time.Now())
	r.Log(1, time.Now(), "line "+testToken)
	r.StepCompleted(1, "name "+testToken, ConclusionSuccess, time.Now())
	r.JobCompleted(Result{Conclusion: ConclusionFailure, Error: "boom " + testToken})
	if s := rec.steps[0]; s.Name != "name ***" || s.Log[0] != "line ***" {
		t.Errorf("%+v", s)
	}
	if rec.result.Error != "boom ***" {
		t.Errorf("error %q", rec.result.Error)
	}
}

// The token is masked before a message is cut at 4096 bytes: a token split by
// the cut would otherwise leave its start in the line.
func TestMaskBeforeTruncation(t *testing.T) {
	needTools(t, "bash")
	pad := strings.Repeat("a", maxIssueMessageLength-5)
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    steps:
      - run: |
          echo "::error::` + pad + `${{ github.token }}"
`
	rec, _ := runLocal(t, yaml, "main", func(c *Config) { c.ContextsJSON = tokenContexts() })
	for _, s := range rec.steps {
		for _, l := range s.Log {
			if strings.Contains(l, "ghs_") {
				t.Errorf("a piece of the token leaked: %q", l[len(l)-20:])
			}
		}
	}
}
