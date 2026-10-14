// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// resultRecorder is a recorder that also keeps StepResults.
type resultRecorder struct {
	*recorder
	rmu     sync.Mutex
	results map[int]StepResults
}

func newResultRecorder() *resultRecorder {
	return &resultRecorder{recorder: newRecorder(), results: map[int]StepResults{}}
}

func (r *resultRecorder) StepResults(n int, res StepResults) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.results[n] = res
}

func runResults(t *testing.T, yaml string, tweak func(*Config)) (*resultRecorder, Result) {
	t.Helper()
	rr := newResultRecorder()
	// The recorder is wrapped: Reporter methods come from recorder, StepResults
	// from resultRecorder.
	_, res := runLocal(t, yaml, "j", func(c *Config) {
		c.Reporter = rr
		c.SendAnnotations, c.SendSummaries = true, true
		if tweak != nil {
			tweak(c)
		}
	})
	return rr, res
}

// Cases of ActionCommandManagerL0.IssueCommandInvalidColumns (actions/runner,
// MIT), as a table: the properties after ValidateLinesAndColumns.
func TestValidateLinesAndColumns(t *testing.T) {
	reg := func(string) bool { return true }
	for _, tc := range []struct {
		name, cmd string
		want      map[string]string
	}{
		{"columns when lines differ", "::warning line=1,endLine=2,col=1,endColumn=2::m", map[string]string{"line": "1", "endline": "2"}},
		{"no lines with columns", "::warning col=1,endColumn=2::m", map[string]string{}},
		{"no line with endLine", "::warning endLine=1::m", map[string]string{"line": "1", "endline": "1"}},
		{"no column with endColumn", "::warning line=1,endColumn=2::m", map[string]string{"line": "1", "col": "2", "endcolumn": "2"}},
		{"empty strings", "::warning line=,endLine=3::m", map[string]string{"line": "3", "endline": "3"}},
		{"end line before line", "::warning line=4,endLine=3::m", map[string]string{}},
		{"end column before column", "::warning line=1,endLine=1,col=3,endColumn=2::m", map[string]string{"line": "1", "endline": "1"}},
		{"valid", "::warning line=1,endLine=1,col=1,endColumn=2::m", map[string]string{"line": "1", "endline": "1", "col": "1", "endcolumn": "2"}},
		{"backwards compatibility", "::warning line=1,col=1,file=test.txt::m", map[string]string{"line": "1", "col": "1", "file": "test.txt"}},
	} {
		c, ok := parseCommandV2(tc.cmd, reg)
		if !ok {
			t.Fatalf("%s: not parsed", tc.name)
		}
		props := map[string]string{}
		for k, v := range c.Props {
			props[k] = v
		}
		validateLinesAndColumns(props, "warning", func(string) {})
		if fmt.Sprint(props) != fmt.Sprint(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, props, tc.want)
		}
	}
}

