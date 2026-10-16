// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeDocker is a docker CLI that appends its arguments, one per line between
// "--" markers, and the values of the variables it was told to look at, to a
// file. pull and build succeed, "run" prints what it was given.
func fakeDocker(t *testing.T, failPulls int) (path, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls")
	counter := filepath.Join(dir, "pulls")
	script := `#!/bin/sh
{ echo "--"; for a in "$@"; do printf "%s\n" "$a"; done; } >> ` + logFile + `
case "$1" in
pull)
  n=$(cat ` + counter + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + counter + `
  if [ "$n" -le ` + string(rune('0'+failPulls)) + ` ]; then echo "pull failed"; exit 1; fi
  echo "pulled $2" ;;
build) echo "built" ;;
run)
  echo "RUN-ENV INPUT_WHO=$INPUT_WHO HOME=$HOME GITHUB_ENV=$GITHUB_ENV GITHUB_WORKSPACE=$GITHUB_WORKSPACE FOO=$FOO"
  echo "RUN-RT RUNTIME=${ACTIONS_RUNTIME_URL-unset} CACHE=${ACTIONS_CACHE_URL-unset} RESULTS=${ACTIONS_RESULTS_URL-unset} SERVER=$GITHUB_SERVER_URL"
  echo "::set-output name=x::y" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "docker"), logFile
}

func calls(t *testing.T, logFile string) [][]string {
	t.Helper()
	b, _ := os.ReadFile(logFile)
	var out [][]string
	for _, blk := range strings.Split(string(b), "--\n") {
		if blk = strings.TrimRight(blk, "\n"); blk != "" {
			out = append(out, strings.Split(blk, "\n"))
		}
	}
	return out
}

func dockerRun(t *testing.T, tree files, yaml string, failPulls int) (*recorder, Result, [][]string, *Layout) {
	t.Helper()
	d, lf := fakeDocker(t, failPulls)
	var lay *Layout
	rec, res := actionRun(t, tree, yaml, func(c *Config) {
		c.Actions.DockerPath = d
		c.Actions.Backoff = func() time.Duration { return time.Millisecond }
		lay = c.Layout
	})
	return rec, res, calls(t, lf), lay
}

func TestDockerHubImageAction(t *testing.T) {
	rec, res, cs, lay := dockerRun(t, files{}, "      - uses: docker://alpine:3.20\n        with:\n          entryPoint: /bin/sh\n          args: -c \"echo hi there\"\n        env:\n          FOO: bar\n", 0)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	want := []string{"Set up job", "Pull alpine:3.20", "Run alpine:3.20", "Complete job"}
	if got := rec.names(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("steps %v", got)
	}
	pull := logOf(rec.steps[1])
	if !strings.HasPrefix(pull, "##[group]Pull down action image 'alpine:3.20'\n##[command]") || !strings.Contains(pull, " pull alpine:3.20\npulled alpine:3.20\n##[endgroup]") {
		t.Errorf("pull:\n%s", pull)
	}
	run := logOf(rec.steps[2])
	if !strings.Contains(run, "##[group]Run docker://alpine:3.20") || !strings.Contains(run, "  entryPoint: /bin/sh") {
		t.Errorf("run:\n%s", run)
	}
	if len(cs) != 2 {
		t.Fatalf("calls %v", cs)
	}
	r := cs[1]
	has := func(a ...string) bool {
		for i := 0; i+len(a) <= len(r); i++ {
			if strings.Join(r[i:i+len(a)], "\x00") == strings.Join(a, "\x00") {
				return true
			}
		}
		return false
	}
	ws, tmp := filepath.Join(lay.Workspace), lay.Temp
	for _, w := range [][]string{
		{"--workdir", "/github/workspace"}, {"--rm"}, {"-e", "INPUT_ENTRYPOINT"}, {"-e", "FOO"}, {"-e", "GITHUB_ACTIONS=true"}, {"-e", "CI=true"},
		{"--entrypoint", "/bin/sh"}, {"-v", "/var/run/docker.sock:/var/run/docker.sock"}, {"-v", tmp + ":/github/runner_temp"},
		{"-v", tmp + "/_github_home:/github/home"}, {"-v", tmp + "/_github_workflow:/github/workflow"},
		{"-v", tmp + "/_runner_file_commands:/github/file_commands"}, {"-v", ws + ":/github/workspace"},
		{"alpine:3.20", "-c", "echo hi there"},
	} {
		if !has(w...) {
			t.Errorf("run lacks %q:\n%s", w, strings.Join(r, "\n"))
		}
	}
	// The docker CLI gets the variables as the container sees them.
	if !strings.Contains(run, "FOO=bar") || !strings.Contains(run, "HOME=/github/home") || !strings.Contains(run, "GITHUB_ENV=/github/file_commands/set_env_") ||
		!strings.Contains(run, "GITHUB_WORKSPACE=/github/workspace ") {
		t.Errorf("env of the docker process:\n%s", run)
	}
	// The echoed command line has the runner's quoting.
	if !strings.Contains(run, `--entrypoint "/bin/sh"`) || !strings.Contains(run, `-e "FOO"`) || !strings.Contains(run, ` alpine:3.20 -c "echo hi there"`) {
		t.Errorf("command line:\n%s", run)
	}
	if !strings.Contains(run, "-v \""+tmp+"\":\"/github/runner_temp\"") {
		t.Errorf("volume quoting:\n%s", run)
	}
	// Inside the container the file command paths are the mounted ones; here the
	// fake docker saw the host's, and what "-e" passes is translated.
	if !strings.Contains(strings.Join(r, "\n"), "-e\nGITHUB_ENV\n") {
		t.Errorf("GITHUB_ENV is passed by name:\n%s", strings.Join(r, "\n"))
	}
}

func TestDockerPullRetries(t *testing.T) {
	rec, res, cs, _ := dockerRun(t, files{}, "      - uses: docker://busybox\n", 2)
	if res.Conclusion != ConclusionSuccess || len(cs) != 4 {
		t.Fatalf("%+v calls %d", res, len(cs))
	}
	if got := logOf(rec.steps[1]); strings.Count(got, "##[warning]Docker pull failed with exit code 1, back off 0.001 seconds before retry.") != 2 {
		t.Errorf("%s", got)
	}
	rec, res, _, _ = dockerRun(t, files{}, "      - uses: docker://busybox\n", 5)
	if res.Conclusion != ConclusionFailure || rec.steps[1].Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.steps[1]), "##[error]Docker pull failed with exit code 1") {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	if len(rec.steps) != 4 || rec.steps[2].Conclusion != ConclusionSkipped {
		t.Errorf("a failed pull skips the step: %v", rec.names())
	}
}

func TestDockerfileAction(t *testing.T) {
	tree := files{
		"act/action.yml": "name: d\ninputs:\n  who:\n    default: me\nruns:\n  using: docker\n  image: Dockerfile\n  entrypoint: /entry.sh\n  args:\n    - hello\n    - ${{ inputs.who }}\n    - ''\n    - 'a \"quoted\" one'\n  env:\n    ACT_ENV: ${{ inputs.who }}\n",
		"act/Dockerfile": "FROM alpine\n",
	}
	rec, res, cs, _ := dockerRun(t, tree, "      - uses: ./act\n        with:\n          who: Ermite\n", 0)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	got := logOf(rec.step(t, "Run ./act"))
	// A local Dockerfile action builds in its own step (ContainerActionHandler.cs:64-84).
	if !strings.Contains(got, "##[group]Building docker image\nDockerfile for action: '") || !strings.Contains(got, "##[command]") {
		t.Errorf("%s", got)
	}
	if len(cs) != 2 || cs[0][0] != "build" || cs[1][0] != "run" {
		t.Fatalf("calls %v", cs)
	}
	if cs[0][1] != "-t" || !strings.Contains(cs[0][2], ":") || cs[0][3] != "-f" || !strings.HasSuffix(cs[0][4], "/act/Dockerfile") || !strings.HasSuffix(cs[0][5], "/act") {
		t.Errorf("build args %v", cs[0])
	}
	run := strings.Join(cs[1], "\n")
	if !strings.Contains(run, "--entrypoint\n/entry.sh\n") || !strings.HasSuffix(run, "\nhello\nEr"+"mite\n\na \"quoted\" one") {
		t.Errorf("args:\n%s", run)
	}
	if !strings.Contains(got, ` "hello" "Ermite" "" "a \"quoted\" one"`) {
		t.Errorf("echoed args:\n%s", got)
	}
	if !strings.Contains(run, "-e\nACT_ENV\n") {
		t.Errorf("env of the manifest:\n%s", run)
	}
}

func TestRepositoryDockerfileActionBuildsInSetup(t *testing.T) {
	forge := newForge(t, map[string][]byte{"/tarball/o/d/v1": tarOf(t, "o-d-x", files{"Dockerfile": "FROM alpine\n"})})
	d, lf := fakeDocker(t, 0)
	rec, res := actionRun(t, files{}, "      - uses: o/d@v1\n      - uses: o/d@v1\n", func(c *Config) {
		c.Actions.DockerPath = d
		c.Actions.Resolver = forge.resolver("")
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	want := "Set up job|Build o/d@v1|Run o/d@v1|Run o/d@v1|Complete job"
	if got := strings.Join(rec.names(), "|"); got != want {
		t.Fatalf("%s", got)
	}
	if got := logOf(rec.steps[1]); !strings.HasPrefix(got, "##[group]Build container for action use: '") || !strings.Contains(got, "/o/d/v1/Dockerfile'.\n##[command]") || !strings.HasSuffix(got, "\nbuilt\n##[endgroup]") {
		t.Errorf("build step:\n%s", got)
	}
	if n := len(calls(t, lf)); n != 3 {
		t.Errorf("one build for the two steps, then two runs: %d calls", n)
	}
	// Both runs use the image the setup step built.
	cs := calls(t, lf)
	if cs[1][len(cs[1])-1] != cs[2][len(cs[2])-1] && cs[1][len(cs[1])-2] != cs[2][len(cs[2])-2] {
		t.Errorf("%v %v", cs[1], cs[2])
	}
}

func TestEscapeDockerArg(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "A": `"A"`, `a"b`: `"a\"b"`, `a\"b`: `"a\\\"b"`, `a\`: `"a\\"`, `a\\`: `"a\\\\"`, `a b`: `"a b"`,
	} {
		if got := escapeDockerArg(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	for in, want := range map[string]string{
		``: ``, `a b  c`: `a|b|c`, `-c "echo hi"`: `-c|echo hi`, `a\"b`: `a"b`, `"" x`: `|x`, `a"b c"d`: `ab cd`,
	} {
		if got := strings.Join(splitArgs(in), "|"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestTranslatePath(t *testing.T) {
	maps := []pathMap{{"/w/ws", "/github/workspace"}, {"/w/tmp/_github_home", "/github/home"}, {"/w/tmp", "/github/runner_temp"}}
	for in, want := range map[string]string{
		"/w/ws": "/github/workspace", "/w/ws/a/b": "/github/workspace/a/b", "/w/tmp/_github_home/x": "/github/home/x",
		"/w/tmp/f": "/github/runner_temp/f", "/w/wsx": "/w/wsx", "x": "x", "": "",
	} {
		if got := translatePath(maps, in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The Dockerfile is passed as written (Path.Combine) and the build context is
// Directory.GetParent(dockerFile).FullName, which Path.GetFullPath resolves
// lexically: "." and ".." are folded, symlinks are not looked at. Recorded in
// act-docker-pre-post: -f ".../work/r/r/./.github/actions/x/Dockerfile" with the
// context ".../work/r/r/.github/actions/x".
func TestDockerfileContextIsTheLexicalParent(t *testing.T) {
	tree := files{
		"act/action.yml": "name: d\nruns:\n  using: docker\n  image: sub/../Dockerfile\n",
		"act/Dockerfile": "FROM alpine\n",
		"act/sub/keep":   "",
	}
	rec, res, cs, _ := dockerRun(t, tree, "      - uses: ./act\n", 0)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	if len(cs) < 1 || cs[0][0] != "build" {
		t.Fatalf("calls %v", cs)
	}
	if f, c := cs[0][4], cs[0][5]; !strings.HasSuffix(f, "/./act/sub/../Dockerfile") || !strings.HasSuffix(c, "/act") || strings.Contains(c, "..") || strings.Contains(c, "/./") {
		t.Errorf("-f %q, context %q", f, c)
	}
}

// act-docker-image, act-docker-dockerfile: "Pull <image>" and "Build <action>"
// are runner steps of the job and are in the steps context under a random
// 32-hex id with their outcome and conclusion (JobExtension.cs:434-440,
// ExecutionContext.cs:644).
func TestSetupStepsAreInTheStepsContext(t *testing.T) {
	needTools(t, "bash")
	forge := newForge(t, map[string][]byte{"/tarball/o/d/v1": tarOf(t, "o-d-x", files{"Dockerfile": "FROM alpine\n"})})
	d, _ := fakeDocker(t, 0)
	rec, res := actionRun(t, files{}, "      - uses: docker://alpine:3.20\n      - uses: o/d@v1\n      - name: dump\n        run: echo \"S=$S\"\n        env:\n          S: ${{ join(steps.*.conclusion, ',') }} ${{ join(steps.*.outcome, ',') }} ${{ toJSON(steps) }}\n", func(c *Config) {
		c.Actions.DockerPath = d
		c.Actions.Resolver = forge.resolver("")
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	got := logOf(rec.step(t, "dump"))
	re := regexp.MustCompile(`(?s)S=success,success success,success \{.*"[0-9a-f]{32}": \{\s*"outputs": \{\},\s*"outcome": "success",\s*"conclusion": "success"\s*\}.*"[0-9a-f]{32}": \{`)
	if !re.MatchString(got) {
		t.Errorf("%s", got)
	}
}

// A setup step that is skipped (the pull before it failed) is in the steps
// context too, with outcome and conclusion "skipped": the context names are
// assigned before anything runs and CompleteStep(Skipped) updates the context
// (JobExtension.cs:434-440, StepsRunner.cs:231-234, ExecutionContext.cs:644-647).
func TestSkippedSetupStepIsInTheStepsContext(t *testing.T) {
	needTools(t, "bash")
	d, _ := fakeDocker(t, 3) // the three pulls of the first image fail
	rec, res := actionRun(t, files{}, "      - uses: docker://alpine:3.20\n      - uses: docker://busybox:1\n      - name: dump\n        if: always()\n        run: echo \"S=$S\"\n        env:\n          S: ${{ toJSON(steps) }}\n", func(c *Config) {
		c.Actions.DockerPath = d
		c.Actions.Backoff = func() time.Duration { return time.Millisecond }
	})
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := strings.Join(rec.names(), "|"); got != "Set up job|Pull alpine:3.20|Pull busybox:1|Run alpine:3.20|Run busybox:1|dump|Complete job" {
		t.Fatalf("%s", got)
	}
	got := logOf(rec.step(t, "dump"))
	got = got[strings.LastIndex(got, "\nS={"):] // the header echoes the env too
	for _, want := range []string{"failure", "skipped"} {
		re := regexp.MustCompile(`(?s)"[0-9a-f]{32}": \{\s*"outputs": \{\},\s*"outcome": "` + want + `",\s*"conclusion": "` + want + `"\s*\}`)
		if !re.MatchString(got) {
			t.Errorf("no %s runner step in:\n%s", want, got)
		}
	}
	if n := len(regexp.MustCompile(`"[0-9a-f]{32}": \{`).FindAllString(got, -1)); n != 2 {
		t.Errorf("%d runner steps in the context, want 2:\n%s", n, got)
	}
}
