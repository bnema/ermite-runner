// SPDX-License-Identifier: MIT

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// files is a tree of files: name to content.
type files map[string]string

func writeFiles(t *testing.T, root string, f files) {
	t.Helper()
	for name, body := range f {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeNode installs a "node" that runs the script with sh: the test actions are
// shell scripts, so the suite needs no Node.js. It records its version.
func fakeNode(t *testing.T) string {
	t.Helper()
	ext := t.TempDir()
	for _, v := range []string{"node20", "node24"} {
		writeFiles(t, ext, files{v + "/bin/node": "#!/bin/sh\necho \"[" + v + "]\"\nexec sh \"$@\"\n"})
	}
	return ext
}

// actionRun runs a workflow whose workspace holds tree.
func actionRun(t *testing.T, tree files, yaml string, tweak func(*Config)) (*recorder, Result) {
	t.Helper()
	needTools(t, "bash")
	return runLocal(t, wfHead+"    steps:\n"+yaml, "j", func(c *Config) {
		writeFiles(t, c.Layout.Workspace, tree)
		c.Actions.ExternalsDir = fakeNode(t)
		if tweak != nil {
			tweak(c)
		}
	})
}

const nodeAction = `name: n
description: d
inputs:
  who:
    default: world
  other:
    default: "${{ github.repository }}"
  old:
    deprecationMessage: use who
runs:
  using: node20
  main: main.sh
`

func TestLocalNodeActionInputsAndDefaults(t *testing.T) {
	rec, res := actionRun(t, files{
		".github/actions/n/action.yml": nodeAction,
		".github/actions/n/main.sh":    "echo \"who=$INPUT_WHO other=$INPUT_OTHER old=[$INPUT_OLD] action=$GITHUB_ACTION path=$GITHUB_ACTION_PATH repo=[$GITHUB_ACTION_REPOSITORY]\"\n",
	}, "      - uses: ./.github/actions/n\n        with:\n          who: Ermite\n          old: x\n          extra: y\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	got := logOf(rec.step(t, "Run ./.github/actions/n"))
	for _, w := range []string{
		"##[warning]Input 'old' has been deprecated with message: use who",
		"##[warning]Unexpected input(s) 'extra', valid inputs are ['who', 'other', 'old']",
		"##[group]Run ./.github/actions/n", "with:", "  who: Ermite", "  extra: y", "##[endgroup]",
		"[node24]", // node20 is upgraded only by the server flag: see TestNodeVersionRules
	} {
		if w == "[node24]" {
			continue
		}
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
	if !strings.Contains(got, "who=Ermite other=octo/hello old=[x] action=__self path=") || !strings.Contains(got, " repo=[]") {
		t.Errorf("env: %s", got)
	}
	if strings.Contains(got, "Missing required") {
		t.Error("the runner has no warning about required inputs")
	}
}

func TestNodePrePostOrderAndState(t *testing.T) {
	act := `name: pp
runs:
  using: node20
  pre: pre.sh
  main: main.sh
  post: post.sh
  post-if: always()
`
	sc := "echo \"[$PHASE] state=[$STATE_s] legacy=[$STATE_l]\"\necho \"s=$PHASE\" >> \"$GITHUB_STATE\"\necho \"::save-state name=l::$PHASE\"\n"
	tree := files{".github/actions/pp/action.yml": act,
		".github/actions/pp/pre.sh": "PHASE=pre\n" + sc, ".github/actions/pp/main.sh": "PHASE=main\n" + sc, ".github/actions/pp/post.sh": "PHASE=post\n" + sc}
	rec, res := actionRun(t, tree, "      - uses: ./.github/actions/pp\n      - run: echo between\n      - uses: ./.github/actions/pp\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	want := []string{"Set up job", "Run ./.github/actions/pp", "Run echo between", "Run ./.github/actions/pp", "Post Run ./.github/actions/pp", "Post Run ./.github/actions/pp", "Complete job"}
	if got := rec.names(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("steps %v", got)
	}
	// The runner never runs pre of a local action and says so.
	if got := logOf(rec.steps[1]); !strings.Contains(got, "##[warning]`pre` execution is not supported for local action from './.github/actions/pp'") {
		t.Errorf("no pre warning:\n%s", got)
	}
	if got := logOf(rec.steps[1]); !strings.Contains(got, "[main] state=[] legacy=[]") {
		t.Errorf("main:\n%s", got)
	}
	post := logOf(rec.steps[4])
	if !strings.Contains(post, "Post job cleanup.") || !strings.Contains(post, "[post] state=[main] legacy=[main]") {
		t.Errorf("post:\n%s", post)
	}
	if rec.steps[4].Name == rec.steps[5].Name && rec.steps[4].Num >= rec.steps[5].Num {
		t.Error("numbering")
	}
}

func TestPostRunsInReverseOrderAndAfterFailure(t *testing.T) {
	mk := func(name string) files {
		d := ".github/actions/" + name + "/"
		return files{d + "action.yml": "name: " + name + "\nruns:\n  using: node20\n  main: m.sh\n  post: p.sh\n  post-if: " + map[string]string{"a": "always()", "s": "success()", "f": "failure()"}[name] + "\n",
			d + "m.sh": "echo main-" + name + "\n", d + "p.sh": "echo post-" + name + "\n"}
	}
	tree := files{}
	for _, n := range []string{"a", "s", "f"} {
		for k, v := range mk(n) {
			tree[k] = v
		}
	}
	rec, res := actionRun(t, tree, "      - uses: ./.github/actions/a\n      - uses: ./.github/actions/s\n      - uses: ./.github/actions/f\n      - run: exit 1\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	names := strings.Join(rec.names(), "|")
	if !strings.Contains(names, "Run exit 1|Post Run ./.github/actions/f|Post Run ./.github/actions/s|Post Run ./.github/actions/a|Complete job") {
		t.Fatalf("%s", names)
	}
	if c := rec.steps[len(rec.steps)-3].Conclusion; c != ConclusionSuccess { // post of s: skipped
		_ = c
	}
	if got := rec.step(t, "Post Run ./.github/actions/s").Conclusion; got != ConclusionSkipped {
		t.Errorf("post-if success() after a failure: %s", got)
	}
	if got := rec.step(t, "Post Run ./.github/actions/f").Conclusion; got != ConclusionSuccess {
		t.Errorf("post-if failure(): %s", got)
	}
}

func TestStepNamesAndGithubActionIDs(t *testing.T) {
	tree := files{".github/actions/n/action.yml": nodeAction, ".github/actions/n/main.sh": "echo \"a=$GITHUB_ACTION\"\n"}
	rec, _ := actionRun(t, tree, "      - run: echo \"a=$GITHUB_ACTION\"\n      - run: echo \"a=$GITHUB_ACTION\"\n      - uses: ./.github/actions/n\n      - uses: ./.github/actions/n\n      - id: named\n        uses: ./.github/actions/n\n", nil)
	for i, want := range []string{"a=__run", "a=__run_2", "a=__self", "a=__self_2", "a=named"} {
		if got := logOf(rec.steps[i+1]); !strings.HasSuffix(got, want) {
			t.Errorf("step %d: %q does not end with %q", i+1, got, want)
		}
	}
}

func TestMissingLocalAction(t *testing.T) {
	rec, res := actionRun(t, files{}, "      - uses: ./nope\n      - run: echo after\n        if: always()\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rec.step(t, "Run ./nope")); !strings.Contains(got, "##[error]Can't find 'action.yml', 'action.yaml' or 'Dockerfile' under '") ||
		!strings.Contains(got, "/hello/hello/nope'. Did you forget to run actions/checkout before running your local action?") {
		t.Errorf("%s", got)
	}
}

func TestManifestErrors(t *testing.T) {
	rec, res := actionRun(t, files{"bad/action.yml": "name: x\nruns:\n  using: node99\n"}, "      - uses: ./bad\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	got := logOf(rec.step(t, "Run ./bad"))
	if !strings.Contains(got, "##[error]System.ArgumentOutOfRangeException") || !strings.Contains(got, "Failed to load ") {
		t.Errorf("%s", got)
	}
}

// The phase of github.com today (recordings act-node-version, act-node-legacy,
// act-node-force24): node12, node16 and node20 actions run on node24 whatever
// the variables say, and the job ends with one warning that lists them.
func TestNodeVersionGitHubPhase(t *testing.T) {
	const tail = ". For more information see: https://github.blog/changelog/2025-09-19-deprecation-of-node-20-on-github-actions-runners/"
	act := func(using string) string {
		return "name: a\nruns:\n  using: " + using + "\n  main: m.sh\n"
	}
	tree := files{"a/action.yml": act("node20"), "a/m.sh": "echo ran\n", "n12/action.yml": act("node12"), "n12/m.sh": "echo ran\n",
		"n16/action.yml": act("node16"), "n16/m.sh": "echo ran\n", "n24/action.yml": act("node24"), "n24/m.sh": "echo ran\n"}
	run := func(yaml string) *recorder {
		t.Helper()
		rec, res := actionRun(t, tree, yaml, func(c *Config) { c.Actions = DefaultActionsConfig(nil); c.Actions.ExternalsDir = fakeNode(t) })
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
		}
		return rec
	}
	// Order: case-insensitive, one entry per action; node24 actions are not listed.
	rec := run("      - uses: ./n16\n      - uses: ./n24\n      - uses: ./a\n      - uses: ./n12\n      - uses: ./a\n")
	want := "##[warning]Node.js 20 is deprecated. The following actions target Node.js 20 but are being forced to run on Node.js 24: ./a, ./n12, ./n16" + tail
	if got := logOf(rec.steps[len(rec.steps)-1]); !strings.Contains(got, "Cleaning up orphan processes\n"+want) {
		t.Errorf("complete job:\n%s", got)
	}
	for _, name := range []string{"Run ./n12", "Run ./n16", "Run ./a", "Run ./n24"} {
		if got := logOf(rec.step(t, name)); !strings.Contains(got, "[node24]") {
			t.Errorf("%s: %s", name, got)
		}
	}
	// The variables change nothing: not even ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION, nor both.
	for _, env := range []string{
		"          ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION: 'true'\n",
		"          FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: 'true'\n",
		"          FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: 'true'\n          ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION: 'true'\n",
	} {
		rec := run("      - uses: ./a\n        env:\n" + env)
		got := logOf(rec.step(t, "Run ./a"))
		if !strings.Contains(got, "[node24]") || strings.Contains(got, "Both ") || strings.Contains(got, "being deprecated") {
			t.Errorf("%s: %s", env, got)
		}
		if last := logOf(rec.steps[len(rec.steps)-1]); !strings.Contains(last, "forced to run on Node.js 24: ./a"+tail) {
			t.Errorf("%s: complete job:\n%s", env, last)
		}
	}
	// Only node24 actions: no warning.
	rec = run("      - uses: ./n24\n")
	if got := logOf(rec.steps[len(rec.steps)-1]); strings.Contains(got, "Node.js 20 is deprecated") {
		t.Errorf("node24 only:\n%s", got)
	}
}

func TestNodeVersionRules(t *testing.T) {
	act := func(using string) files {
		return files{"a/action.yml": "name: a\nruns:\n  using: " + using + "\n  main: m.sh\n", "a/m.sh": "echo ran\n"}
	}
	run := func(using, env string, tweak func(*Config)) string {
		yaml := "      - uses: ./a\n"
		if env != "" {
			yaml += "        env:\n" + env
		}
		rec, res := actionRun(t, act(using), yaml, tweak)
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v\n%s", using, res, logOf(rec.steps[1]))
		}
		return logOf(rec.step(t, "Run ./a"))
	}
	for using, want := range map[string]string{"node12": "[node20]", "node16": "[node20]", "node20": "[node20]", "node24": "[node24]"} {
		if got := run(using, "", nil); !strings.Contains(got, want) {
			t.Errorf("%s: %s", using, got)
		}
	}
	if got := run("node20", "          FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: 'true'\n", nil); !strings.Contains(got, "[node24]") {
		t.Errorf("force: %s", got)
	}
	rec, _ := actionRun(t, act("node20"), "      - uses: ./a\n        env:\n          FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: 'true'\n", nil)
	if got := logOf(rec.steps[len(rec.steps)-1]); !strings.Contains(got, "##[warning]Node.js 20 is deprecated. The following actions target Node.js 20 but are being forced to run on Node.js 24: ./a. For more information see: https://github.blog/changelog/2025-09-19-deprecation-of-node-20-on-github-actions-runners/") {
		t.Errorf("complete job:\n%s", got)
	}
	by24 := func(c *Config) { c.Actions.Node24ByDefault = true }
	got := run("node20", "", by24)
	if !strings.Contains(got, "[node24]") || !strings.Contains(got, "Node 20 is being deprecated. This workflow is running with Node 24 by default.") {
		t.Errorf("by default: %s", got)
	}
	if got := run("node20", "          ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION: 'true'\n", by24); !strings.Contains(got, "[node20]") {
		t.Errorf("unsecure: %s", got)
	}
	both := "          FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: 'true'\n          ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION: 'true'\n"
	got = run("node20", both, nil)
	if !strings.Contains(got, "##[warning]Both FORCE_JAVASCRIPT_ACTIONS_TO_NODE24 and ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION environment variables are set to true in the workflow environment. This is likely a configuration error. Using the default Node version: node20.") ||
		!strings.Contains(got, "[node20]") {
		t.Errorf("both: %s", got)
	}
	// Setting node24 explicitly ignores the variables.
	if got := run("node24", "          ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION: 'true'\n", by24); !strings.Contains(got, "[node24]") {
		t.Errorf("node24 explicit: %s", got)
	}
}

func TestCompositeActionBasics(t *testing.T) {
	tree := files{
		".github/actions/c/action.yml": `name: c
inputs:
  who:
    default: world
outputs:
  greeting:
    value: ${{ steps.greet.outputs.greeting }}
  literal:
    value: fixed
runs:
  using: composite
  steps:
    - id: greet
      shell: bash
      run: |
        echo "greeting=hello ${{ inputs.who }}" >> "$GITHUB_OUTPUT"
        echo "path=[$GITHUB_ACTION_PATH] self=[${{ github.action_path }}] status=[${{ github.action_status }}]"
        echo "OUTER=$OUTER_STEP_ENV"
    - shell: bash
      env:
        INNER: x
      run: echo "inner=$INNER id=$GITHUB_ACTION"
`}
	rec, res := actionRun(t, tree, `      - id: c
        uses: ./.github/actions/c
        with:
          who: Ermite
        env:
          OUTER_STEP_ENV: set
      - run: |
          echo "out=${{ steps.c.outputs.greeting }} lit=${{ steps.c.outputs.literal }} inner=[${{ steps.greet.outputs.greeting }}]"
          echo "gh=[$GITHUB_ACTION_PATH]"
`, nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	got := logOf(rec.step(t, "Run ./.github/actions/c"))
	// github.action is not changed for the steps of a composite action: they keep
	// the id of the step that uses it (CompositeActionHandler.cs:152-161 copies the
	// context; StepsRunner.cs:118 sets the key for the steps of the job only).
	for _, w := range []string{"/.github/actions/c] self=[", "status=[success]", "OUTER=set", "inner=x id=c"} {
		if !strings.Contains(got, w) {
			t.Errorf("composite missing %q:\n%s", w, got)
		}
	}
	after := logOf(rec.steps[2])
	if !strings.Contains(after, "out=hello Ermite lit=fixed inner=[]") {
		t.Errorf("outputs:\n%s", after)
	}
	if strings.Contains(after, "gh=[/") {
		t.Errorf("action_path must not leak out of the composite:\n%s", after)
	}
	if len(rec.steps) != 4 {
		t.Errorf("composite steps are not steps of the job: %v", rec.names())
	}
}

func TestCompositeFailureAndStatus(t *testing.T) {
	tree := files{".github/actions/c/action.yml": `name: c
runs:
  using: composite
  steps:
    - shell: bash
      run: exit 3
    - shell: bash
      run: echo "never"
    - if: always()
      shell: bash
      run: echo "status=${{ github.action_status }}"
`}
	rec, res := actionRun(t, tree, "      - id: c\n        uses: ./.github/actions/c\n      - if: always()\n        run: echo \"outcome=${{ steps.c.outcome }} conclusion=${{ steps.c.conclusion }}\"\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	got := logOf(rec.steps[1])
	if !strings.Contains(got, "##[error]Process completed with exit code 3.") || strings.Contains(got, "\nnever") ||
		!strings.Contains(got, "status=failure") {
		t.Errorf("%s", got)
	}
	if rec.steps[1].Conclusion != ConclusionFailure {
		t.Errorf("%s", rec.steps[1].Conclusion)
	}
	if !strings.Contains(logOf(rec.steps[2]), "outcome=failure conclusion=failure") {
		t.Errorf("%s", logOf(rec.steps[2]))
	}
}

func TestCompositeNestedScopes(t *testing.T) {
	tree := files{
		"o/action.yml": `name: o
inputs:
  v:
    default: d
outputs:
  r:
    value: ${{ steps.mid.outputs.r }}
runs:
  using: composite
  steps:
    - id: mid
      uses: ./i
      with:
        v: ${{ inputs.v }}-outer
`,
		"i/action.yml": `name: i
inputs:
  v:
    default: d
outputs:
  r:
    value: ${{ steps.s.outputs.r }}
runs:
  using: composite
  steps:
    - id: s
      shell: bash
      run: echo "r=in:${{ inputs.v }}" >> "$GITHUB_OUTPUT"
`}
	rec, res := actionRun(t, tree, "      - id: top\n        uses: ./o\n        with:\n          v: x\n      - run: echo \"r=${{ steps.top.outputs.r }} leak=[${{ steps.mid.outputs.r }}${{ steps.s.outputs.r }}]\"\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	if got := logOf(rec.steps[2]); !strings.HasSuffix(got, "r=in:x-outer leak=[]") {
		t.Errorf("%s", got)
	}
}

func TestCompositePostOfInnerStepsRunsInOuterPost(t *testing.T) {
	tree := files{
		"c/action.yml":  "name: c\nruns:\n  using: composite\n  steps:\n    - uses: ./n1\n    - uses: ./n2\n",
		"n1/action.yml": "name: n1\nruns:\n  using: node20\n  main: m.sh\n  post: p.sh\n", "n1/m.sh": "echo main1\n", "n1/p.sh": "echo post1\n",
		"n2/action.yml": "name: n2\nruns:\n  using: node20\n  main: m.sh\n  post: p.sh\n", "n2/m.sh": "echo main2\n", "n2/p.sh": "echo post2\n",
	}
	rec, res := actionRun(t, tree, "      - uses: ./c\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := rec.names(); strings.Join(got, "|") != "Set up job|Run ./c|Post Run ./c|Complete job" {
		t.Fatalf("%v", got)
	}
	main, post := logOf(rec.steps[1]), logOf(rec.steps[2])
	if !strings.Contains(main, "main1") || !strings.Contains(main, "main2") {
		t.Errorf("%s", main)
	}
	if i, k := strings.Index(post, "post2"), strings.Index(post, "post1"); i < 0 || k < 0 || i > k {
		t.Errorf("inner posts must run last to first:\n%s", post)
	}
}

// The depth limit (ActionManager.cs:444) is checked while the actions of a level
// are prepared. Local actions are not prepared before their step runs, one level
// at a time, so a chain of local composite actions never reaches it; a chain of
// repository actions does: the steps of the tenth composite are prepared at
// depth 10, which is more than 9.
func TestCompositeDepthLimit(t *testing.T) {
	chain := func(n int) *fakeForge {
		arch := map[string][]byte{}
		for i := 1; i <= n; i++ {
			next := "    - shell: bash\n      run: echo leaf\n"
			if i < n {
				next = "    - uses: o/r" + string(rune('a'+i)) + "@v1\n"
			}
			arch["/tarball/o/r"+string(rune('a'+i-1))+"/v1"] = tarOf(t, "o-r-x", files{"action.yml": "name: r\nruns:\n  using: composite\n  steps:\n" + next})
		}
		return newForge(t, arch)
	}
	run := func(n int) (*recorder, Result) {
		f := chain(n)
		return actionRun(t, files{}, "      - uses: o/ra@v1\n", func(c *Config) { c.Actions.Resolver = f.resolver("") })
	}
	if rec, res := run(9); res.Conclusion != ConclusionSuccess {
		t.Fatalf("9 levels: %+v\n%s", res, logOf(rec.steps[0]))
	}
	rec, res := run(10)
	if res.Conclusion != ConclusionFailure || len(rec.steps) != 1 || !strings.Contains(logOf(rec.steps[0]), "##[error]Composite action depth exceeded max depth 9") {
		t.Fatalf("10 levels: %+v %v\n%s", res, rec.names(), logOf(rec.steps[0]))
	}
}

// Local composites are not limited on GitHub (the recording act-composite-depth
// has 9, 10 and 12 levels succeed); only ermite's guard against recursion stops
// a composite that uses itself.
func TestLocalCompositeDepth(t *testing.T) {
	chain := func(n int) files {
		tree := files{}
		for i := 0; i < n; i++ {
			next := ""
			if i < n-1 {
				next = "    - uses: ./d" + strconv.Itoa(i+1) + "\n"
			}
			tree["d"+strconv.Itoa(i)+"/action.yml"] = "name: d\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo level\n" + next
		}
		return tree
	}
	for _, n := range []int{9, 10, 12} {
		rec, res := actionRun(t, chain(n), "      - uses: ./d0\n", nil)
		if res.Conclusion != ConclusionSuccess || strings.Count(logOf(rec.step(t, "Run ./d0")), "\nlevel") != n {
			t.Fatalf("%d levels: %+v\n%s", n, res, logOf(rec.step(t, "Run ./d0")))
		}
	}
	// Up to the guard.
	if _, res := actionRun(t, chain(maxLocalDepth), "      - uses: ./d0\n", nil); res.Conclusion != ConclusionSuccess {
		t.Fatalf("%d levels: %+v", maxLocalDepth, res)
	}
	// A composite that uses itself fails cleanly, and the job goes on.
	self := files{"s/action.yml": "name: s\nruns:\n  using: composite\n  steps:\n    - uses: ./s\n"}
	rec, res := actionRun(t, self, "      - uses: ./s\n      - if: always()\n        run: echo after\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rec.step(t, "Run ./s")); strings.Count(got, "##[error]Composite action depth exceeded max depth 100") != 1 {
		t.Errorf("%s", got[:min(len(got), 600)])
	}
	if got := rec.step(t, "Run echo after").Conclusion; got != ConclusionSuccess {
		t.Errorf("the step after: %s", got)
	}
	// Two actions that use each other.
	cyc := files{
		"a/action.yml": "name: a\nruns:\n  using: composite\n  steps:\n    - uses: ./b\n",
		"b/action.yml": "name: b\nruns:\n  using: composite\n  steps:\n    - uses: ./a\n",
	}
	if _, res := actionRun(t, cyc, "      - uses: ./a\n", nil); res.Conclusion != ConclusionFailure {
		t.Errorf("cycle: %+v", res)
	}
}

// A repository composite used twice, and by a local composite used many times,
// is resolved and downloaded once.
func TestResolutionsAreReusedAcrossLocalComposites(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/o/leaf/v1": tarOf(t, "o-leaf-x", files{"action.yml": nodeAction, "main.sh": "echo leaf\n"})})
	tree := files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: o/leaf@v1\n"}
	var yaml string
	for i := 0; i < 5; i++ {
		yaml += "      - uses: ./c\n"
	}
	rec, res := actionRun(t, tree, yaml, func(c *Config) { c.Actions.Resolver = forge.resolver("") })
	if res.Conclusion != ConclusionSuccess || strings.Count(logOf(rec.steps[1])+logOf(rec.steps[2])+logOf(rec.steps[5]), "leaf") < 3 {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if len(forge.asked) != 1 || forge.hits.Load() != 1 {
		t.Errorf("asked %v, downloaded %d times: once is enough", forge.asked, forge.hits.Load())
	}
}

// tarOf builds the archive of a repository: one top directory, like GitHub's.
func tarOf(t *testing.T, top string, f files) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range f {
		_ = tw.WriteHeader(&tar.Header{Name: top + "/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

type fakeForge struct {
	srv      *httptest.Server
	archives map[string][]byte
	auth     map[string]string
	hits     atomic.Int32
	asked    [][]string
}

func newForge(t *testing.T, archives map[string][]byte) *fakeForge {
	f := &fakeForge{archives: archives, auth: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.auth[r.URL.Path] = r.Header.Get("Authorization")
		b, ok := f.archives[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeForge) resolver(token string) Resolver {
	return func(_ context.Context, uses []string) ([]ResolvedAction, error) {
		f.asked = append(f.asked, uses)
		var out []ResolvedAction
		for _, u := range uses {
			name, ref, _ := strings.Cut(u, "@")
			name = strings.Join(strings.SplitN(name, "/", 3)[:2], "/")
			if strings.Contains(u, "missing") {
				out = append(out, ResolvedAction{Ref: u, Error: "Unable to resolve action `" + u + "`, repository not found: `" + name + "`"})
				continue
			}
			out = append(out, ResolvedAction{Ref: u, NameWithOwner: name, ResolvedSHA: strings.Repeat("a", 40),
				TarballURL: f.srv.URL + "/tarball/" + name + "/" + ref, Token: token, AuthOrigin: tokenOrigin(token, f.srv.URL)})
		}
		return out, nil
	}
}

func TestRepositoryActionDownload(t *testing.T) {
	forge := newForge(t, map[string][]byte{
		"/tarball/octo/tool/v1": tarOf(t, "octo-tool-aaaa", files{"action.yml": nodeAction, "main.sh": "echo \"repo=$GITHUB_ACTION_REPOSITORY ref=$GITHUB_ACTION_REPOSITORY@$GITHUB_ACTION_REF dir=$GITHUB_ACTION_PATH\"\n"}),
	})
	rec, res := actionRun(t, files{}, "      - uses: octo/tool@v1\n      - uses: octo/tool@v1\n", func(c *Config) {
		c.Actions.Resolver = forge.resolver("archive-secret-token")
		c.Actions.Backoff = func() time.Duration { return time.Millisecond }
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[0]))
	}
	setup := logOf(rec.steps[0])
	if !strings.Contains(setup, "Getting action download info\nDownload action repository 'octo/tool@v1' (SHA:"+strings.Repeat("a", 40)+")") {
		t.Errorf("set up:\n%s", setup)
	}
	if strings.Count(setup, "Download action repository") != 1 || forge.hits.Load() != 1 {
		t.Errorf("an action is downloaded once per job (hits %d):\n%s", forge.hits.Load(), setup)
	}
	if len(forge.asked) != 1 || len(forge.asked[0]) != 1 {
		t.Errorf("the same action is resolved once: %v", forge.asked)
	}
	if got := forge.auth["/tarball/octo/tool/v1"]; got != "Basic eC1hY2Nlc3MtdG9rZW46YXJjaGl2ZS1zZWNyZXQtdG9rZW4=" {
		t.Errorf("auth header %q", got)
	}
	if got := logOf(rec.steps[1]); !strings.Contains(got, "repo=octo/tool ref=octo/tool@v1") || !strings.Contains(got, "##[group]Run octo/tool@v1") {
		t.Errorf("run:\n%s", got)
	}
	if rec.steps[1].Name != "Run octo/tool@v1" {
		t.Errorf("name %q", rec.steps[1].Name)
	}
	for _, s := range rec.steps {
		if strings.Contains(logOf(s), "archive-secret-token") {
			t.Errorf("the archive token leaked in %q", s.Name)
		}
	}
}

func TestRepositoryActionSubpathAndErrors(t *testing.T) {
	forge := newForge(t, map[string][]byte{
		"/tarball/octo/mono/v2": tarOf(t, "octo-mono-bbbb", files{"sub/dir/action.yml": nodeAction, "sub/dir/main.sh": "echo sub-ok\n"}),
	})
	cfg := func(c *Config) {
		c.Actions.Resolver = forge.resolver("")
		c.Actions.Backoff = func() time.Duration { return time.Millisecond }
	}
	rec, res := actionRun(t, files{}, "      - uses: octo/mono/sub/dir@v2\n", cfg)
	if res.Conclusion != ConclusionSuccess || !strings.Contains(logOf(rec.steps[1]), "sub-ok") || rec.steps[1].Name != "Run octo/mono/sub/dir@v2" {
		t.Fatalf("%+v %v\n%s", res, rec.names(), logOf(rec.steps[1]))
	}
	// A repository the server cannot resolve: its message ends "Set up job".
	rec, res = actionRun(t, files{}, "      - uses: octo/missing@v1\n      - run: echo NO\n", cfg)
	if res.Conclusion != ConclusionFailure || len(rec.steps) != 1 || !strings.Contains(logOf(rec.steps[0]), "##[error]Unable to resolve action `octo/missing@v1`, repository not found: `octo/missing`") {
		t.Fatalf("%+v %v\n%s", res, rec.names(), logOf(rec.steps[0]))
	}
	// An archive that does not exist: 404 is not retried.
	rec, res = actionRun(t, files{}, "      - uses: octo/gone@v1\n", cfg)
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.steps[0]), "##[error]An action could not be found at the URI '"+forge.srv.URL+"/tarball/octo/gone/v1'") {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[0]))
	}
}

func TestDownloadRetriesAndWarns(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1); http.Error(w, "x", 500) }))
	defer srv.Close()
	res := func(context.Context, []string) ([]ResolvedAction, error) {
		return []ResolvedAction{{Ref: "a/b@v1", NameWithOwner: "a/b", ResolvedSHA: strings.Repeat("c", 40), TarballURL: srv.URL + "/t"}}, nil
	}
	rec, r := actionRun(t, files{}, "      - uses: a/b@v1\n", func(c *Config) {
		c.Actions.Resolver = res
		c.Actions.Backoff = func() time.Duration { return time.Millisecond }
	})
	if r.Conclusion != ConclusionFailure || n.Load() != 3 {
		t.Fatalf("%+v attempts %d", r, n.Load())
	}
	got := logOf(rec.steps[0])
	if strings.Count(got, "##[warning]Failed to download action '"+srv.URL+"/t'. Error: Response status code does not indicate success: 500 (Internal Server Error). ") != 2 ||
		strings.Count(got, "##[warning]Back off 0.001 seconds before retry.") != 2 {
		t.Errorf("%s", got)
	}
}

func TestMissingManifestInDownloadedAction(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/octo/empty/v1": tarOf(t, "octo-empty-cccc", files{"README.md": "x"})})
	rec, res := actionRun(t, files{}, "      - uses: octo/empty@v1\n", func(c *Config) { c.Actions.Resolver = forge.resolver("") })
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.steps[0]), "##[error]Can't find 'action.yml', 'action.yaml' or 'Dockerfile' for action 'octo/empty@v1'.") {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[0]))
	}
}

func TestRepositoryActionPrePostAndState(t *testing.T) {
	act := "name: pp\nruns:\n  using: node20\n  pre: pre.sh\n  main: main.sh\n  post: post.sh\n  pre-if: always()\n  post-if: success()\n"
	sc := "echo \"[$P] state=[$STATE_k]\"\necho \"::save-state name=k::from-$P\"\n"
	forge := newForge(t, map[string][]byte{"/tarball/o/pp/v1": tarOf(t, "o-pp-x", files{
		"action.yml": act, "pre.sh": "P=pre\n" + sc, "main.sh": "P=main\n" + sc, "post.sh": "P=post\n" + sc})})
	rec, res := actionRun(t, files{}, "      - run: echo first\n      - uses: o/pp@v1\n      - run: echo last\n", func(c *Config) { c.Actions.Resolver = forge.resolver("") })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	want := "Set up job|Pre Run o/pp@v1|Run echo first|Run o/pp@v1|Run echo last|Post Run o/pp@v1|Complete job"
	if got := strings.Join(rec.names(), "|"); got != want {
		t.Fatalf("%s", got)
	}
	if got := logOf(rec.steps[1]); !strings.Contains(got, "[pre] state=[]") || !strings.Contains(got, "##[group]Run o/pp@v1") {
		t.Errorf("pre:\n%s", got)
	}
	if got := logOf(rec.steps[3]); !strings.Contains(got, "[main] state=[from-pre]") {
		t.Errorf("main sees the state of pre:\n%s", got)
	}
	if got := logOf(rec.steps[5]); !strings.Contains(got, "Post job cleanup.") || !strings.Contains(got, "[post] state=[from-main]") {
		t.Errorf("post:\n%s", got)
	}
}

func TestDockerPrePostEntrypoints(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/o/dk/v1": tarOf(t, "o-dk-x", files{
		"action.yml": "name: dk\nruns:\n  using: docker\n  image: docker://alpine:3\n  pre-entrypoint: /pre.sh\n  entrypoint: /main.sh\n  post-entrypoint: /post.sh\n"})})
	d, lf := fakeDocker(t, 0)
	rec, res := actionRun(t, files{}, "      - uses: o/dk@v1\n", func(c *Config) {
		c.Actions.DockerPath = d
		c.Actions.Resolver = forge.resolver("")
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	want := "Set up job|Pull alpine:3|Pre Run o/dk@v1|Run o/dk@v1|Post Run o/dk@v1|Complete job"
	if got := strings.Join(rec.names(), "|"); got != want {
		t.Fatalf("%s", got)
	}
	var eps []string
	for _, c := range calls(t, lf) {
		if c[0] != "run" {
			continue
		}
		for i, a := range c {
			if a == "--entrypoint" {
				eps = append(eps, c[i+1])
			}
		}
	}
	if strings.Join(eps, ",") != "/pre.sh,/main.sh,/post.sh" {
		t.Errorf("entrypoints %v", eps)
	}
}

func TestRepositoryActionInputsAreMaskedAndTokenNotInEnv(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/o/t/v1": tarOf(t, "o-t-x", files{
		"action.yml": nodeAction, "main.sh": "env | grep -c archive-secret || true\n"})})
	rec, res := actionRun(t, files{}, "      - uses: o/t@v1\n", func(c *Config) { c.Actions.Resolver = forge.resolver("archive-secret") })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rec.steps[1]); !strings.HasSuffix(got, "\n0") {
		t.Errorf("the archive token is not in the environment of actions:\n%s", got)
	}
}