func TestAnnotationsFromCommands(t *testing.T) {
	needTools(t, "bash")
	rr, res := runResults(t, wfHead+`    steps:
      - run: |
          echo "::warning title=T,file=src/a.go,line=3,endLine=5,col=1,endColumn=2::careful"
          echo "::error::broken%0Aline two"
          echo "::notice::fyi"
`, nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	got := rr.results[2].Annotations
	want := []Annotation{
		{Level: LevelWarning, Title: "T", Message: "careful", File: "src/a.go", StartLine: 3, EndLine: 5},
		// No file and no line: the line of the ##[error] and ##[notice] lines in
		// the step log (header: group, 3 script lines, shell, endgroup = 6 lines;
		// the warning is line 7, the error 8 and its second line 9, the notice 10).
		{Level: LevelFailure, Message: "broken\nline two", StartLine: 8, EndLine: 8},
		{Level: LevelNotice, Message: "fyi", StartLine: 10, EndLine: 10},
	}
	// col and endColumn are dropped because the lines differ.
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("annotations\n got %+v\nwant %+v", got, want)
	}
	log := logOf(rr.step(t, "Run echo \"::warning title=T,file=src/a.go,line=3,endLine=5,col=1,endColumn=2::careful\""))
	for _, w := range []string{"##[warning]careful", "##[error]broken\nline two", "##[notice]fyi"} {
		if !strings.Contains(log, w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
}

// ExecutionContext.cs:144-146: ten per level per step, messages of 4096
// characters at most; the rest only reaches the log.
func TestAnnotationLimits(t *testing.T) {
	needTools(t, "bash")
	rr, _ := runResults(t, wfHead+`    steps:
      - name: many
        run: |
          for i in $(seq 1 12); do
            echo "::warning::w$i"; echo "::error::e$i"; echo "::notice::n$i"
          done
      - name: long
        run: |
          printf '::warning::%s\n' "$(head -c 5000 /dev/zero | tr '\0' x)"
`, nil)
	count := map[AnnotationLevel]int{}
	for _, a := range rr.results[2].Annotations {
		count[a.Level]++
	}
	if count[LevelWarning] != 10 || count[LevelFailure] != 10 || count[LevelNotice] != 10 {
		t.Errorf("counts %v", count)
	}
	if a := rr.results[2].Annotations; a[len(a)-1].Message == "w12" {
		t.Error("the eleventh was kept")
	}
	log := logOf(rr.step(t, "many"))
	if !strings.Contains(log, "##[warning]w12") || !strings.Contains(log, "##[error]e12") {
		t.Error("annotations over the limit are still logged")
	}
	long := rr.results[3].Annotations
	if len(long) != 1 || len(long[0].Message) != 4096 {
		t.Errorf("long message: %d annotations, len %d", len(long), len(long[0].Message))
	}
	if l := logOf(rr.step(t, "long")); !strings.Contains(l, "##[warning]"+strings.Repeat("x", 4096)+"\n") && !strings.HasSuffix(l, "##[warning]"+strings.Repeat("x", 4096)) {
		t.Error("the log has the message cut to 4096 characters")
	}
}

// Every ##[error] the runner itself writes is an issue and an annotation.
func TestRunnerErrorsAreAnnotations(t *testing.T) {
	needTools(t, "bash")
	rr, res := runResults(t, wfHead+`    steps:
      - run: exit 3
`, nil)
	if res.Conclusion != ConclusionFailure {
		t.Fatal(res)
	}
	a := rr.results[2].Annotations
	if len(a) != 1 || a[0].Level != LevelFailure || a[0].Message != "Process completed with exit code 3." {
		t.Errorf("%+v", a)
	}
}

func TestAnnotationsAreMaskedAndOffWithoutTheFeature(t *testing.T) {
	needTools(t, "bash")
	rr, _ := runResults(t, wfHead+`    steps:
      - run: |
          echo "::add-mask::hunter2hunter2"
          echo "::warning title=hunter2hunter2::pw hunter2hunter2"
`, nil)
	a := rr.results[2].Annotations
	if len(a) != 1 || a[0].Message != "pw ***" || a[0].Title != "***" {
		t.Errorf("%+v", a)
	}
	rr2, _ := runResults(t, wfHead+"    steps:\n      - run: echo '::warning::w'\n", func(c *Config) { c.SendAnnotations = false })
	if len(rr2.results[2].Annotations) != 0 {
		t.Error("annotations without the feature")
	}
}

func TestAddMaskCommand(t *testing.T) {
	needTools(t, "bash")
	rec, res := runLocal(t, wfHead+`    steps:
      - name: step one
        run: |
          echo "::add-mask::abc%0Ddef%0Aghi%0D%0Ajkl"
          echo "::add-mask:: %0D  %0A   %0D%0A    %0D"
          echo "::add-mask::"
          echo "def ghi jkl abc"
          echo "::echo::on"
          echo "::add-mask::echoed-value"
          echo "echoed-value"
      - name: step two
        run: echo "still masked jkl"
`, "j", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	log := logOf(rec.step(t, "step one"))
	_, body, _ := strings.Cut(log, "##[endgroup]\n")
	for _, w := range []string{"*** *** *** ***", "##[warning]Can't add secret mask for empty string in ##[add-mask] command.", "::add-mask::***", "\n***\n"} {
		if !strings.Contains(body+"\n", w) {
			t.Errorf("log lacks %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, "echoed-value") && !strings.Contains(body, "::echo::on") {
		t.Errorf("%s", body)
	}
	if got := logOf(rec.step(t, "step two")); !strings.HasSuffix(got, "still masked ***") {
		t.Errorf("%s", got)
	}
}

func TestSaveStateInRunStep(t *testing.T) {
	needTools(t, "bash")
	var j *job
	rec, res := runLocal(t, wfHead+`    steps:
      - run: |
          echo "::save-state name=Foo::bar"
          echo "baz=qux" >> "$GITHUB_STATE"
          echo "::save-state::x"
`, "j", func(c *Config) { _ = j })
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("a save-state without a name fails the step: %+v", res)
	}
	log := logOf(rec.steps[1])
	for _, w := range []string{"##[warning]The `save-state` command is deprecated", "##[error]Required field 'name' is missing in ##[save-state] command."} {
		if !strings.Contains(log, w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
}

// IssueExtensions.ToAnnotation (Sdk/RSWebApi/Contracts/IssueExtensions.cs:25-29):
// an issue without file and line takes the number of its own line in the step
// log (logFileLineNumber, ExecutionContext.cs:855) as start and end line.
func TestAnnotationOfAPathlessIssueIsItsLogLine(t *testing.T) {
	j := &job{}
	j.masker.Store(newMasker())
	for _, tc := range []struct {
		name    string
		props   map[string]string
		logLine int
		want    [2]int32
	}{
		{"no props", nil, 103, [2]int32{103, 103}},
		{"not logged", nil, 0, [2]int32{0, 0}},
		{"blank file", map[string]string{"file": "  "}, 5, [2]int32{5, 5}},
		{"zero line", map[string]string{"line": "0"}, 5, [2]int32{5, 5}},
		{"line not a number", map[string]string{"line": "x"}, 5, [2]int32{5, 5}},
		{"a file", map[string]string{"file": "a.go"}, 5, [2]int32{0, 0}},
		{"a line", map[string]string{"line": "2"}, 5, [2]int32{2, 2}},
		{"a line range", map[string]string{"line": "2", "endline": "4"}, 5, [2]int32{2, 4}},
		{"negative line, no file", map[string]string{"line": "-1"}, 5, [2]int32{0, 0}},
	} {
		a := j.annotationOf(issue{Level: LevelWarning, Props: tc.props}, "m", tc.logLine)
		wantFile := tc.props["file"]
		if strings.TrimSpace(wantFile) == "" {
			wantFile = ""
		}
		if got := [2]int32{a.StartLine, a.EndLine}; got != tc.want || a.File != wantFile {
			t.Errorf("%s: %+v, want lines %v", tc.name, a, tc.want)
		}
	}
}

// ann-*: a plain ::warning:: gets the number of its line in the log of the step,
// the header counted (recorded: 103 for the exit code error closing a 103 line
// log). Here: group, script (4 lines), shell, endgroup, then the output.
func TestPlainWarningLineIsItsLogLine(t *testing.T) {
	needTools(t, "bash")
	rr, _ := runResults(t, wfHead+`    steps:
      - name: s
        run: |
          echo out
          echo "::warning::x"
          echo "::warning::x"
          exit 3
`, nil)
	log := rr.step(t, "s").Log
	var want []int32
	for i, l := range log {
		if strings.HasPrefix(l, "##[warning]") || strings.HasPrefix(l, "##[error]") {
			want = append(want, int32(i+1))
		}
	}
	if fmt.Sprint(want) != "[9 10 11]" {
		t.Fatalf("log layout changed: %v\n%s", want, strings.Join(log, "\n"))
	}
	var got []int32
	for _, a := range rr.results[2].Annotations {
		if a.File != "" || a.StartLine != a.EndLine {
			t.Errorf("%+v", a)
		}
		got = append(got, a.StartLine)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("start lines %v, log lines %v\n%s", got, want, strings.Join(log, "\n"))
	}
}

// Mirror of the recording match-catastrophic (GitHub): the annotations of a
// matcher with no file are at the line of their ##[warning] line, 10, 13, 16,
// 18 and 20 in the steps of the corpus script (the removal warning at 14 and,
// in the next step, 7). The step logs are laid out as the recorded ones.
func TestMatcherAnnotationsWithoutFileHaveTheirLogLine(t *testing.T) {
	needTools(t, "bash")
	rr, res := runResults(t, wfHead+`    steps:
      - name: workspace as a repository
        run: |
          mkdir -p src .github
          cat > .github/bad.json <<'JSON'
          {"problemMatcher":[{"owner":"catastrophic","pattern":[{"regexp":"^((a+)+)$","message":1}]}]}
          JSON
          cat > .github/good.json <<'JSON'
          {"problemMatcher":[{"owner":"good","severity":"warning","pattern":[{"regexp":"^good: (.+)$","message":1}]}]}
          JSON
          printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!\n' > .github/long-line.txt
      - name: catastrophic matcher
        run: |
          echo "::add-matcher::.github/bad.json"
          echo "::add-matcher::.github/good.json"
          echo "good: before the long line"
          cat .github/long-line.txt
          echo "good: after the long line"
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!"
          echo "good: after a second long line"
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
          echo "good: matcher still alive"
      - name: the matcher stays removed in the next step
        run: |
          echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!"
          echo "aaa"
          echo "good: still alive"
`, nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	lines := func(num int) (out []string) {
		for _, a := range rr.results[num].Annotations {
			if a.File != "" || a.StartLine != a.EndLine {
				t.Errorf("step %d: %+v", num, a)
			}
			m, _, _ := strings.Cut(a.Message, ".")
			out = append(out, fmt.Sprintf("%d:%s", a.StartLine, m))
		}
		return out
	}
	// The first step has no annotation; the removal message is line 14.
	want3 := "[13:before the long line 14:Removing issue matcher 'catastrophic'" +
		" 16:after the long line 18:after a second long line 20:matcher still alive]"
	if got := fmt.Sprint(lines(3)); got != want3 {
		t.Errorf("catastrophic matcher: %v\nwant %v\n%s", got, want3, logOf(rr.step(t, "catastrophic matcher")))
	}
	want4 := "[7:Removing issue matcher 'catastrophic' 10:still alive]"
	if got := fmt.Sprint(lines(4)); got != want4 {
		t.Errorf("next step: %v\nwant %v\n%s", got, want4, logOf(rr.step(t, "the matcher stays removed in the next step")))
	}
}
