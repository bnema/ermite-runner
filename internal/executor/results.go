// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

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

// HostPath is TranslateToHostPath of the job container.
func (s *stepRun) HostPath(p string) string {
	if c := s.j.stepContainer(); c != nil {
		return c.translateToHost(p)
	}
	return p
}

func (j *job) githubValue(k string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state.GitHub(k)
}

// AddMask is SecretMasker.AddValue.
func (s *stepRun) AddMask(v string) { s.j.addMask(v) }

// addMask registers values with the masker of the job, with their encodings
// (SecretMasker.AddValue). It is the one way to add a secret after the job
// started (add-mask, a stop-commands token, the token of an action archive): it
// only adds, the masker is never replaced, so the text that other goroutines
// mask meanwhile loses no value.
func (j *job) addMask(values ...string) {
	m := j.masker.Load()
	for _, v := range values {
		m.add(v)
	}
}

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

// intraState is IntraActionState: a Dictionary<string, string> with an
// OrdinalIgnoreCase comparer. A name that is already there keeps the spelling
// it was first saved with and takes the new value (the indexer of Dictionary
// replaces the value, not the key), and the entries enumerate in the order
// they were added: ActionRunner.cs:255-258 sets STATE_<key> from that
// enumeration, so the spelling of the first save-state is the one the action
// sees.
type intraState struct {
	keys []string // spelling and order of first insertion
	vals map[string]string
}

func (st *intraState) set(name, value string) {
	if st.vals == nil {
		st.vals = map[string]string{}
	}
	lo := strings.ToLower(name)
	if _, ok := st.vals[lo]; !ok {
		st.keys = append(st.keys, name)
	}
	st.vals[lo] = value
}

// Pair is one saved name (as first spelled) and its value.
func (st *intraState) pairs() []Pair {
	out := make([]Pair, 0, len(st.keys))
	for _, k := range st.keys {
		out = append(out, Pair{k, st.vals[strings.ToLower(k)]})
	}
	return out
}

func (j *job) saveState(key, name, value string) {
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if j.states == nil {
		j.states = map[string]*intraState{}
	}
	st := j.states[key]
	if st == nil {
		st = &intraState{}
		j.states[key] = st
	}
	st.set(name, value)
}

// intraActionState returns the state saved under key (the number of the step,
// or the stateKey of its scope) as the runner enumerates it: names as first
// spelled, in the order they were first saved. Phase F sets STATE_<name> from
// it for the pre, main and post stages of the action.
func (j *job) intraActionState(key string) []Pair {
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if st := j.states[key]; st != nil {
		return st.pairs()
	}
	return nil
}

// Summary receives GITHUB_STEP_SUMMARY after the step (see processStepSummary).
//
// The server keeps one summary per step of the workflow. The steps inside a
// composite action each have a summary file in the runner (uploaded under the
// id of the embedded step); here they are concatenated, in order, under the
// step that uses the action.
func (s *stepRun) Summary(markdown string) {
	j := s.j
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if j.summaries == nil {
		j.summaries = map[int]string{}
	}
	if s.scope.embedded {
		markdown = j.summaries[s.num] + markdown
		if len(markdown) > stepSummaryLimit {
			return // over the limit of the contract: the first ones stay
		}
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

// ---- hook points for steps that contain steps (phase F)
//
// A step of the workflow is run by runStep; a composite action runs its inner
// steps itself. What an inner step shares with the step that contains it is
// named by a stepScope, and what its lifecycle needs from the commands is:
//
//   - embeddedScope(outer, stateKey): the scope of an inner step. Its issues
//     go to the collector of the outer step (counted against its limits when
//     the outer step completes) and its summary is appended to the outer
//     step's; its save-state goes to the state of stateKey.
//   - newOutputManager(s, cancel): one per process of a step (inner or not),
//     with the cancellation channel the process gets; call dispose when the
//     process is over.
//   - armStepTimeout / stepTimedOut: the timeout of a step, inner or not,
//     around the run of its body (see runStep).
//   - addMask: add a secret.

// embeddedScope is the scope of a step inside the step outer.
func (j *job) embeddedScope(outer *stepRun, stateKey string) stepScope {
	return stepScope{issues: j.collector(outer.num), embedded: true, stateKey: stateKey}
}

// armStepTimeout starts the timeout-minutes timer of the step (StepsRunner.
// RunStepAsync, :280-296). When it fires, the step is marked timed out and
// cancel is called (the same cancellation as a job cancel: SIGINT, SIGTERM,
// SIGKILL). It returns the function that stops the timer and the number of
// minutes, 0 for none.
func (j *job) armStepTimeout(s *stepRun, cancel func()) (stop func(), minutes int) {
	minutes = j.stepTimeout(s)
	if minutes <= 0 {
		return func() {}, 0
	}
	t := time.AfterFunc(time.Duration(minutes)*time.Minute+j.cfg.StepTimeoutSkew, func() {
		s.timedOut.Store(true)
		cancel()
	})
	return func() { t.Stop() }, minutes
}

// stepTimedOut turns the result of a step whose own timer fired (and not the
// job's cancellation) into a failure with the runner's message
// (StepsRunner.cs:323-330). Other results pass through.
func (j *job) stepTimedOut(s *stepRun, r stepResult, minutes int) stepResult {
	if r == resCanceled && s.timedOut.Load() && !j.cancelRequested() {
		j.errorf(s.num, "%s", stepTimeoutMessage(s.name, minutes))
		return resFailed
	}
	return r
}