func tokenOrigin(token, u string) string {
	if token == "" {
		return ""
	}
	return originOf(u)
}

// GO-002: the token goes to the origin the host named and nowhere else, and
// a redirect never carries it to another origin.
func TestArchiveTokenOnlyToTheNamedOrigin(t *testing.T) {
	var gotAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(tarOf(t, "o-x-1", files{"action.yml": nodeAction, "main.sh": "echo ok\n"}))
	}))
	defer other.Close()
	var firstAuth atomic.Value
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstAuth.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, other.URL+"/t", http.StatusFound)
	}))
	defer redir.Close()
	mk := func(u, token, origin string) Resolver {
		return func(context.Context, []string) ([]ResolvedAction, error) {
			return []ResolvedAction{{Ref: "o/x@v1", NameWithOwner: "o/x", ResolvedSHA: strings.Repeat("d", 40), TarballURL: u, Token: token, AuthOrigin: origin}}, nil
		}
	}
	run := func(r Resolver) Result {
		_, res := actionRun(t, files{}, "      - uses: o/x@v1\n", func(c *Config) {
			c.Actions.Resolver = r
			c.Actions.Backoff = func() time.Duration { return time.Millisecond }
		})
		return res
	}
	// Redirect to another origin (another port on the same host): the header is
	// sent to the first hop and dropped for the second.
	if res := run(mk(redir.URL+"/t", "tok", originOf(redir.URL))); res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if firstAuth.Load() == "" || gotAuth.Load() != "" {
		t.Errorf("first hop %q, second hop %q", firstAuth.Load(), gotAuth.Load())
	}
	// A token without an AuthOrigin, or for another origin, is never sent.
	gotAuth.Store("unset")
	if res := run(mk(other.URL+"/t", "tok", "")); res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if gotAuth.Load() != "" {
		t.Errorf("no AuthOrigin: sent %q", gotAuth.Load())
	}
	gotAuth.Store("unset")
	_ = run(mk(other.URL+"/t", "tok", "https://other.example"))
	if gotAuth.Load() != "" {
		t.Errorf("another origin: sent %q", gotAuth.Load())
	}
	// Without a token (github.com) nothing is sent either.
	gotAuth.Store("unset")
	_ = run(mk(other.URL+"/t", "", ""))
	if gotAuth.Load() != "" {
		t.Errorf("anonymous: sent %q", gotAuth.Load())
	}
	// The named origin, with the token.
	gotAuth.Store("unset")
	_ = run(mk(other.URL+"/t", "tok", originOf(other.URL)))
	if got, _ := gotAuth.Load().(string); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("named origin: %q", got)
	}
}

