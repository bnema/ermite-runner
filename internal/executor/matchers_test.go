// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
	"time"
)

func mustConfig(t *testing.T, js string) *matchersConfig {
	t.Helper()
	c, err := parseMatchersConfig([]byte(js))
	if err != nil || c == nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// Cases of actions/runner src/Test/L0/Worker/IssueMatcherL0.cs (MIT, (c) 2019
// GitHub; see THIRD_PARTY.md): Config_Validate_*.
func TestMatcherConfigValidate(t *testing.T) {
	one := func(extra string) string {
		return `{"problemMatcher":[{"owner":"myMatcher","pattern":[` + extra + `]}]}`
	}
	for _, tc := range []struct{ name, js, err string }{
		{"loop on a single pattern", one(`{"regexp":"^error: (.+)$","message":1,"loop":true}`), "Only the last pattern in a multiline matcher may set 'loop'"},
		{"loop sanity", one(`{"regexp":"^file: (.+)$","file":1},{"regexp":"^error: (.+)$","message":1,"loop":true}`), ""},
		{"loop only on the last", one(`{"regexp":"^(error)$","severity":1},{"regexp":"^file: (.+)$","file":1,"loop":true},{"regexp":"^error: (.+)$","message":1}`), "Only the last pattern in a multiline matcher may set 'loop'"},
		{"loop must set message", one(`{"regexp":"^file: (.+)$","message":1},{"regexp":"^file: (.+)$","file":1,"loop":true}`), "The loop pattern must set 'message'"},
		{"message in the first pattern", one(`{"regexp":"^file: (.+)$","message":1},{"regexp":"^error: (.+)$","file":1}`), ""},
		{"message required", one(`{"regexp":"^error: (.+)$","file":1}`), "At least one pattern must set 'message'"},
		{"owners distinct", `{"problemMatcher":[{"owner":"myMatcher","pattern":[{"regexp":"^error: (.+)$","message":1}]},{"owner":"MYmatcher","pattern":[{"regexp":"^ERR: (.+)$","message":1}]}]}`, "Duplicate owner name 'MYmatcher'"},
		{"owner required", `{"problemMatcher":[{"pattern":[{"regexp":"^(.+)$","message":1}]}]}`, "Owner must not be empty"},
		{"pattern required", `{"problemMatcher":[{"owner":"m"}]}`, "Matcher 'm' does not contain any patterns"},
		{"property set twice", one(`{"regexp":"^(.+) (.+)$","file":1,"message":2},{"regexp":"^(.+)$","file":1}`), "The property 'file' is set twice"},
		{"out of range", one(`{"regexp":"^(.+)$","message":2}`), "The property 'message' is set to 2 which is out of range"},
		{"less than zero", one(`{"regexp":"^(.+)$","message":-1}`), "The property 'message' is set to -1 which is out of range"},
		{"bad severity", `{"problemMatcher":[{"owner":"m","severity":"fatal","pattern":[{"regexp":"(.+)","message":1}]}]}`, "Matcher 'm' contains unexpected default severity 'fatal'"},
	} {
		err := mustConfig(t, tc.js).validate()
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.err)
		}
	}
}

