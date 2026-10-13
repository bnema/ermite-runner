// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"bnema.dev/ermite-actions/template"
)

// What a step leaves besides its log: annotations, summary, outcome, saved
// state. The pieces that the steps of phase F share (embedded steps of a
// composite action, state of an action) are the fields marked below.

// stepScope is what a step that runs inside another one (a step of a
// composite action) shares with the step of the workflow that uses the action.
// The zero value is a step of the workflow.
type stepScope struct {
	// issues, when set, is the collector of the outer step: the issues of an
	// embedded step count against its limits (ExecutionContext.AddIssue,
	// :838). Embedded is true then.
	issues   *issueCollector
	embedded bool
	// stateKey names the intra-action state the step saves to (SaveState). Empty
	// means the step's own number.
	stateKey string
}

// AddIssue is ExecutionContext.AddIssue for the step.
func (s *stepRun) AddIssue(is issue) { s.j.addIssueFor(s, is) }

func (j *job) addIssueFor(s *stepRun, is issue) {
	j.addIssue(s.num, s.scope.issues, s.scope.embedded, is)
}

// Workspace is github.workspace.
func (s *stepRun) Workspace() string { return s.j.githubValue("workspace") }

func (j *job) githubValue(k string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state.GitHub(k)
}

// AddMask is SecretMasker.AddValue.
func (s *stepRun) AddMask(v string) { s.j.masker.Load().add(v) }

// Matchers is the root of the problem matchers of the job.
func (s *stepRun) Matchers() *matcherRoot { return s.j.matchers }

// maxMatcherFile bounds a problem matcher file.
const maxMatcherFile = 1 << 20

// ReadMatcherFile reads a problem matcher file.
func (s *stepRun) ReadMatcherFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxMatcherFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxMatcherFile {
		return nil, ghErrorf("The problem matcher file is larger than %d bytes", maxMatcherFile)
	}
	return b, nil
}

func (j *job) getenv(k string) string {
	if j.cfg.Getenv != nil {
		return j.cfg.Getenv(k)
	}
	return os.Getenv(k)
}

// SaveState is the state of an action step (save-state and GITHUB_STATE): kept
// per step so that the post step of the action can read it as STATE_<name>
// (ActionCommandManager.cs:384-398). A step that is not an action only fills
// it; the runner does the same.
func (s *stepRun) SaveState(name, value string) { s.j.saveState(s.stateKey(), name, value) }

func (s *stepRun) stateKey() string {
	if s.scope.stateKey != "" {
		return s.scope.stateKey
	}
	return fmt.Sprint(s.num)
}

func (j *job) saveState(key, name, value string) {
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if j.states == nil {
		j.states = map[string]map[string]string{}
	}
	m := j.states[key]
	if m == nil {
		m = map[string]string{}
		j.states[key] = m
	}
	// IntraActionState is a Dictionary with an OrdinalIgnoreCase comparer.
	m[strings.ToLower(name)] = value
}

// IntraActionState returns the state saved under key (the number of the step,
// or the stateKey of its scope), with lower-case names.
func (j *job) intraActionState(key string) map[string]string {
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	out := map[string]string{}
	for k, v := range j.states[key] {
		out[k] = v
	}
	return out
}

// Summary receives GITHUB_STEP_SUMMARY after the step (see processStepSummary).
func (s *stepRun) Summary(markdown string) {
	j := s.j
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if j.summaries == nil {
		j.summaries = map[int]string{}
	}
	j.summaries[s.num] = markdown
}

// MaskText masks text with the secrets known now.
func (s *stepRun) MaskText(t string) string { return s.j.mask(t) }

// summarySink is implemented by the Sink of a step that keeps summaries.
type summarySink interface{ Summary(markdown string) }

// recordOutcome keeps steps.<id>.outcome of a step for StepResults.
func (j *job) recordOutcome(num int, c Conclusion) {
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if j.outcomes == nil {
		j.outcomes = map[int]Conclusion{}
	}
	j.outcomes[num] = c
}

// flushStepResults hands the results of step n to the reporter, before the step
// completes. Called with the serialisation of the reporter not held.
func (j *job) flushStepResults(next Reporter, n int, c Conclusion) {
	rr, ok := next.(StepResultReporter)
	if !ok {
		return
	}
	j.issuesMu.Lock()
	outcome, has := j.outcomes[n]
	summary, hasSummary := j.summaries[n]
	delete(j.summaries, n)
	coll := j.collectors[n]
	j.issuesMu.Unlock()
	if !has {
		outcome = c
	}
	r := StepResults{Outcome: outcome}
	if coll != nil && j.cfg.SendAnnotations {
		r.Annotations = coll.result()
	}
	if hasSummary && j.cfg.SendSummaries {
		r.Summary, r.HasSummary = summary, true
	}
	rr.StepResults(n, r)
}

// logCanceled logs the cancellation of the process of a step
// (StepsRunner.cs:333-337 logs the exception of a cancelled step), unless the
// step timed out: then StepsRunner logs the timeout message instead.
func (s *stepRun) logCanceled() {
	if s.timedOut.Load() && !s.j.cancelRequested() {
		return
	}
	s.j.errorf(s.num, "The operation was canceled.")
}

func (j *job) cancelRequested() bool {
	select {
	case <-j.cancel.Done():
		return true
	default:
		return false
	}
}

// ---- timeout-minutes

// timedOutFlag is the marker of a step that reached its timeout.
type timedOutFlag = atomic.Bool

// stepTimeoutMessage is StepsRunner.cs:328.
func stepTimeoutMessage(name string, minutes int) string {
	return fmt.Sprintf("The action '%s' has timed out after %d minutes.", name, minutes)
}

// ---- step summary

// summaryContent is what CreateStepSummaryCommand uploads: the lines of the
// file, each masked, each ended by a line feed (StreamReader.ReadLine and
// StreamWriter.WriteLine; ProcessCommand, FileCommandManager.cs:253-261).
func summaryContent(text string, mask func(string) string) string {
	var sb strings.Builder
	for _, l := range splitLines(text) {
		sb.WriteString(mask(l))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// maxStepTimeoutMinutes bounds the timeout so that the duration fits.
const maxStepTimeoutMinutes = 1 << 24

// stepTimeout is PipelineTemplateEvaluator.EvaluateStepTimeout and
// ConvertToStepTimeout: the number is truncated to an int; zero, negative and
// absent values mean no timeout. An expression that fails logs the runner's
// two errors and the step runs without timeout (StepsRunner.cs:266-273).
func (j *job) stepTimeout(s *stepRun) int {
	tok := s.st.TimeoutMinutes
	if tok == nil {
		return 0
	}
	j.mu.Lock()
	f, err := template.EvaluateNumber(context.Background(), tok, j.evalEnv(s.env))
	j.mu.Unlock()
	if err != nil {
		j.errorf(s.num, "An error occurred when attempting to determine the step timeout.")
		j.errorf(s.num, "%s", templateNotValid(err, true))
		return 0
	}
	switch {
	case f != f || f < 1:
		return 0
	case f > maxStepTimeoutMinutes:
		return maxStepTimeoutMinutes
	}
	return int(f)
}
