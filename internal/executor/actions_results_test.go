// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// actionResults runs a workflow of actions with a recorder that keeps the
// results of the steps (annotations, summaries, outcomes).
func actionResults(t *testing.T, tree files, yaml string, tweak func(*Config)) (*resultRecorder, Result) {
	t.Helper()
	needTools(t, "bash")
	rr := newResultRecorder()
	_, res := runLocal(t, wfHead+"    steps:\n"+yaml, "j", func(c *Config) {
		writeFiles(t, c.Layout.Workspace, tree)
		c.Actions.ExternalsDir = fakeNode(t)
		c.Reporter = rr
		c.SendAnnotations, c.SendSummaries = true, true
		if tweak != nil {
			tweak(c)
		}
	})
	return rr, res
}

func countLevel(as []Annotation, l AnnotationLevel) int {
	n := 0
	for _, a := range as {
		if a.Level == l {
			n++
		}
	}
	return n
}

// Commands from a node action: annotations, masks, a problem matcher, the step
// summary and the saved state work as in a run step.
func TestNodeActionCommands(t *testing.T) {
	tree := files{
		"n/action.yml": "name: n\nruns:\n  using: node20\n  main: main.sh\n",
		"n/main.sh": `echo "::error title=T,file=a.txt,line=3::broken"
echo "::add-mask::hunter2"
echo "the password is hunter2"
echo "::warning::careful"
echo "# Summary hunter2" >> "$GITHUB_STEP_SUMMARY"
echo '{"problemMatcher":[{"owner":"m","pattern":[{"regexp":"^bad: (.+)$","message":1}]}]}' > "$RUNNER_TEMP/m.json"
echo "::add-matcher::$RUNNER_TEMP/m.json"
echo "bad: from the matcher"
`,
	}
	rr, res := actionResults(t, tree, "      - uses: ./n\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rr.steps[1]))
	}
	log := logOf(rr.step(t, "Run ./n"))
	if strings.Contains(log, "hunter2") || !strings.Contains(log, "the password is ***") {
		t.Errorf("add-mask:\n%s", log)
	}
	as := rr.results[2].Annotations
	if len(as) != 3 || as[0].Level != LevelFailure || as[0].Title != "T" || as[0].File != "a.txt" || as[0].StartLine != 3 ||
		as[1].Level != LevelWarning || as[2].Level != LevelFailure || as[2].Message != "from the matcher" {
		t.Errorf("annotations %+v", as)
	}
	if got := rr.results[2].Summary; got != "# Summary ***\n" {
		t.Errorf("summary %q", got)
	}
	// An ::error:: fails the step only through its exit code, not by itself, but
	// the command result merges: the step succeeds here, as the script exited 0.
	if rr.step(t, "Run ./n").Conclusion != ConclusionSuccess {
		t.Errorf("conclusion %s", rr.step(t, "Run ./n").Conclusion)
	}
}

// The state of an action is kept under one key for its pre, main and post stages;
// the first spelling of a name wins and entries come in the order they were saved.
// (The runner never runs pre of a local action, so this one is a repository action.)
func TestActionStateKeepsFirstSpellingAndOrder(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/o/p/v1": tarOf(t, "o-p-x", files{
		"action.yml": "name: p\nruns:\n  using: node20\n  pre: pre.sh\n  main: main.sh\n  post: post.sh\n",
		"pre.sh":     "echo \"Zed=1\" >> \"$GITHUB_STATE\"\necho \"alpha=2\" >> \"$GITHUB_STATE\"\n",
		"main.sh":    "echo \"ZED=3\" >> \"$GITHUB_STATE\"\necho \"main: $(env | grep '^STATE_' | sort | tr '\\n' ' ')\"\n",
		"post.sh":    "echo \"post: $(env | grep '^STATE_' | sort | tr '\\n' ' ')\"\n",
	})})
	rr, res := actionResults(t, files{}, "      - uses: o/p@v1\n", func(c *Config) { c.Actions.Resolver = forge.resolver("") })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rr.step(t, "Run o/p@v1")); !strings.Contains(got, "main: STATE_Zed=1 STATE_alpha=2 ") {
		t.Errorf("main:\n%s", got)
	}
	if got := logOf(rr.step(t, "Post Run o/p@v1")); !strings.Contains(got, "post: STATE_Zed=3 STATE_alpha=2 ") {
		t.Errorf("post:\n%s", got)
	}
}

