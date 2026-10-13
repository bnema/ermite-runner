// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
)

// Recorded on github.com (ctx-github-names): names that only read the
// contexts of the job message are evaluated when the job starts, with
// github.job empty, while the script of the step runs with the real job key.
func TestNamesSeeAnEmptyGithubJob(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  first-job:
    name: Display name of ${{ github.job }}
    runs-on: ubuntu-24.04
    steps:
      - run: echo "job=${{ github.job }}"
      - name: late ${{ github.job }} ${{ env.X }}
        env:
          X: x
        run: echo hi
`
	rec, res := runLocal(t, yaml, "first-job", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := rec.steps[0].Log; got[len(got)-1] != "Complete job name: Display name of" {
		t.Errorf("job name: %v", got)
	}
	s := rec.step(t, `Run echo "job="`)
	if !strings.Contains(logOf(s), `##[group]Run echo "job=first-job"`) || !strings.Contains(logOf(s), "\njob=first-job") {
		t.Errorf("the script must see the real job key:\n%s", logOf(s))
	}
	// A name that reads env is not available at job start: evaluated with the
	// step's contexts, where github.job is the key.
	rec.step(t, "late first-job x")
}
