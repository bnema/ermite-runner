// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
)

// Recorded on github.com (expr-runtime-errors): a fromJSON that fails in a
// step's env or run is "The template is not valid. <file> (Line: l, Col: c):
// <Json.NET message>"; in a step's if there is no location and the exception
// class comes first. The line is the line of the expression in the workflow
// file the executor was given (GitHub's Line 80 and the 90 of a current
// recording differ only because the harness injected a shorter dump step then).
func TestFromJSONErrorsAreTemplateErrors(t *testing.T) {
	needTools(t, "bash")
	const msg = "Unexpected character encountered while parsing value: n. Path '', line 0, position 0."
	yaml := `on: push
env:
  NOT_JSON: not json
jobs:
  bad-env:
    runs-on: ubuntu-24.04
    steps:
      - name: bad env expression
        env:
          BAD: ${{ fromJSON(env.NOT_JSON) }}
        run: echo SHOULD-NOT-RUN
      - name: after
        if: always()
        run: echo after-ran
  bad-if:
    runs-on: ubuntu-24.04
    steps:
      - name: bad if expression
        if: fromJSON(env.NOT_JSON)
        run: echo SHOULD-NOT-RUN
  bad-run:
    runs-on: ubuntu-24.04
    steps:
      - name: bad run expression
        run: echo ${{ fromJSON(env.NOT_JSON) }}
`
	for _, tc := range []struct{ job, step, want string }{
		{"bad-env", "bad env expression", "##[error]The template is not valid. .github/workflows/ci.yml (Line: 10, Col: 16): " + msg},
		{"bad-if", "bad if expression", "##[error]The template is not valid. Newtonsoft.Json.JsonReaderException: " + msg},
		{"bad-run", "bad run expression", "##[error]The template is not valid. .github/workflows/ci.yml (Line: 25, Col: 14): " + msg},
	} {
		t.Run(tc.job, func(t *testing.T) {
			rec, res := runLocal(t, yaml, tc.job, nil)
			if res.Conclusion != ConclusionFailure {
				t.Fatalf("%+v", res)
			}
			s := rec.step(t, tc.step)
			if s.Conclusion != ConclusionFailure || len(s.Log) != 1 || s.Log[0] != tc.want {
				t.Errorf("%s: %s\nwant %s", s.Conclusion, strings.Join(s.Log, "\n"), tc.want)
			}
			if tc.job == "bad-env" && rec.step(t, "after").Conclusion != ConclusionSuccess {
				t.Error("the always() step must run")
			}
		})
	}
}