// Two uses of one action have two states, and the ::save-state:: command and the
// file end in the same one.
func TestActionStateIsPerStep(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/o/q/v1": tarOf(t, "o-q-x", files{
		"action.yml": "name: q\ninputs:\n  v:\n    default: d\nruns:\n  using: node20\n  main: main.sh\n  post: post.sh\n",
		"main.sh":    "echo \"::save-state name=v::$INPUT_V\"\necho \"f=$INPUT_V\" >> \"$GITHUB_STATE\"\n",
		"post.sh":    "echo \"post sees v=$STATE_v f=$STATE_f\"\n",
	})})
	rr, res := actionResults(t, files{}, "      - uses: o/q@v1\n        with:\n          v: one\n      - uses: o/q@v1\n        with:\n          v: two\n", func(c *Config) { c.Actions.Resolver = forge.resolver("") })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	var posts []string
	for _, s := range rr.steps {
		if strings.HasPrefix(s.Name, "Post ") {
			posts = append(posts, logOf(s))
		}
	}
	if len(posts) != 2 || !strings.Contains(posts[0], "post sees v=two f=two") || !strings.Contains(posts[1], "post sees v=one f=one") {
		t.Errorf("%q", posts)
	}
}

// ::error:: of a node action and of the inner steps of a composite land on the
// step of the workflow; the limit of 10 per level counts every inner step.
func TestCompositeAnnotationsAreCountedOnTheOuterStep(t *testing.T) {
	inner := func(n int, level string) string {
		return "      run: |\n        for i in $(seq 1 " + fmt.Sprint(n) + "); do echo \"::" + level + "::" + level + " $i\"; done\n"
	}
	tree := files{
		"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - shell: bash\n" + inner(8, "error") +
			"    - shell: bash\n" + inner(8, "error") + "    - shell: bash\n" + inner(3, "warning") + "    - uses: ./n\n",
		"n/action.yml": "name: n\nruns:\n  using: node20\n  main: main.sh\n",
		"n/main.sh":    "echo '::notice::from the node action inside'\n",
	}
	rr, res := actionResults(t, tree, "      - uses: ./c\n      - run: echo \"::error::the next step\"\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	as := rr.results[2].Annotations
	if countLevel(as, LevelFailure) != 10 || countLevel(as, LevelWarning) != 3 || countLevel(as, LevelNotice) != 1 {
		t.Errorf("failure %d (want 10 of 16), warning %d, notice %d: %+v", countLevel(as, LevelFailure), countLevel(as, LevelWarning), countLevel(as, LevelNotice), as)
	}
	if as[0].Message != "error 1" {
		t.Errorf("the first ones are kept: %+v", as[0])
	}
	// Nothing leaks to the step after, and the inner steps have no number of their own.
	if got := rr.results[3].Annotations; len(got) != 1 || got[0].Message != "the next step" {
		t.Errorf("next step %+v", got)
	}
	// A local action inside a composite is assumed to have a post step, so the
	// composite has one too (ActionManager.cs:643-654): "Post Run ./c" is empty.
	if got := strings.Join(rr.names(), "|"); got != "Set up job|Run ./c|Run echo \"::error::the next step\"|Post Run ./c|Complete job" {
		t.Errorf("steps %v", rr.names())
	}
}

// The outer step's own issues come first: a composite whose inner steps have used
// the 10 slots of "failure" cannot push out the error the runner logs for the
// outer step itself... in the runner's order, which is chronological.
func TestCompositeSummaryAndMaskBelongToTheOuterStep(t *testing.T) {
	tree := files{"c/action.yml": `name: c
runs:
  using: composite
  steps:
    - shell: bash
      run: |
        echo "::add-mask::inner-secret"
        echo "first" >> "$GITHUB_STEP_SUMMARY"
    - shell: bash
      run: |
        echo "second inner-secret" >> "$GITHUB_STEP_SUMMARY"
        echo "visible: inner-secret"
`}
	rr, res := actionResults(t, tree, "      - uses: ./c\n      - run: echo \"after - inner-secret\"\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := rr.results[2].Summary; got != "first\nsecond ***\n" {
		t.Errorf("summary %q", got)
	}
	if got := logOf(rr.steps[1]) + logOf(rr.steps[2]); strings.Contains(got, "inner-secret\n") && !strings.Contains(got, "***") || strings.Contains(strings.ReplaceAll(got, "::add-mask::inner-secret", ""), "visible: inner-secret") || strings.Contains(logOf(rr.steps[2]), "after - inner-secret") {
		t.Errorf("a mask added by an inner step must hide the value everywhere:\n%s", got)
	}
}

// The archive token, and its Basic form, are masked wherever they appear.
func TestArchiveTokenAndBasicFormAreMasked(t *testing.T) {
	const token = "archive-tok-123"
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	forge := newForge(t, map[string][]byte{"/tarball/o/t/v1": tarOf(t, "o-t-x", files{
		"action.yml": "name: t\nruns:\n  using: node20\n  main: main.sh\n",
		"main.sh":    "echo \"token " + token + " basic " + basic + "\"\n"})})
	rr, res := actionResults(t, files{}, "      - uses: o/t@v1\n", func(c *Config) { c.Actions.Resolver = forge.resolver(token) })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rr.step(t, "Run o/t@v1")); !strings.Contains(got, "token *** basic ***") {
		t.Errorf("%s", got)
	}
}