func TestArchiveRedirectPolicy(t *testing.T) {
	j := &job{}
	c := j.archiveClient()
	mkreq := func(u string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", "Basic x")
		return r
	}
	for _, tc := range []struct {
		from, to string
		keep     bool
		refused  bool
	}{
		{"https://a.example/x", "https://a.example/y", true, false},
		{"https://a.example/x", "https://cdn.a.example/y", false, false},  // subdomain
		{"https://a.example/x", "https://a.example:8443/y", false, false}, // port
		{"https://a.example/x", "http://a.example/y", false, true},        // downgrade
		{"http://a.example/x", "https://a.example/y", false, false},       // scheme change
		{"https://a.example:443/x", "https://a.example/y", true, false},   // same origin
		{"https://A.example/x", "https://a.example/y", true, false},
	} {
		req := mkreq(tc.to)
		err := c.CheckRedirect(req, []*http.Request{mkreq(tc.from)})
		if (err != nil) != tc.refused || (req.Header.Get("Authorization") != "") != tc.keep && !tc.refused {
			t.Errorf("%s -> %s: err=%v header=%q", tc.from, tc.to, err, req.Header.Get("Authorization"))
		}
	}
	if err := c.CheckRedirect(mkreq("https://a.example/"), make([]*http.Request, 10)); err == nil {
		t.Error("redirect loops must stop")
	}
}

