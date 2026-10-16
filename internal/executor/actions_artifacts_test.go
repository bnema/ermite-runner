// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withResults is a job whose runtime channel serves the cache and the results
// service.
func withResults(c *Config) { withCache(c); c.Results = true }

// ACTIONS_RESULTS_URL (NodeScriptActionHandler.cs:71-84) is the runtime URL,
// for node actions, when the job has the results service only.
func TestNodeActionGetsResultsURL(t *testing.T) {
	tree := files{".github/actions/e/action.yml": envAction, ".github/actions/e/main.sh": envScript}
	for _, tc := range []struct {
		name  string
		tweak func(*Config)
		want  string
	}{
		{"results", withResults, "url=[" + cacheTestURL + "] rurl=[" + cacheTestURL + "] token=[***] v2=[unset] results=[" + cacheTestURL + "]"},
		{"cache only", withCache, "url=[" + cacheTestURL + "] rurl=[" + cacheTestURL + "] token=[***] v2=[unset] results=[unset]"},
		// A channel that serves results only: no cache variables.
		{"results only", func(c *Config) { withResults(c); c.CacheMode = "write"; c.CacheOff = true },
			"mode=[unset] url=[] rurl=[" + cacheTestURL + "] token=[***] v2=[unset] results=[" + cacheTestURL + "]"},
	} {
		rec, res := actionRun(t, tree, "      - uses: ./.github/actions/e\n", tc.tweak)
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v", tc.name, res)
		}
		if got := logOf(rec.step(t, "Run ./.github/actions/e")); !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q in\n%s", tc.name, tc.want, got)
		}
	}
}

// ContainerActionHandler.cs:237-245: a docker action gets ACTIONS_RESULTS_URL
// too, with the URL of the bridge (its network is not the sandbox's), and no
// GITHUB_SERVER_URL of its own.
func TestDockerActionGetsResultsURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tweak func(*Config)
		want  string
	}{
		{"results", withResults, "RUN-RT RUNTIME=" + cacheTestContainerURL + " CACHE=" + cacheTestContainerURL + " RESULTS=" + cacheTestContainerURL + " SERVER=https://ermite.example\n"},
		{"cache only", withCache, "RUN-RT RUNTIME=" + cacheTestContainerURL + " CACHE=" + cacheTestContainerURL + " RESULTS=unset SERVER=https://ermite.example\n"},
		{"results only", func(c *Config) { withResults(c); c.CacheOff = true }, "RUN-RT RUNTIME=" + cacheTestContainerURL + " CACHE=unset RESULTS=" + cacheTestContainerURL + " SERVER=https://ermite.example\n"},
		// No bridge in the sandbox: the container gets nothing.
		{"no bridge", func(c *Config) { withResults(c); c.ContainerCacheURL = "" }, "RUN-RT RUNTIME=unset CACHE=unset RESULTS=unset SERVER=https://ermite.example\n"},
	} {
		d, _ := fakeDocker(t, 0)
		rec, res := actionRun(t, files{}, "      - uses: docker://alpine:3.20\n", func(c *Config) {
			c.Actions.DockerPath = d
			tc.tweak(c)
		})
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v %v", tc.name, res, rec.names())
		}
		if got := logOf(rec.steps[2]); !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q in\n%s", tc.name, tc.want, got)
		}
	}
}

// A node action in the job container reaches the relay at the bridge URL; a
// run step there gets no runtime variable.
func TestContainerStepsGetContainerURL(t *testing.T) {
	tree := files{
		".github/actions/x/action.yml": "name: x\nruns:\n  using: node24\n  main: index.js\n",
		".github/actions/x/index.js":   "x\n",
	}
	vars := "ACTIONS_RUNTIME_URL ACTIONS_CACHE_URL ACTIONS_RESULTS_URL GITHUB_SERVER_URL"
	for _, tc := range []struct {
		name  string
		tweak func(*Config)
		want  string
	}{
		{"bridge", withResults, "VAR ACTIONS_RUNTIME_URL=" + cacheTestContainerURL + "\nVAR ACTIONS_CACHE_URL=" + cacheTestContainerURL + "\nVAR ACTIONS_RESULTS_URL=" + cacheTestContainerURL + "\nVAR GITHUB_SERVER_URL=https://ermite.example"},
		{"no bridge", func(c *Config) { withResults(c); c.ContainerCacheURL = "" }, "VAR ACTIONS_RUNTIME_URL=unset\nVAR ACTIONS_CACHE_URL=unset\nVAR ACTIONS_RESULTS_URL=unset\n"},
	} {
		f := newFakeCtr(t)
		f.set(t, "exec_vars", vars)
		rec, res := ctrRun(t, f, "    container: ubuntu:24.04\n", "      - name: a\n        uses: ./.github/actions/x\n      - name: r\n        run: echo\n", func(c *Config) {
			writeFiles(t, c.Layout.Workspace, tree)
			tc.tweak(c)
		})
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v\n%s", tc.name, res, logOf(rec.step(t, "a")))
		}
		if got := logOf(rec.step(t, "a")); !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q in\n%s", tc.name, tc.want, got)
		}
		if got := logOf(rec.step(t, "r")); !strings.Contains(got, "VAR ACTIONS_RUNTIME_URL=unset\nVAR ACTIONS_CACHE_URL=unset\nVAR ACTIONS_RESULTS_URL=unset\n") {
			t.Errorf("%s: run step:\n%s", tc.name, got)
		}
	}
}

