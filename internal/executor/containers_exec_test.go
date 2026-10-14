// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
)

// execCalls are the `docker exec` calls recorded by the fake.
func (f *fakeCtr) execCalls(t *testing.T) [][]string {
	var out [][]string
	for _, c := range f.calls(t) {
		if c[0] == "exec" {
			out = append(out, c)
		}
	}
	return out
}

func TestRunStepThroughDockerExec(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "exec_out", "hello from the container\n")
	var work string
	rec, res := ctrRun(t, f, "    container:\n      image: ubuntu:24.04\n      env:\n        CVAR: c\n",
		"      - name: s\n        working-directory: sub\n        env:\n          FOO: bar\n        run: echo hi\n", func(c *Config) { work = c.Layout.WorkRoot })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.step(t, "Initialize containers")))
	}
	if got := strings.Join(rec.names(), "|"); got != "Set up job|Initialize containers|s|Stop containers|Complete job" {
		t.Fatalf("steps %s", got)
	}
	ex := f.execCalls(t)
	if len(ex) != 1 {
		t.Fatalf("exec calls %v", ex)
	}
	c := strings.Join(ex[0], "\n")
	id := strings.Repeat("0", 63) + "1"
	for _, w := range []string{
		"exec\n-i\n--workdir\n/__w/hello/hello/sub\n-e\nFOO\n", "\n-e\nGITHUB_WORKSPACE\n", "\n-e\nGITHUB_OUTPUT\n",
		"\n" + id + "\nsh\n-e\n/__w/_temp/",
	} {
		if !strings.Contains(c, w) {
			t.Errorf("exec lacks %q:\n%s", w, c)
		}
	}
	if strings.Contains(c, "bar") || strings.Contains(c, work) {
		t.Errorf("values and host paths stay off the command line:\n%s", c)
	}
	log := logOf(rec.step(t, "s"))
	if !strings.Contains(log, "shell: sh -e {0}\n") || !strings.Contains(log, "hello from the container") {
		t.Errorf("log:\n%s", log)
	}
	// The create command of the job container.
	create := logOf(rec.step(t, "Initialize containers"))
	for _, w := range []string{
		"--workdir /__w/hello/hello --network github_network_", "  -e \"CVAR=c\" -e \"HOME=/github/home\" -e GITHUB_ACTIONS=true -e CI=true -v \"/var/run/docker.sock\":\"/var/run/docker.sock\" -v \"" + work + "\":\"/__w\"",
		"\":\"/__e\":ro -v \"" + work + "/_temp\":\"/__w/_temp\"", "\":\"/__t\" -v \"" + work + "/_temp/_github_home\":\"/github/home\" -v \"" + work + "/_temp/_github_workflow\":\"/github/workflow\" --entrypoint \"tail\" ubuntu:24.04 \"-f\" \"/dev/null\"",
	} {
		if !strings.Contains(create, w) {
			t.Errorf("create lacks %q:\n%s", w, create)
		}
	}
}

func TestRunEnvIsTranslatedForTheContainer(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "exec_env", "1")
	rec, res := ctrRun(t, f, "    container: ubuntu:24.04\n",
		"      - name: s\n        run: echo hi\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	// HOME is the container's own (/github/home): it is not passed with -e.
	if c := strings.Join(f.execCalls(t)[0], "\n"); strings.Contains(c, "\nHOME\n") {
		t.Errorf("HOME must not be passed:\n%s", c)
	}
	log := logOf(rec.step(t, "s"))
	for _, w := range []string{
		"ENV GITHUB_WORKSPACE=/__w/hello/hello", "RUNNER_TEMP=/__w/_temp",
		"GITHUB_OUTPUT=/__w/_temp/_runner_file_commands/set_output_", "GITHUB_EVENT_PATH=/github/workflow/event.json",
	} {
		if !strings.Contains(log, w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
}

func TestNodeActionInContainerUsesExternalsAndAlpine(t *testing.T) {
	for _, tc := range []struct{ release, node string }{{"ID=ubuntu\nVERSION_ID=\"24.04\"\n", "node24"}, {"ID=alpine\nVERSION_ID=3.23.4\n", "node24_alpine"}} {
		f := newFakeCtr(t)
		f.set(t, "exec_out", tc.release)
		tree := files{
			".github/actions/x/action.yml": "name: x\nruns:\n  using: node24\n  main: index.js\n",
			".github/actions/x/index.js":   "x\n",
		}
		rec, res := ctrRun(t, f, "    container: ubuntu:24.04\n", "      - name: a\n        uses: ./.github/actions/x\n", func(c *Config) {
			writeFiles(t, c.Layout.Workspace, tree)
		})
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v\n%s", tc.node, res, logOf(rec.step(t, "a")))
		}
		ex := f.execCalls(t)
		if len(ex) != 2 {
			t.Fatalf("%s: exec calls %v", tc.node, ex)
		}
		if got := strings.Join(ex[0][len(ex[0])-4:len(ex[0])-1], " "); got != "sh -c "+alpineCheck {
			t.Errorf("probe: %v", ex[0])
		}
		run := strings.Join(ex[1], "\n")
		if !strings.Contains(run, "\n/__e/"+tc.node+"/bin/node\n") || !strings.Contains(run, "\n/__w/hello/hello/./.github/actions/x/index.js") {
			t.Errorf("%s: node call:\n%s", tc.node, run)
		}
		if got := logOf(rec.step(t, "a")); !strings.Contains(got, "exec  "+strings.Repeat("0", 63)+"1 sh -c \"cat /etc/*release | grep ^ID\"") {
			t.Errorf("probe line missing:\n%s", got)
		}
	}
}

func TestCommandPathsFromTheContainerAreTranslated(t *testing.T) {
	f := newFakeCtr(t)
	// The step prints an annotation for a file at its container path.
	f.set(t, "exec_out", "::error file=/__w/hello/hello/src/a.go,line=3::boom\n")
	rec, res := ctrRun(t, f, ctrJob, "      - name: s\n        run: echo\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	log := logOf(rec.step(t, "s"))
	if !strings.Contains(log, "##[error]boom") {
		t.Errorf("log:\n%s", log)
	}
}

func TestDockerActionJoinsTheJobNetworkAndContextHasContainerKey(t *testing.T) {
	f := newFakeCtr(t)
	rec, res := ctrRun(t, f, "    services:\n      redis:\n        image: redis:7\n",
		"      - uses: docker://alpine:3.20\n      - name: ctx\n        run: echo \"${{ toJSON(job.container) }}\"\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	var run []string
	for _, c := range f.calls(t) {
		if c[0] == "run" {
			run = c
		}
	}
	if run == nil || !strings.Contains(strings.Join(run, "\n"), "\n--network\ngithub_network_") {
		t.Errorf("docker run without the job network: %v", run)
	}
	if got := logOf(rec.step(t, "ctx")); !strings.Contains(got, "\"network\": \"github_network_") {
		t.Errorf("ctx:\n%s", got)
	}
}
