// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
)

const (
	idTokenTestURL   = "http://ermite.example:8088/-/oidc/token?api-version=2.0"
	idTokenTestToken = "ermiteoidc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func withIDToken(c *Config) {
	c.IDTokenURL, c.IDTokenRequestToken = idTokenTestURL, idTokenTestToken
}

const idTokenEcho = `echo "oidc url=[${ACTIONS_ID_TOKEN_REQUEST_URL-unset}] token=[${ACTIONS_ID_TOKEN_REQUEST_TOKEN-unset}]"`

const idTokenAction = "name: e\ndescription: d\nruns:\n  using: node20\n  main: main.sh\n"

var idTokenWant = "oidc url=[" + idTokenTestURL + "] token=[***]"

const idTokenUnset = "oidc url=[unset] token=[unset]"

// ScriptHandler.cs:315-319: a run step gets ACTIONS_ID_TOKEN_REQUEST_URL and
// ACTIONS_ID_TOKEN_REQUEST_TOKEN in its process, the token masked. The env of
// the step (its header, the env context) does not show them.
func TestRunStepGetsIDTokenEnv(t *testing.T) {
	rec, res := actionRun(t, files{},
		"      - name: r\n        env:\n          FOO: bar\n        run: |\n          "+idTokenEcho+"\n          echo \"ctx=[${{ env.ACTIONS_ID_TOKEN_REQUEST_URL }}]\"\n", withIDToken)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	got := logOf(rec.step(t, "r"))
	if !strings.Contains(got, idTokenWant) || !strings.Contains(got, "ctx=[]") {
		t.Errorf("want %q and an empty env context in\n%s", idTokenWant, got)
	}
	if strings.Contains(got, idTokenTestToken) || strings.Contains(got, "  ACTIONS_ID_TOKEN_REQUEST_URL:") {
		t.Errorf("the token or the variables are in the log:\n%s", got)
	}
}

// NodeScriptActionHandler.cs:66-70: a node action gets them too, without a
// cache service, and they are not in the env block of its header.
func TestNodeActionGetsIDTokenEnv(t *testing.T) {
	rec, res := actionRun(t, files{".github/actions/e/action.yml": idTokenAction, ".github/actions/e/main.sh": idTokenEcho + "\n"},
		"      - uses: ./.github/actions/e\n", withIDToken)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	got := logOf(rec.step(t, "Run ./.github/actions/e"))
	if !strings.Contains(got, idTokenWant) {
		t.Errorf("want %q in\n%s", idTokenWant, got)
	}
	if strings.Contains(got, idTokenTestToken) || strings.Contains(got, "  ACTIONS_ID_TOKEN_REQUEST_URL:") {
		t.Errorf("the token or the variables are in the log:\n%s", got)
	}
}

// A composite action sets nothing itself; its steps get the variables from
// their own handlers, as on GitHub.
func TestCompositeStepsGetIDTokenEnv(t *testing.T) {
	composite := "name: c\ndescription: d\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: '" + idTokenEcho + "'\n"
	rec, res := actionRun(t, files{".github/actions/c/action.yml": composite}, "      - uses: ./.github/actions/c\n", withIDToken)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if got := logOf(rec.step(t, "Run ./.github/actions/c")); !strings.Contains(got, idTokenWant) || strings.Contains(got, idTokenTestToken) {
		t.Errorf("want %q in\n%s", idTokenWant, got)
	}
}

// ContainerActionHandler.cs:232-236: a docker action gets them by name on the
// "docker run" command line, never by value.
func TestDockerActionGetsIDTokenEnv(t *testing.T) {
	d, lf := fakeDocker(t, 0)
	rec, res := actionRun(t, files{}, "      - uses: docker://alpine:3.20\n", func(c *Config) {
		c.Actions.DockerPath = d
		withIDToken(c)
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	cs := calls(t, lf)
	if len(cs) != 2 {
		t.Fatalf("calls %v", cs)
	}
	args := "\n" + strings.Join(cs[1], "\n") + "\n"
	for _, n := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
		if !strings.Contains(args, "\n-e\n"+n+"\n") {
			t.Errorf("docker run lacks -e %s:\n%s", n, args)
		}
	}
	if strings.Contains(args, idTokenTestToken) {
		t.Errorf("the token value is on the command line:\n%s", args)
	}
}

// A run step in the job container gets them through "docker exec -e NAME".
func TestRunStepInContainerGetsIDTokenEnv(t *testing.T) {
	f := newFakeCtr(t)
	rec, res := ctrRun(t, f, "    container: ubuntu:24.04\n", "      - name: s\n        run: echo hi\n", withIDToken)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	ex := f.execCalls(t)
	if len(ex) != 1 {
		t.Fatalf("exec calls %v", ex)
	}
	c := "\n" + strings.Join(ex[0], "\n") + "\n"
	for _, n := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
		if !strings.Contains(c, "\n-e\n"+n+"\n") {
			t.Errorf("exec lacks -e %s:\n%s", n, c)
		}
	}
	if strings.Contains(c, idTokenTestToken) {
		t.Errorf("the token value is on the command line:\n%s", c)
	}
}

// Nothing is set unless the job has both the URL and the token.
func TestIDTokenEnvNeedsBoth(t *testing.T) {
	for name, tweak := range map[string]func(*Config){
		"none":       nil,
		"url only":   func(c *Config) { c.IDTokenURL = idTokenTestURL },
		"token only": func(c *Config) { c.IDTokenRequestToken = idTokenTestToken },
	} {
		t.Run(name, func(t *testing.T) {
			rec, res := actionRun(t, files{".github/actions/e/action.yml": idTokenAction, ".github/actions/e/main.sh": idTokenEcho + "\n"},
				"      - name: r\n        run: '"+idTokenEcho+"'\n      - uses: ./.github/actions/e\n", tweak)
			if res.Conclusion != ConclusionSuccess {
				t.Fatalf("%+v", res)
			}
			for _, s := range []string{"r", "Run ./.github/actions/e"} {
				if got := logOf(rec.step(t, s)); !strings.Contains(got, idTokenUnset) {
					t.Errorf("%s: want %q in\n%s", s, idTokenUnset, got)
				}
			}
		})
	}
}

// The token is in the masker before the first step: a step that prints it,
// even by another name, shows ***.
func TestIDTokenIsMasked(t *testing.T) {
	rec, res := actionRun(t, files{}, "      - name: r\n        run: echo \"leak "+idTokenTestToken+"\"\n", withIDToken)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	for _, s := range rec.steps {
		if got := logOf(s); strings.Contains(got, idTokenTestToken) {
			t.Errorf("the token is in the log:\n%s", got)
		}
	}
	if got := logOf(rec.step(t, "r")); !strings.Contains(got, "leak ***") {
		t.Errorf("want the token masked in\n%s", got)
	}
}