func newMatcher(t *testing.T, js string) *issueMatcher {
	t.Helper()
	c := mustConfig(t, js)
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	m, err := newIssueMatcher(c.Matchers[0], time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func matchOf(t *testing.T, m *issueMatcher, line string) *issueMatch {
	t.Helper()
	r, err := m.match(line)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// IssueMatcherL0.Matcher_MultiplePatterns_Loop_AccumulatesStatePerLine.
func TestMatcherLoopAccumulatesState(t *testing.T) {
	m := newMatcher(t, `{"problemMatcher":[{"owner":"myMatcher","pattern":[
		{"regexp":"^(.+)$","file":1},{"regexp":"^(.+)$","code":1},{"regexp":"^message:(.+)$","message":1,"loop":true}]}]}`)
	type want struct{ file, code, msg string }
	check := func(line string, w *want) {
		t.Helper()
		got := matchOf(t, m, line)
		switch {
		case w == nil && got != nil:
			t.Fatalf("%q: matched %+v", line, got)
		case w != nil && (got == nil || got.file.s != w.file || got.code.s != w.code || got.message.s != w.msg):
			t.Fatalf("%q: %+v, want %+v", line, got, w)
		}
	}
	check("file1", nil)
	check("code1", nil)
	check("message:message1", &want{"file1", "code1", "message1"})
	check("message:message1-2", &want{"file1", "code1", "message1-2"}) // the loop keeps the running match
	check("abc", nil)                                                  // discarded
	check("file2", nil)
	check("code2", nil)
	check("message:message2", &want{"file2", "code2", "message2"})
	check("abc", nil)
	check("abc", nil)
	check("file3", nil)
	check("code3", nil)
	check("message:message3", &want{"file3", "code3", "message3"})
}

// IssueMatcherL0.Matcher_MultiplePatterns_NonLoop_*: no loop, a match clears
// the state.
func TestMatcherNonLoop(t *testing.T) {
	m := newMatcher(t, `{"problemMatcher":[{"owner":"m","pattern":[
		{"regexp":"^(.+)$","file":1},{"regexp":"^message:(.+)$","message":1}]}]}`)
	if matchOf(t, m, "file1") != nil {
		t.Fatal("early match")
	}
	if got := matchOf(t, m, "message:m1"); got == nil || got.file.s != "file1" || got.message.s != "m1" {
		t.Fatalf("%+v", got)
	}
	// "message:m2" is also a file line (first pattern matches every line).
	if got := matchOf(t, m, "message:m2"); got != nil {
		t.Fatalf("a non loop pattern matched without a running match: %+v", got)
	}
}

// IssueMatcherL0.Matcher_SinglePattern_ExtractsProperties, DefaultSeverity,
// DefaultFromPath.
func TestMatcherSinglePattern(t *testing.T) {
	m := newMatcher(t, `{"problemMatcher":[{"owner":"m","severity":"warning","fromPath":"web/package.json","pattern":[
		{"regexp":"^(.+):(\\d+):(\\d+) (\\w+)? (\\w+) (.+)$","file":1,"line":2,"column":3,"severity":4,"code":5,"message":6}]}]}`)
	got := matchOf(t, m, "file.js:5:10 error E1 something bad")
	if got == nil || got.file.s != "file.js" || got.line.s != "5" || got.column.s != "10" || got.severity.s != "error" || got.code.s != "E1" || got.message.s != "something bad" || got.fromPath.s != "web/package.json" {
		t.Fatalf("%+v", got)
	}
	// A group that took no part gives "" and the default severity applies.
	got = matchOf(t, m, "file.js:5:10  E1 msg")
	if got == nil || got.severity.s != "warning" {
		t.Fatalf("%+v", got)
	}
	if matchOf(t, m, "nothing") != nil {
		t.Fatal("matched")
	}
}

// ECMAScript semantics (\d is ASCII only) and lookarounds, which RE2 lacks.
func TestMatcherRegexFlavour(t *testing.T) {
	m := newMatcher(t, `{"problemMatcher":[{"owner":"m","pattern":[{"regexp":"^(?<=ERR )(\\d+) (?!ignored)(.+)$","line":1,"message":2}]}]}`)
	_ = m
	m = newMatcher(t, `{"problemMatcher":[{"owner":"m","pattern":[{"regexp":"^(\\d+) (.+)$","line":1,"message":2}]}]}`)
	if matchOf(t, m, "١٢ arabic digits") != nil {
		t.Error("\\d must be ASCII with RegexOptions.ECMAScript")
	}
	if matchOf(t, m, "12 ascii") == nil {
		t.Error("ASCII digits")
	}
	b := newMatcher(t, `{"problemMatcher":[{"owner":"m","pattern":[{"regexp":"^(a)\\1 (.+)$","message":2}]}]}`)
	if matchOf(t, b, "aa backreference") == nil {
		t.Error("backreference")
	}
}

// AC6b: a catastrophic pattern times out in about a second and does not stall
// the step.
func TestMatcherCatastrophicPatternTimesOut(t *testing.T) {
	m := newMatcher(t, `{"problemMatcher":[{"owner":"bad","pattern":[{"regexp":"^(a+)+$","message":1}]}]}`)
	start := time.Now()
	_, err := m.match(strings.Repeat("a", 30) + "!")
	took := time.Since(start)
	if err == nil || !strings.HasPrefix(err.Error(), "The Regex engine has timed out while trying to match a pattern to an input string.") {
		t.Fatalf("err %v after %v", err, took)
	}
	if took < 900*time.Millisecond || took > 3*time.Second {
		t.Errorf("timed out after %v, want about 1 s", took)
	}
}

func TestParseTimeSpan(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:0:0.01": 10 * time.Millisecond, "00:00:05": 5 * time.Second, "0:2": 2 * time.Minute, "1.02:03:04": 26*time.Hour + 3*time.Minute + 4*time.Second,
		"1": 24 * time.Hour,
	} {
		if got, ok := parseTimeSpan(in); !ok || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "abc", "1:2:3:4", "25:00:00", "0:61"} {
		if _, ok := parseTimeSpan(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if d := matcherTimeout(env(nil)); d != time.Second {
		t.Error(d)
	}
	if d := matcherTimeout(env(map[string]string{matcherTimeoutKey: "00:00:05"})); d != 5*time.Second {
		t.Error(d)
	}
	if d := matcherTimeout(env(map[string]string{matcherTimeoutKey: "-1"})); d != time.Second {
		t.Error(d)
	}
}