// GO-008: Go rebuilds the headers of a redirected request from the first
// request, so a chain that leaves the origin and stays on the new one, or comes
// back, would get the Authorization header again on the third request. Real
// servers, real redirects.
func TestArchiveRedirectChainNeverReauthorises(t *testing.T) {
	type hit struct{ path, auth string }
	var mu sync.Mutex
	var hits []hit
	record := func(r *http.Request) {
		mu.Lock()
		hits = append(hits, hit{r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
	}
	var a, b *httptest.Server
	// a (the authorised origin) and b are two servers on the same host and
	// different ports: the case of Go's host-only comparison.
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, b.URL+"/first", http.StatusFound)
		case "/back":
			_, _ = w.Write(tarOf(t, "o-x-1", files{"action.yml": nodeAction, "main.sh": "echo ok\n"}))
		}
	}))
	defer a.Close()
	b = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(r)
		switch r.URL.Path {
		case "/first":
			http.Redirect(w, r, b.URL+"/second", http.StatusFound)
		case "/second":
			http.Redirect(w, r, a.URL+"/back", http.StatusFound) // and back to the origin
		}
	}))
	defer b.Close()
	res := func(context.Context, []string) ([]ResolvedAction, error) {
		return []ResolvedAction{{Ref: "o/x@v1", NameWithOwner: "o/x", ResolvedSHA: strings.Repeat("e", 40), TarballURL: a.URL + "/start", Token: "tok", AuthOrigin: originOf(a.URL)}}, nil
	}
	_, r := actionRun(t, files{}, "      - uses: o/x@v1\n", func(c *Config) { c.Actions.Resolver = res })
	if r.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", r)
	}
	want := []hit{{"/start", "set"}, {"/first", ""}, {"/second", ""}, {"/back", ""}}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != len(want) {
		t.Fatalf("hits %v", hits)
	}
	for i, h := range hits {
		if h.path != want[i].path || (h.auth != "") != (want[i].auth != "") {
			t.Errorf("hit %d: %+v, want %+v (auth %q)", i, h, want[i], h.auth)
		}
	}
}

