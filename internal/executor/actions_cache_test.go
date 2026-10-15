// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
)

const (
	cacheTestURL   = "http://127.0.0.1:9089/"
	cacheTestToken = "eyJqIjoiMSJ9.c2lnbmF0dXJl"
)

func withCache(c *Config) { c.CacheURL, c.CacheToken = cacheTestURL, cacheTestToken }

const envAction = `name: e
description: d
runs:
  using: node20
  main: main.sh
`

const envScript = "echo \"url=[$ACTIONS_CACHE_URL] rurl=[$ACTIONS_RUNTIME_URL] token=[$ACTIONS_RUNTIME_TOKEN] v2=[${ACTIONS_CACHE_SERVICE_V2-unset}] results=[${ACTIONS_RESULTS_URL-unset}]\"\n"

// NodeScriptActionHandler.cs:55-72: a node action gets the runtime variables. The
// URL is the base the toolkit appends "_apis/artifactcache/…" to (it ends with a
// slash), ACTIONS_CACHE_SERVICE_V2 is never set, and the token is masked in the
// log.
func TestNodeActionGetsCacheEnv(t *testing.T) {
	rec, res := actionRun(t, files{".github/actions/e/action.yml": envAction, ".github/actions/e/main.sh": envScript},
		"      - uses: ./.github/actions/e\n", withCache)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s", res, logOf(rec.steps[1]))
	}
	got := logOf(rec.step(t, "Run ./.github/actions/e"))
	want := "url=[" + cacheTestURL + "] rurl=[" + cacheTestURL + "] token=[***] v2=[unset] results=[unset]"
	if !strings.Contains(got, want) {
		t.Errorf("want %q in\n%s", want, got)
	}
	if strings.Contains(got, cacheTestToken) {
		t.Errorf("the token is in the log:\n%s", got)
	}
}

// A run: step is not an action handler: it gets none of it (the official runner
// sets these for actions only), and a job without a cache service sets nothing.
func TestCacheEnvOnlyForActions(t *testing.T) {
	rec, res := actionRun(t, files{}, "      - run: echo \"[$ACTIONS_CACHE_URL][$ACTIONS_RUNTIME_TOKEN]\"\n", withCache)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rec.step(t, "Run echo \"[$ACTIONS_CACHE_URL][$ACTIONS_RUNTIME_TOKEN]\"")); !strings.HasSuffix(strings.TrimRight(got, "\n"), "\n[][]") {
		t.Errorf("run step env:\n%s", got)
	}
	rec, res = actionRun(t, files{".github/actions/e/action.yml": envAction, ".github/actions/e/main.sh": envScript},
		"      - uses: ./.github/actions/e\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rec.step(t, "Run ./.github/actions/e")); !strings.Contains(got, "url=[] rurl=[] token=[]") {
		t.Errorf("no cache service, no variables:\n%s", got)
	}
}

// ContainerActionHandler.cs:219-236: a docker action gets them too, passed by
// name to "docker run" like the other variables.
func TestDockerActionGetsCacheEnv(t *testing.T) {
	d, lf := fakeDocker(t, 0)
	rec, res := actionRun(t, files{}, "      - uses: docker://alpine:3.20\n", func(c *Config) {
		c.Actions.DockerPath = d
		withCache(c)
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	cs := calls(t, lf)
	if len(cs) != 2 {
		t.Fatalf("calls %v", cs)
	}
	args := "\n" + strings.Join(cs[1], "\n") + "\n"
	for _, n := range []string{"ACTIONS_CACHE_URL", "ACTIONS_RUNTIME_URL", "ACTIONS_RUNTIME_TOKEN"} {
		if !strings.Contains(args, "\n-e\n"+n+"\n") {
			t.Errorf("docker run lacks -e %s:\n%s", n, args)
		}
	}
	if strings.Contains(args, "ACTIONS_CACHE_SERVICE_V2") || strings.Contains(args, cacheTestToken) {
		t.Errorf("service v2 or the token value on the command line:\n%s", args)
	}
	if run := logOf(rec.steps[2]); strings.Contains(run, cacheTestToken) {
		t.Errorf("the token is in the log:\n%s", run)
	}
}
