// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const testToken = "ghs_testtoken123"

func tokenContexts() string {
	return strings.Replace(testGitHubContext("main"), `"github":{`, `"github":{"token":"`+testToken+`",`, 1)
}

func TestMaskerForms(t *testing.T) {
	m := newMasker(testToken)
	for _, tc := range []struct{ name, text string }{
		{"plain", "x " + testToken + " y"},
		{"base64", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(testToken))},
		{"base64, one byte of prefix", base64.StdEncoding.EncodeToString([]byte("u" + testToken))},
		{"base64, two bytes of prefix", base64.StdEncoding.EncodeToString([]byte("us" + testToken))},
		{"base64, three bytes of prefix", base64.StdEncoding.EncodeToString([]byte("usr" + testToken))},
	} {
		if got := m.mask(tc.text); strings.Contains(got, testToken) || got == tc.text || !strings.Contains(got, "***") {
			t.Errorf("%s: %q -> %q", tc.name, tc.text, got)
		}
		if !m.has(tc.text) {
			t.Errorf("%s: has() is false", tc.name)
		}
	}
	// base64 at the three offsets: the secret's own text is found in every form
	for shift := 0; shift < 3; shift++ {
		prefix := strings.Repeat("p", shift)
		enc := base64.StdEncoding.EncodeToString([]byte(prefix + testToken))
		if got := m.mask("[" + enc + "]"); strings.Contains(got, enc[4:len(enc)-4]) {
			t.Errorf("shift %d: %q", shift, got)
		}
	}
	for _, tc := range []string{"", "nothing here", "ghs_other"} {
		if m.has(tc) {
			t.Errorf("%q must not be masked", tc)
		}
	}
	if !newMasker().has("") == false || newMasker().mask("a") != "a" {
		t.Error("an empty masker changes text")
	}
	// special characters: the JSON and URL forms
	s := newMasker(`a"b/c d`)
	for _, text := range []string{`a"b/c d`, `a\"b/c d`, `a%22b%2Fc%20d`} {
		if got := s.mask(text); strings.Contains(got, "b") {
			t.Errorf("%q -> %q", text, got)
		}
	}
}

// Bytes after the secret change the end of its base64 (`echo "$T" | base64`
// appends a newline): the part the secret alone determines is still masked, at
// the three offsets and whatever the length of the secret.
func TestMaskerBase64FollowedByOtherBytes(t *testing.T) {
	for _, secret := range []string{testToken, testToken + "4", testToken + "45", "ghs_0123456789abcdef"} {
		m := newMasker(secret)
		for shift := 0; shift < 3; shift++ {
			prefix := strings.Repeat("p", shift)
			for _, suffix := range []string{"\n", "!", "xy", " and more text"} {
				enc := base64.StdEncoding.EncodeToString([]byte(prefix + secret + suffix))
				text := "out: " + enc + " :end"
				got := m.mask(text)
				if !m.has(text) || got == text || !strings.Contains(got, "***") {
					t.Errorf("secret %q, prefix %q, suffix %q: %q -> %q", secret, prefix, suffix, text, got)
					continue
				}
				// the groups the secret alone determines are gone
				if strings.Contains(got, enc[4:12]) {
					t.Errorf("secret %q, prefix %q, suffix %q: %q still has the secret", secret, prefix, suffix, got)
				}
			}
		}
	}
	// the exact case of the review
	m := newMasker(testToken)
	if got := m.mask("Z2hzX3Rlc3R0b2tlbjEyMwo="); got == "Z2hzX3Rlc3R0b2tlbjEyMwo=" {
		t.Errorf("echo $T | base64 is not masked: %q", got)
	}
	// a short secret does not mask the base64 of a few common bytes
	short := newMasker("abcd")
	if got := short.mask("YWJj"); got != "YWJj" {
		t.Errorf("a short tail is masked: %q", got)
	}
}

// The name of a step that holds the token is masked before it reaches the
// server (StepStarted and StepCompleted).
func TestStepNameIsMasked(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    steps:
      - name: leak ${{ github.token }}
        run: echo hi
      - name: leak2 ${{ secrets.GITHUB_TOKEN }} ${{ env.X }}
        env:
          X: x
        run: echo hi
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) { c.ContextsJSON = tokenContexts() })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	for _, s := range rec.steps {
		if strings.Contains(s.Name, testToken) {
			t.Errorf("step name %q holds the token", s.Name)
		}
	}
	rec.step(t, "leak ***")
	rec.step(t, "leak2 *** x")
}

// A job output that holds the token is dropped with GitHub's warning; the
// other outputs stay.
func TestJobOutputWithTokenIsDropped(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    outputs:
      leak: ${{ steps.s.outputs.v }}
      fine: ${{ steps.s.outputs.w }}
    steps:
      - id: s
        env:
          T: ${{ github.token }}
        run: |
          echo "v=prefix-$T-suffix" >> "$GITHUB_OUTPUT"
          echo "w=ok" >> "$GITHUB_OUTPUT"
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) { c.ContextsJSON = tokenContexts() })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	if _, has := res.Outputs["leak"]; has || res.Outputs["fine"] != "ok" {
		t.Errorf("outputs %v", res.Outputs)
	}
	for _, v := range res.Outputs {
		if strings.Contains(v, testToken) {
			t.Errorf("the token is in an output: %q", v)
		}
	}
	if log := logOf(rec.step(t, CompleteJobName)); !strings.Contains(log, "##[warning]Skip output 'leak' since it may contain secret.") {
		t.Errorf("warning missing:\n%s", log)
	}
}

// Everything the executor reports goes through the masker: the names of the
// steps, the log, and the error of the job (no real run puts a token there, so
// the reporter is exercised directly).
func TestMaskReporter(t *testing.T) {
	rec := newRecorder()
	j := &job{}
	j.masker.Store(newMasker(testToken))
	r := &maskReporter{j: j, next: rec}
	r.StepStarted(1, "name "+testToken, time.Now())
	r.Log(1, time.Now(), "line "+testToken)
	r.StepCompleted(1, "name "+testToken, ConclusionSuccess, time.Now())
	r.JobCompleted(Result{Conclusion: ConclusionFailure, Error: "boom " + testToken})
	if s := rec.steps[0]; s.Name != "name ***" || s.Log[0] != "line ***" {
		t.Errorf("%+v", s)
	}
	if rec.result.Error != "boom ***" {
		t.Errorf("error %q", rec.result.Error)
	}
}

// The token is masked before a message is cut at 4096 bytes: a token split by
// the cut would otherwise leave its start in the line.
func TestMaskBeforeTruncation(t *testing.T) {
	needTools(t, "bash")
	pad := strings.Repeat("a", maxIssueMessageLength-5)
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    steps:
      - run: |
          echo "::error::` + pad + `${{ github.token }}"
`
	rec, _ := runLocal(t, yaml, "main", func(c *Config) { c.ContextsJSON = tokenContexts() })
	for _, s := range rec.steps {
		for _, l := range s.Log {
			if strings.Contains(l, "ghs_") {
				t.Errorf("a piece of the token leaked: %q", l[len(l)-20:])
			}
		}
	}
}