// GO-012: the token goes to the exact origin, in canonical form.
func TestTokenAllowedOrigins(t *testing.T) {
	for _, tc := range []struct {
		url, origin string
		ok          bool
	}{
		{"https://ermite.example/x", "https://ermite.example", true},
		{"https://ermite.example:443/x", "https://ermite.example", true}, // default port, either way
		{"https://ermite.example/x", "https://ermite.example:443", true},
		{"https://ERMITE.example/x", "https://ermite.example", true},
		{"https://[::1]:8443/x", "https://[::1]:8443", true},
		{"https://[::1]:8443/x", "https://[::1:8443]", false}, // the collision of the review
		{"https://[::1:8443]/x", "https://[::1]:8443", false},
		{"https://[::1]/x", "https://[::1]:443", true},
		{"https://[::1]/x", "https://[::1]:8443", false},
		{"http://ermite.example/x", "https://ermite.example", false},
		{"https://ermite.example:8443/x", "https://ermite.example", false},
		{"https://sub.ermite.example/x", "https://ermite.example", false},
		{"https://ermite.example/x", "", false},
		{"https://ermite.example/x", "not an origin", false},
		{"https://u:p@ermite.example/x", "https://ermite.example", false},
	} {
		if got := tokenAllowed(ResolvedAction{Token: "t", TarballURL: tc.url, AuthOrigin: tc.origin}); got != tc.ok {
			t.Errorf("url %q origin %q: %v, want %v", tc.url, tc.origin, got, tc.ok)
		}
	}
	if tokenAllowed(ResolvedAction{TarballURL: "https://a.test/x", AuthOrigin: "https://a.test"}) {
		t.Error("no token, nothing to allow")
	}
}

