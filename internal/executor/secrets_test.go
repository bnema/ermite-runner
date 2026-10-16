// SPDX-License-Identifier: MIT

package executor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"bnema.dev/ermite-actions/expr"
	"github.com/bnema/zerowrap"
)

// jobSecretMasker is the masker of a job holding these secrets, built the way
// Run does: the token first, then the secrets of the job.
func jobSecretMasker(t *testing.T, secrets map[string]string) *masker {
	t.Helper()
	s, _, err := NewState(testGitHubContext("main"), "", "main", RunnerInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.WithJobData(secrets, nil)
	m := newMasker(s.Secrets()...)
	registerSecrets(m, secrets)
	return m
}

// A secret is masked as is and in the encodings of CI-2 (Worker.cs:158-171,
// HostContext.cs:103-113): base64, JSON, URL.
func TestJobSecretIsMaskedInItsEncodings(t *testing.T) {
	const sec = `p@ss"w/rd é+1`
	m := jobSecretMasker(t, map[string]string{"DB_PASSWORD": sec})
	j, _ := json.Marshal(sec)
	for name, text := range map[string]string{
		"plain":  "x " + sec + " y",
		"base64": "Basic " + base64.StdEncoding.EncodeToString([]byte(sec)),
		"json":   "{\"k\":" + string(j) + "}",
		"url":    "?q=" + strings.ReplaceAll(url.QueryEscape(sec), "+", "%20"), // Uri.EscapeDataString
	} {
		got := m.mask(text)
		if got == text || !strings.Contains(got, "***") {
			t.Errorf("%s: %q was not masked: %q", name, text, got)
		}
	}
	if got := m.mask("harmless"); got != "harmless" {
		t.Errorf("%q", got)
	}
}

// Worker.cs:158-171: the value is trimmed, and each non-empty trimmed line of a
// multi-line value is registered too (split on CR and LF).
func TestJobSecretLinesAndTrim(t *testing.T) {
	m := jobSecretMasker(t, map[string]string{
		"KEY":  "  \n-----BEGIN KEY-----\r\n  line-two-secret  \n\n-----END KEY-----\n ",
		"PAD":  "  padded-secret \t\n",
		"NONE": "",
		"WS":   " \r\n ",
	})
	for _, text := range []string{"-----BEGIN KEY-----", "line-two-secret", "-----END KEY-----", "padded-secret",
		"-----BEGIN KEY-----\r\n  line-two-secret  \n\n-----END KEY-----"} {
		if m.mask("a "+text+" b") == "a "+text+" b" {
			t.Errorf("%q is not masked", text)
		}
	}
	// empty and blank values register nothing: text is untouched
	for _, text := range []string{"", " ", "\r\n", "plain text", "a b"} {
		if got := m.mask(text); got != text {
			t.Errorf("%q -> %q", text, got)
		}
	}
	// the value as sent, with its padding, is masked as far as the trimmed one
	if got := m.mask("v=padded-secret "); got != "v=*** " {
		t.Errorf("%q", got)
	}
}

func TestSecretsContext(t *testing.T) {
	s, _, err := NewState(tokenContexts(), "", "main", RunnerInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.WithJobData(map[string]string{"B": "2", "A": "1", "GITHUB_TOKEN": "forged", "EMPTY": ""}, nil)
	d := s.secrets()
	got := map[string]string{}
	for _, p := range d.Pairs() {
		got[p.Key] = str(d, p.Key)
	}
	if len(got) != 4 || got["GITHUB_TOKEN"] != testToken || got["A"] != "1" || got["B"] != "2" || got["EMPTY"] != "" {
		t.Errorf("secrets context %v", got)
	}
	// without a token and without secrets the context is empty
	s0, _, _ := NewState(testGitHubContext("main"), "", "main", RunnerInfo{}, "")
	if s0.secrets().Len() != 0 {
		t.Error("secrets context is not empty")
	}
}

func TestVarsContext(t *testing.T) {
	vars := map[string]string{"A": "from-job"}
	// the contexts JSON has no vars: Job.vars is the fallback
	s, _, _ := NewState(testGitHubContext("main"), "", "main", RunnerInfo{}, "")
	s.WithJobData(nil, vars)
	if got := dictOf(t, s.other["vars"]); got != "A=from-job" {
		t.Errorf("fallback: %s", got)
	}
	// the contexts JSON has vars: it wins
	withVars := strings.Replace(testGitHubContext("main"), `{"github"`, `{"vars":{"A":"from-contexts","B":"b"},"github"`, 1)
	s2, _, err := NewState(withVars, "", "main", RunnerInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s2.WithJobData(nil, vars)
	if got := dictOf(t, s2.other["vars"]); got != "A=from-contexts B=b" {
		t.Errorf("contexts: %s", got)
	}
	// neither: an empty object
	s3, _, _ := NewState(testGitHubContext("main"), "", "main", RunnerInfo{}, "")
	s3.WithJobData(nil, nil)
	if got := dictOf(t, s3.other["vars"]); got != "" {
		t.Errorf("none: %s", got)
	}
}

func TestJobSecretsEndToEnd(t *testing.T) {
	needTools(t, "bash")
	const (
		pw  = "hunter2-secret"
		key = "-----BEGIN-----\nsecond-line-secret\n-----END-----"
	)
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    outputs:
      leak: ${{ steps.s.outputs.v }}
      leak64: ${{ steps.s.outputs.b }}
      line: ${{ steps.s.outputs.l }}
      fine: ${{ steps.s.outputs.w }}
    steps:
      - id: s
        env:
          PW: ${{ secrets.PW }}
          KEY: ${{ secrets.KEY }}
          V: ${{ vars.V }}
          MISSING: ${{ secrets.NOPE }}
        run: |
          echo "pw=$PW"
          echo "b64=$(printf %s "$PW" | base64)"
          echo "line=$(printf %s "$KEY" | sed -n 2p)"
          echo "v=$V missing=[$MISSING]"
          echo "v=pre-$PW-post" >> "$GITHUB_OUTPUT"
          echo "b=$(printf %s "$PW" | base64)" >> "$GITHUB_OUTPUT"
          echo "l=second-line-secret" >> "$GITHUB_OUTPUT"
          echo "w=$V" >> "$GITHUB_OUTPUT"
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) {
		c.Secrets = map[string]string{"PW": pw, "KEY": key}
		c.Vars = map[string]string{"V": "visible"}
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	var all []string
	for _, s := range rec.steps {
		all = append(all, s.Name, logOf(s))
	}
	joined := strings.Join(all, "\n")
	for _, secret := range []string{pw, "second-line-secret", base64.StdEncoding.EncodeToString([]byte(pw))} {
		if strings.Contains(joined, secret) {
			t.Errorf("%q leaked in the log:\n%s", secret, joined)
		}
	}
	for _, want := range []string{"pw=***", "line=***", "v=visible missing=[]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q:\n%s", want, joined)
		}
	}
	if len(res.Outputs) != 1 || res.Outputs["fine"] != "visible" {
		t.Errorf("outputs %v", res.Outputs)
	}
	done := logOf(rec.step(t, CompleteJobName))
	for _, n := range []string{"leak", "leak64", "line"} {
		if w := "##[warning]Skip output '" + n + "' since it may contain secret."; !strings.Contains(done, w) {
			t.Errorf("warning %q missing:\n%s", w, done)
		}
	}
	if strings.Contains(done, "Skip output 'fine'") {
		t.Error("fine output skipped")
	}
}

func TestSecretsWithheldLine(t *testing.T) {
	needTools(t, "bash")
	yaml := wfHead + "    steps:\n      - run: echo hi\n"
	with := setupLog(t, yaml, "j", func(c *Config) { c.SecretsWithheld = true })
	if n := strings.Count(with, SecretsWithheldLine); n != 1 || SecretsWithheldLine != "Secrets are not available to this job: the run was started by a limited token." {
		t.Errorf("line count %d:\n%s", n, with)
	}
	if without := setupLog(t, yaml, "j", nil); strings.Contains(without, "Secrets are not available") {
		t.Errorf("printed without the flag:\n%s", without)
	}
}

// dictOf renders a string dictionary as "K=v K=v" in order.
func dictOf(t *testing.T, v expr.Value) string {
	t.Helper()
	d, ok := v.(*expr.Dictionary)
	if !ok {
		t.Fatalf("not a dictionary: %T", v)
	}
	var parts []string
	for _, p := range d.Pairs() {
		parts = append(parts, p.Key+"="+str(d, p.Key))
	}
	return strings.Join(parts, " ")
}

// The first lines of "Set up job" are reported before the contexts exist: the
// secrets of the job are already in the masker then (GO-002). GitHub's
// SecretMasker has no minimum length (SecretMasker.AddValue ignores only null
// or empty values, SecretMasker.cs:78-84), so a one-character secret is masked.
func TestSecretsAreMaskedFromTheFirstLine(t *testing.T) {
	needTools(t, "bash")
	yaml := wfHead + "    steps:\n      - run: echo hi\n"
	rec, res := runLocal(t, yaml, "j", func(c *Config) {
		c.Secrets = map[string]string{"ONE": "7", "LONG": "topsecretvalue"}
		c.RunnerVersion = "v7.1-topsecretvalue"
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	first := rec.steps[0].Log[0]
	if first != "Current runner version: 'v***.1-***'" {
		t.Errorf("first line %q", first)
	}
}

// An early failure of "Set up job" (before the contexts are built) is masked in
// the log and in the error of the job.
func TestEarlySetupFailureIsMasked(t *testing.T) {
	yaml := wfHead + "    steps:\n      - run: echo hi\n"
	rec, res := runLocal(t, yaml, "no-such-job-S3CRET", func(c *Config) {
		c.Secrets = map[string]string{"S": "S3CRET", "ONE": "-"}
	})
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	all := logOf(rec.steps[0]) + "\n" + res.Error + "\n" + rec.result.Error
	if strings.Contains(all, "S3CRET") || strings.Contains(all, "-") || !strings.Contains(all, "no***such***job***") {
		t.Errorf("not masked:\n%s", all)
	}
}

// The diagnostics of the runner (Config.Log) bypass the reporter: they are
// masked by diag (GO-001). A path with a secret in a failing hashFiles reaches
// the diagnostics through the error.
func TestDiagnosticsAreMasked(t *testing.T) {
	needTools(t, "bash")
	const sec = "s3cr3t-dir-name"
	yaml := `on: push
jobs:
  j:
    runs-on: x
    steps:
      - run: ln -s missing ` + sec + `
      - run: echo ${{ hashFiles('**') }}
`
	var logs bytes.Buffer
	rec, res := runLocal(t, yaml, "j", func(c *Config) {
		c.Secrets = map[string]string{"D": sec}
		c.Log = zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs})
	})
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(logs.String(), "hashFiles failed") {
		t.Fatalf("no diagnostic:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), sec) || !strings.Contains(logs.String(), "/***: statat ***") {
		t.Errorf("diagnostics not masked:\n%s", logs.String())
	}
	for _, s := range rec.steps {
		if strings.Contains(logOf(s), sec) {
			t.Errorf("job log leaks:\n%s", logOf(s))
		}
	}

	// The helper masks message, error and fields.
	logs.Reset()
	j := &job{cfg: Config{Log: zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs})}}
	j.masker.Store(newMasker(sec))
	j.diagWarn(errors.New("open "+sec+": no"), "msg "+sec, "k", "v "+sec)
	j.diagDebug(nil, "d "+sec)
	if strings.Contains(logs.String(), sec) || strings.Count(logs.String(), "***") != 4 {
		t.Errorf("helper: %s", logs.String())
	}
}

// Config.Log is used only by diag: any other use would bypass the masker.
func TestOnlyDiagUsesTheDiagnosticsLogger(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		n := strings.Count(string(b), "cfg.Log")
		// job.go: the two uses are in diag.
		if (f == "job.go" && n != 2) || (f != "job.go" && n != 0) {
			t.Errorf("%s uses cfg.Log %d times: use diagWarn or diagDebug", f, n)
		}
	}
}

// The group prints what the server sent in Job.permissions, as GitHub's runner
// prints system.github.token.permissions, and not what the workflow says.
func TestSetUpJobPrintsTheServersTokenPermissions(t *testing.T) {
	needTools(t, "bash")
	group := func(log string) string {
		_, rest, _ := strings.Cut(log, "##[group]GITHUB_TOKEN Permissions\n")
		g, _, _ := strings.Cut(rest, "\n##[endgroup]")
		return g
	}
	// The workflow asks for write-all; the server lowered it (an untrusted run).
	yaml := "on: push\npermissions: write-all\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	for _, tc := range []struct {
		name   string
		levels map[string]string
		want   string
	}{
		{"lowered by the server", map[string]string{"contents": "read", "pull-requests": "read", "issues": "read", "metadata": "read"},
			"Contents: read\nIssues: read\nMetadata: read\nPullRequests: read"},
		{"write kept", map[string]string{"contents": "write", "metadata": "read", "packages": "read"}, "Contents: write\nMetadata: read\nPackages: read"},
		{"none and id-token are not listed, metadata always is", map[string]string{"contents": "none", "id-token": "write", "actions": "read"},
			"Actions: read\nMetadata: read"},
		{"nothing at all is metadata only", map[string]string{}, "Metadata: read"},
		{"a key the runner does not know", map[string]string{"made-up-key": "read"}, "MadeUpKey: read\nMetadata: read"},
	} {
		got := group(setupLog(t, yaml, "j", func(c *Config) { c.TokenPermissions = tc.levels }))
		if got != tc.want {
			t.Errorf("%s:\n%s\nwant:\n%s", tc.name, got, tc.want)
		}
	}
	// A server that sends none: worked out from the workflow, as before.
	got := group(setupLog(t, yaml, "j", nil))
	if !strings.Contains(got, "Contents: write") || !strings.Contains(got, "Models: read") {
		t.Errorf("fallback to the workflow:\n%s", got)
	}
}