// serverAction prints the GITHUB_SERVER_URL of its process and the
// github.server_url of its inputs, in its pre, main and post stages.
const serverAction = `name: s
inputs:
  ctx:
    default: "${{ github.server_url }}"
runs:
  using: node20
  pre: pre.sh
  main: main.sh
  post: post.sh
`

func serverScripts(extra string) files {
	f := files{"action.yml": serverAction}
	for _, st := range []string{"pre", "main", "post"} {
		f[st+".sh"] = extra + "echo \"" + st + " env=[$GITHUB_SERVER_URL] ctx=[$INPUT_CTX]\"\n"
	}
	return f
}

// serverForge serves, under each of the given repositories, an action that
// prints its server URL, at the root and in merge/.
func serverForge(t *testing.T, names ...string) *fakeForge {
	tree := files{}
	for k, v := range serverScripts("") {
		tree[k], tree["merge/"+k] = v, v
	}
	archives := map[string][]byte{}
	for _, n := range names {
		archives["/tarball/"+n+"/v4"] = tarOf(t, strings.ReplaceAll(n, "/", "-")+"-x", tree)
	}
	return newForge(t, archives)
}

const ghesURL = "env=[" + artifactsServerURL + "] ctx=[https://ermite.example]"
const ownURL = "env=[https://ermite.example] ctx=[https://ermite.example]"

// The node steps of upload-artifact and download-artifact (any case, any path
// inside them) get a GITHUB_SERVER_URL @actions/artifact takes for GitHub, in
// pre, main and post; the github context and the next steps keep the job's.
func TestArtifactActionsGetServerURL(t *testing.T) {
	for _, uses := range []string{"actions/upload-artifact@v4", "actions/download-artifact@v4", "actions/upload-artifact/merge@v4", "Actions/Upload-Artifact@v4", "ACTIONS/download-artifact@v4"} {
		name, _, _ := strings.Cut(uses, "@")
		repo := strings.Join(strings.SplitN(name, "/", 3)[:2], "/")
		forge := serverForge(t, repo)
		rec, res := actionRun(t, files{}, "      - uses: "+uses+"\n      - run: echo \"after=[$GITHUB_SERVER_URL] [${{ github.server_url }}]\"\n", func(c *Config) {
			withResults(c)
			c.Debug = true
			c.Actions.Resolver = forge.resolver("")
			c.Actions.Backoff = func() time.Duration { return time.Millisecond }
		})
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v %v\n%s", uses, res, rec.names(), logOf(rec.steps[0]))
		}
		for _, st := range []string{"Pre Run " + uses, "Run " + uses, "Post Run " + uses} {
			got := logOf(rec.step(t, st))
			if !strings.Contains(got, ghesURL) {
				t.Errorf("%s: %s:\n%s", uses, st, got)
			}
			if strings.Count(got, "##[debug]GITHUB_SERVER_URL set to "+artifactsServerURL+" for the artifacts service") != 1 {
				t.Errorf("%s: %s: the override is logged once:\n%s", uses, st, got)
			}
		}
		if got := logOf(rec.step(t, "Run echo \"after=[$GITHUB_SERVER_URL] [https://ermite.example]\"")); !strings.Contains(got, "after=[https://ermite.example] [https://ermite.example]") {
			t.Errorf("%s: the next step:\n%s", uses, got)
		}
	}
}

// A node action in a composite action is one too.
func TestArtifactActionInCompositeGetsServerURL(t *testing.T) {
	forge := serverForge(t, "actions/upload-artifact")
	tree := files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: actions/upload-artifact@v4\n"}
	rec, res := actionRun(t, tree, "      - uses: ./c\n", func(c *Config) { withResults(c); c.Actions.Resolver = forge.resolver("") })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := logOf(rec.step(t, "Run ./c")); !strings.Contains(got, "main "+ghesURL) {
		t.Errorf("composite:\n%s", got)
	}
}