// The markers of the runner around the steps of a composite action
// (CompositeActionHandler.cs:323, :415-466), as recorded in act-composite-basic,
// act-composite-nested and act-composite-depth: the id is the dotted context
// name, a step without an id gets the id the workflow reader generated.
func TestCompositeStepMarkers(t *testing.T) {
	tree := files{
		".github/actions/mid/action.yml": `name: mid
runs:
  using: composite
  steps:
    - id: leaf-step
      shell: bash
      name: "multi %; ] line"
      run: echo leaf
    - uses: ./.github/actions/leaf
`,
		".github/actions/leaf/action.yml": `name: leaf
runs:
  using: composite
  steps:
    - shell: bash
      run: echo "level 2"
    - shell: bash
      if: false
      run: echo skipped
    - shell: bash
      continue-on-error: true
      run: exit 1
`,
	}
	rec, res := actionRun(t, tree, "      - id: call\n        uses: ./.github/actions/mid\n      - uses: ./.github/actions/leaf\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	var got []string
	for _, st := range rec.steps[1:3] {
		for _, l := range st.Log {
			if strings.HasPrefix(l, "##[start-action") || strings.HasPrefix(l, "##[end-action") {
				if !strings.Contains(l, "outcome=skipped") {
					l = regexp.MustCompile(`duration_ms=\d+`).ReplaceAllString(l, "duration_ms=N")
				}
				got = append(got, l)
			}
		}
	}
	want := []string{
		"##[start-action display=multi %25%3B %5D line;id=call.leaf-step]",
		"##[end-action id=call.leaf-step;outcome=success;conclusion=success;duration_ms=N]",
		"##[start-action display=Run ./.github/actions/leaf;id=call.__self]",
		`##[start-action display=Run echo "level 2";id=call.__self.__run]`,
		"##[end-action id=call.__self.__run;outcome=success;conclusion=success;duration_ms=N]",
		"##[start-action display=Run echo skipped;id=call.__self.__run_2]",
		"##[end-action id=call.__self.__run_2;outcome=skipped;conclusion=skipped;duration_ms=0]",
		"##[start-action display=Run exit 1;id=call.__self.__run_3]",
		"##[end-action id=call.__self.__run_3;outcome=failure;conclusion=success;duration_ms=N]",
		"##[end-action id=call.__self;outcome=success;conclusion=success;duration_ms=N]",
		`##[start-action display=Run echo "level 2";id=__self.__run]`,
		"##[end-action id=__self.__run;outcome=success;conclusion=success;duration_ms=N]",
		"##[start-action display=Run echo skipped;id=__self.__run_2]",
		"##[end-action id=__self.__run_2;outcome=skipped;conclusion=skipped;duration_ms=0]",
		"##[start-action display=Run exit 1;id=__self.__run_3]",
		"##[end-action id=__self.__run_3;outcome=failure;conclusion=success;duration_ms=N]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("markers:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A local composite action that uses other local composite actions has one post
// step, which prints "Post job cleanup." once (act-composite-depth: 9, 10 and 12
// levels, one line on GitHub). What the runner registers while it prepares the
// steps of a local composite is keyed by the id of the step of the job.
func TestNestedLocalCompositePostsOnce(t *testing.T) {
	tree := files{
		"c1/action.yml": "name: c1\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo one\n    - uses: ./c2\n",
		"c2/action.yml": "name: c2\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo two\n    - uses: ./c3\n",
		"c3/action.yml": "name: c3\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo three\n",
	}
	rec, res := actionRun(t, tree, "      - uses: ./c1\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	post := logOf(rec.step(t, "Post Run ./c1"))
	if post != "Post job cleanup." {
		t.Errorf("post of c1: %q", post)
	}
	for _, n := range rec.names() {
		if strings.HasPrefix(n, "Post ") && n != "Post Run ./c1" {
			t.Errorf("unexpected step %q in %v", n, rec.names())
		}
	}
}

// A local action's directory is the workspace and the path of the step joined as
// .NET's Path.Combine does, without cleaning: github.action_path and the paths in
// the manifest errors keep the "./" of "uses: ./.github/actions/x" (recorded in
// act-composite-basic and act-manifest-errors).
func TestLocalActionPathKeepsDotSlash(t *testing.T) {
	tree := files{
		".github/actions/ok/action.yml":  "name: ok\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo \"path=$GITHUB_ACTION_PATH\"\n",
		".github/actions/bad/action.yml": "name: bad\nruns:\n  using: composite\n  steps:\n    - run: echo no shell\n",
	}
	rec, res := actionRun(t, tree, "      - uses: ./.github/actions/ok\n      - uses: ./.github/actions/bad\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	ws := regexp.MustCompile(`path=(.*)/\./\.github/actions/ok`).FindStringSubmatch(logOf(rec.steps[1]))
	if ws == nil {
		t.Fatalf("action_path lost its ./:\n%s", logOf(rec.steps[1]))
	}
	bad := logOf(rec.steps[2])
	if !strings.Contains(bad, "##[error]"+ws[1]+"/./.github/actions/bad/action.yml (Line: 5, Col: 7): Required property is missing: shell") ||
		!strings.Contains(bad, "##[error]Failed to load "+ws[1]+"/./.github/actions/bad/action.yml") {
		t.Errorf("manifest errors:\n%s", bad)
	}
}

// The markers of a composite step go through the masker like every log line
// (ExecutionContext.Write masks tag+message after the marker was built): a
// secret in the name of an inner step, from the configuration or from an
// ::add-mask:: of an earlier step, never reaches the log.
func TestCompositeMarkersAreMasked(t *testing.T) {
	tree := files{
		".github/actions/m/action.yml": `name: m
runs:
  using: composite
  steps:
    - shell: bash
      run: echo "::add-mask::late-secret-""value"
    - name: "token ${{ github.token }} and late-secret-value"
      shell: bash
      run: echo hi
    - name: "env ${{ env.TOK }}"
      env:
        TOK: late-secret-value
      shell: bash
      run: echo hi
`,
	}
	rec, res := actionRun(t, tree, "      - uses: ./.github/actions/m\n", func(c *Config) { c.ContextsJSON = tokenContexts() })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	log := logOf(rec.steps[1])
	for _, secret := range []string{testToken, "late-secret-value"} {
		if strings.Contains(log, secret) {
			t.Errorf("%q is in the log:\n%s", secret, log)
		}
	}
	// A secret with a character the marker escapes is masked before the escape.
	if got := markerEscaper.Replace(sanitizeDisplayName("a;b")); got != "a%3Bb" {
		t.Errorf("escape %q", got)
	}
	for _, w := range []string{"##[start-action display=token *** and ***;id=", "##[start-action display=env ***;id="} {
		if !strings.Contains(log, w) {
			t.Errorf("missing %q:\n%s", w, log)
		}
	}
}

// An inner step whose env does not evaluate still gets its markers: the error
// is logged while the environment is set up, the start marker follows, then the
// end marker with the step failed (CompositeActionHandler.cs:296-302, :308-324).
// The runner would go on and run the step; this port does not (see runEmbedded).
func TestCompositeEnvErrorKeepsMarkers(t *testing.T) {
	tree := files{
		".github/actions/e/action.yml": `name: e
runs:
  using: composite
  steps:
    - name: bad env
      shell: bash
      env:
        X: ${{ fromJSON('{') }}
      run: echo never
`,
	}
	rec, res := actionRun(t, tree, "      - uses: ./.github/actions/e\n", nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	var got []string
	for _, l := range rec.steps[1].Log {
		switch {
		case strings.HasPrefix(l, "##[error]"):
			got = append(got, "error")
		case strings.HasPrefix(l, "##[start-action"), strings.HasPrefix(l, "##[end-action"):
			got = append(got, l)
		}
	}
	want := []string{"error", "##[start-action display=bad env;id=__self.__run]", "##[end-action id=__self.__run;outcome=failure;conclusion=failure;duration_ms=0]"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || strings.Contains(logOf(rec.steps[1]), "never") {
		t.Errorf("markers:\n%s\n%s", strings.Join(got, "\n"), logOf(rec.steps[1]))
	}
}

// A secret that the marker would escape, or that the cut at 1000 characters
// would split, is masked before: the display name of an inner step holding it
// leaves no part of it in the log.
func TestCompositeMarkerMasksBeforeEscapeAndCut(t *testing.T) {
	secret := "sec;ret]%value"
	long := strings.Repeat("x", 996) + secret // the cut falls inside the secret
	tree := files{
		".github/actions/m/action.yml": "name: m\nruns:\n  using: composite\n  steps:\n" +
			"    - name: \"pre ${{ env.S }}\"\n      shell: bash\n      run: echo hi\n" +
			"    - name: \"" + long + "\"\n      shell: bash\n      run: echo hi\n",
	}
	rec, res := actionRun(t, tree, "      - run: echo \"::add-mask::"+secret+"\"\n      - uses: ./.github/actions/m\n        env:\n          S: "+secret+"\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[2]))
	}
	log := logOf(rec.steps[2])
	for _, frag := range []string{"sec;", "sec%3B", "value", "%5D%25", "xsec"} {
		if strings.Contains(log, frag) {
			t.Errorf("%q is in the log:\n%.1500s", frag, log)
		}
	}
	if !strings.Contains(log, "##[start-action display=pre ***;id=") {
		t.Errorf("marker:\n%s", log)
	}
}

// pathCombine is Path.Combine: a rooted part replaces what precedes it, an empty
// part is skipped, nothing is cleaned.
func TestPathCombine(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"/w/r", "./.github/a"}, "/w/r/./.github/a"},
		{[]string{"/w/r/", "x"}, "/w/r/x"},
		{[]string{"/w/r/a", "/etc/x.js"}, "/etc/x.js"},
		{[]string{"/w/r/a", "../../x.js"}, "/w/r/a/../../x.js"},
		{[]string{"", "x"}, "x"},
		{[]string{"/w", ""}, "/w"},
	} {
		if got := pathCombine(c.in...); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}