// timeout-minutes on an action step, on a docker step and on an inner step of a
// composite: the step fails with the runner's message and the job goes on.
func TestActionStepTimeouts(t *testing.T) {
	skew := -(60*time.Second - 1500*time.Millisecond)
	tree := files{
		"slow/action.yml": "name: slow\nruns:\n  using: node20\n  main: main.sh\n",
		"slow/main.sh":    "sleep 30\n",
		"c/action.yml":    "name: c\nruns:\n  using: composite\n  steps:\n    - name: inner\n      shell: bash\n      run: sleep 30\n    - shell: bash\n      if: always()\n      run: echo inner-after\n",
	}
	start := time.Now()
	rr, res := actionResults(t, tree, `      - name: node
        timeout-minutes: 1
        uses: ./slow
      - name: after
        if: always()
        run: echo after-ran
`, func(c *Config) { c.StepTimeoutSkew = skew })
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the timeout did not stop the action: %v", time.Since(start))
	}
	n := rr.step(t, "node")
	if n.Conclusion != ConclusionFailure || !strings.Contains(logOf(n), "##[error]The action 'node' has timed out after 1 minutes.") || strings.Contains(logOf(n), "The operation was canceled.") {
		t.Errorf("%s\n%s", n.Conclusion, logOf(n))
	}
	if rr.step(t, "after").Conclusion != ConclusionSuccess {
		t.Error("the job is not cancelled by a step timeout")
	}
	// The inner step of a composite action.
	start = time.Now()
	rr, res = actionResults(t, tree, "      - name: comp\n        timeout-minutes: 1\n        uses: ./c\n", func(c *Config) { c.StepTimeoutSkew = skew })
	if res.Conclusion != ConclusionFailure || time.Since(start) > 10*time.Second {
		t.Fatalf("%+v %v", res, time.Since(start))
	}
	log := logOf(rr.step(t, "comp"))
	// The inner step has what is left of the timeout of the step that uses the
	// action and fails with a message that names no step (the runner's composite
	// handler). The time is spent after it: the steps that follow are cancelled at
	// once, as the runner's are (their token is cancelled when they are created).
	if strings.Count(log, "##[error]The action has timed out.") < 1 || strings.Contains(log, "after 0 minutes") || strings.Contains(log, "The operation was canceled.") {
		t.Errorf("%s", log)
	}
}

// A docker action's output goes through the same commands: annotations, masks,
// matchers.
func TestDockerActionCommands(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
run)
  echo "::add-mask::dkr-secret"
  echo "value dkr-secret"
  echo "::error file=x.go,line=2::from docker"
  echo "::group::g"
  echo "::endgroup::"
  ;;
esac
exit 0
`
	writeFiles(t, dir, files{"docker": script})
	rr, res := actionResults(t, files{}, "      - uses: docker://alpine:3\n", func(c *Config) { c.Actions.DockerPath = dir + "/docker" })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	log := logOf(rr.step(t, "Run alpine:3"))
	if strings.Contains(log, "value dkr-secret") || !strings.Contains(log, "value ***") {
		t.Errorf("%s", log)
	}
	if as := rr.results[3].Annotations; len(as) != 1 || as[0].Message != "from docker" || as[0].File != "x.go" || as[0].StartLine != 2 {
		t.Errorf("annotations %+v", rr.results)
	}
}

// The outcome of an action step, before continue-on-error, reaches the reporter.
func TestActionOutcomeIsReported(t *testing.T) {
	tree := files{"f/action.yml": "name: f\nruns:\n  using: node20\n  main: main.sh\n", "f/main.sh": "exit 1\n"}
	rr, res := actionResults(t, tree, "      - uses: ./f\n        continue-on-error: true\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := rr.results[2].Outcome; got != ConclusionFailure {
		t.Errorf("outcome %q, want failure", got)
	}
	if rr.step(t, "Run ./f").Conclusion != ConclusionSuccess {
		t.Error("continue-on-error")
	}
	// A failing action says why in an annotation too: the exit code is an error of
	// the runner, which GitHub reports as an annotation for run steps; node
	// actions only mark the step failed.
	if as := rr.results[2].Annotations; len(as) != 0 {
		t.Errorf("annotations %+v", as)
	}
}