// Without the results service, or for another action, nothing changes.
func TestServerURLUnchangedForOtherActions(t *testing.T) {
	forge := serverForge(t, "actions/upload-artifact", "octo/tool")
	for _, tc := range []struct {
		uses  string
		tweak func(*Config)
	}{{"actions/upload-artifact@v4", withCache}, {"octo/tool@v4", withResults}} {
		rec, res := actionRun(t, files{}, "      - uses: "+tc.uses+"\n", func(c *Config) {
			tc.tweak(c)
			c.Debug = true
			c.Actions.Resolver = forge.resolver("")
		})
		if res.Conclusion != ConclusionSuccess {
			t.Fatalf("%s: %+v %v", tc.uses, res, rec.names())
		}
		for _, s := range rec.steps {
			if got := logOf(s); strings.Contains(got, artifactsServerURL) {
				t.Errorf("%s: %s:\n%s", tc.uses, s.Name, got)
			}
		}
		if got := logOf(rec.step(t, "Run "+tc.uses)); !strings.Contains(got, "main "+ownURL) {
			t.Errorf("%s:\n%s", tc.uses, got)
		}
	}
}

// A script that bundles @actions/artifact v2 (its GHES message is in it) gets
// the URL whatever the action; a run step and a docker action never do.
func TestBundledArtifactClientGetsServerURL(t *testing.T) {
	tree := files{}
	for k, v := range serverScripts("# throw new GHESNotSupportedError('" + ghesMessage + "')\n") {
		tree["a/"+k] = v
	}
	d, _ := fakeDocker(t, 0)
	rec, res := actionRun(t, tree, "      - uses: ./a\n      - run: echo \"run=[$GITHUB_SERVER_URL]\"\n      - uses: docker://alpine:3.20\n", func(c *Config) {
		withResults(c)
		c.Actions.DockerPath = d
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	// A local action has no pre stage (it is not downloaded before the job).
	for _, st := range []string{"Run ./a", "Post Run ./a"} {
		if got := logOf(rec.step(t, st)); !strings.Contains(got, ghesURL) {
			t.Errorf("%s:\n%s", st, got)
		}
	}
	if got := logOf(rec.step(t, "Run echo \"run=[$GITHUB_SERVER_URL]\"")); !strings.Contains(got, "run=[https://ermite.example]") {
		t.Errorf("run step:\n%s", got)
	}
	if got := logOf(rec.step(t, "Run alpine:3.20")); !strings.Contains(got, "SERVER=https://ermite.example\n") {
		t.Errorf("docker action:\n%s", got)
	}
	// The same script without the results service: no change.
	rec, res = actionRun(t, tree, "      - uses: ./a\n", withCache)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := logOf(rec.step(t, "Run ./a")); !strings.Contains(got, "main "+ownURL) {
		t.Errorf("without results:\n%s", got)
	}
}

// The sniff finds the message across the boundary of two reads, and a file
// that cannot be read holds nothing.
func TestFileContains(t *testing.T) {
	dir := t.TempDir()
	needle := []byte(ghesMessage)
	for _, at := range []int{0, sniffChunk - 10, sniffChunk - len(needle), sniffChunk + len(needle) - 1, 3*sniffChunk + 5} {
		b := make([]byte, at, at+len(needle)+100)
		for i := range b {
			b[i] = 'x'
		}
		b = append(b, needle...)
		b = append(b, []byte(strings.Repeat("y", 100))...)
		p := filepath.Join(dir, "f")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if !fileContains(p, needle) {
			t.Errorf("message at %d not found", at)
		}
		// One byte short of the message: no match.
		if err := os.WriteFile(p, b[:at+len(needle)-1], 0o644); err != nil {
			t.Fatal(err)
		}
		if fileContains(p, needle) {
			t.Errorf("truncated message at %d found", at)
		}
	}
	if fileContains(filepath.Join(dir, "missing"), needle) || fileContains(dir, needle) {
		t.Error("an unreadable file holds the message")
	}
}

// In the job container the replacement comes after the runtime variables of
// the contexts too, in the environment docker exec passes on.
func TestContainerNodeStepGetsServerURL(t *testing.T) {
	tree := files{
		".github/actions/x/action.yml": "name: x\nruns:\n  using: node24\n  main: index.js\n",
		".github/actions/x/index.js":   "throw new Error('" + ghesMessage + "')\n",
	}
	f := newFakeCtr(t)
	f.set(t, "exec_vars", "GITHUB_SERVER_URL")
	rec, res := ctrRun(t, f, "    container: ubuntu:24.04\n", "      - name: a\n        uses: ./.github/actions/x\n      - name: r\n        run: echo\n", func(c *Config) {
		writeFiles(t, c.Layout.Workspace, tree)
		withResults(c)
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.step(t, "a")))
	}
	if got := logOf(rec.step(t, "a")); !strings.Contains(got, "VAR GITHUB_SERVER_URL="+artifactsServerURL) {
		t.Errorf("node step:\n%s", got)
	}
	if got := logOf(rec.step(t, "r")); !strings.Contains(got, "VAR GITHUB_SERVER_URL=https://ermite.example") {
		t.Errorf("run step:\n%s", got)
	}
}
