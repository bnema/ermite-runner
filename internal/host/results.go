// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// Features of the server that the host uses (GetVersionResponse.features).
const (
	FeatureAnnotations = "ci.annotations"
	FeatureSummaries   = "ci.summaries"
)

// Limits of Annotation and UploadSummaryRequest (runner.proto). The executor
// keeps within them; the guest is untrusted code, so the host enforces them
// again, by dropping or cutting what is over rather than failing the job: the
// server rejects a whole call that breaks one.
const (
	maxAnnotationsPerLevel = 10
	maxAnnotationMessage   = 4096
	maxAnnotationTitle     = 255
	maxAnnotationFile      = 1024
	maxSummaryBytes        = 1 << 20
)

// ServerFeatures asks the server for its features. It returns nil when it
// cannot be reached: the optional results are then not sent.
type ServerFeatures func(ctx context.Context) ([]string, error)

// jobFeatures is what the server accepts for this job.
type jobFeatures struct{ annotations, summaries bool }

func featuresOf(list []string) jobFeatures {
	var f jobFeatures
	for _, x := range list {
		switch x {
		case FeatureAnnotations:
			f.annotations = true
		case FeatureSummaries:
			f.summaries = true
		}
	}
	return f
}

// loadFeatures reads the features of the server for this job (GetVersion).
func (j *jobRun) loadFeatures(ctx context.Context, rt *Runner) jobFeatures {
	if rt.Features == nil {
		return jobFeatures{}
	}
	cctx, cancel := context.WithTimeout(ctx, j.t.CallTimeout)
	defer cancel()
	list, err := rt.Features(cctx)
	if err != nil {
		j.log.Warn().Err(err).Msg("GetVersion failed: annotations and summaries are not sent for this job")
		return jobFeatures{}
	}
	return featuresOf(list)
}

func levelOf(l execpb.AnnotationLevel) ermite.AnnotationLevel {
	switch l {
	case execpb.AnnotationLevel_ANNOTATION_LEVEL_NOTICE:
		return ermite.AnnotationLevel_ANNOTATION_LEVEL_NOTICE
	case execpb.AnnotationLevel_ANNOTATION_LEVEL_WARNING:
		return ermite.AnnotationLevel_ANNOTATION_LEVEL_WARNING
	case execpb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE:
		return ermite.AnnotationLevel_ANNOTATION_LEVEL_FAILURE
	}
	return ermite.AnnotationLevel_ANNOTATION_LEVEL_UNSPECIFIED
}

// cutRunes bounds s to n characters (code points), after making it valid UTF-8.
// A string that is cut is copied: a substring would keep the whole allocation
// of what the guest sent (up to a frame of 2 MiB) alive for as long as the
// short string is, and a guest can open 1,000 steps.
func cutRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return strings.Clone(s[:k])
		}
		i++
	}
	return s
}

// annotationsFrom turns the annotations of a step into the ones of the
// contract: the ones that break a rule of the contract are dropped (no level,
// no message, a message over 4096 characters, more than ten of a level), the
// numbers are made consistent (negative values and an end before the start
// become "not given").
func annotationsFrom(step int32, in []*execpb.Annotation) []*ermite.Annotation {
	var out []*ermite.Annotation
	count := map[ermite.AnnotationLevel]int{}
	for _, a := range in {
		lvl := levelOf(a.GetLevel())
		msg := strings.ToValidUTF8(a.GetMessage(), "\ufffd")
		if lvl == ermite.AnnotationLevel_ANNOTATION_LEVEL_UNSPECIFIED || msg == "" || utf8.RuneCountInString(msg) > maxAnnotationMessage {
			continue
		}
		if count[lvl] >= maxAnnotationsPerLevel {
			continue
		}
		count[lvl]++
		n := func(v int32) int32 { return max(v, 0) }
		o := &ermite.Annotation{
			Level: lvl, Title: cutRunes(a.GetTitle(), maxAnnotationTitle), Message: msg, StepNumber: step,
			StartLine: n(a.GetStartLine()), EndLine: n(a.GetEndLine()), StartColumn: n(a.GetStartColumn()), EndColumn: n(a.GetEndColumn()),
		}
		if f := strings.ToValidUTF8(a.GetFile(), "\ufffd"); utf8.RuneCountInString(f) <= maxAnnotationFile {
			o.File = f
		}
		if o.EndLine != 0 && o.EndLine < o.StartLine {
			o.EndLine = 0
		}
		out = append(out, o)
	}
	return out
}

// stepAnnotations records the annotations of a step; they are sent with its
// completion (UpdateJob replaces the annotations of the step by this list).
func (j *jobRun) stepAnnotations(a *execpb.StepAnnotations) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.violation != "" || !j.features.annotations {
		return
	}
	if why := j.resultFrameProblemLocked("annotations", a.Step, j.annSeen); why != "" {
		j.mu.Unlock()
		j.violate("%s", why)
		j.mu.Lock()
		return
	}
	if j.annLists == nil {
		j.annLists = map[int32][]*ermite.Annotation{}
	}
	j.annSeen[a.Step] = true
	list := annotationsFrom(a.Step, a.Annotations)
	// The list stays in memory until the call that carries it is answered,
	// first here while its step runs, then in the outbox: all of it counts in
	// the one budget of the outbox, from now (ReserveAnnotations) to the answer
	// (sendStep). When there is no room (the server has been failing for long)
	// the list is dropped, and the log says so.
	n := annotationBytes(list)
	if !j.outbox.ReserveAnnotations(n) {
		j.log.Warn().Int32("step", a.Step).Int("bytes", n).Msg("too much is waiting to be sent to the server: the annotations of this step are dropped")
		return
	}
	j.annLists[a.Step] = list
}

// annotationsBytes is the size of the lists of a call.
func annotationsBytes(m map[int32][]*ermite.Annotation) int {
	n := 0
	for _, l := range m {
		n += annotationBytes(l)
	}
	return n
}

func annotationBytes(l []*ermite.Annotation) int {
	n := 0
	for _, a := range l {
		n += len(a.Title) + len(a.Message) + len(a.File) + 64
	}
	return n
}

// resultFrameProblemLocked checks a frame that carries a result of a step
// (annotations, summary). The executor sends each at most once per step, while
// the step runs, just before it completes (the runner reads GITHUB_STEP_SUMMARY
// once, after the step, and keeps the issues of a step in one list): anything
// else is the guest misbehaving, and would let it queue unbounded data.
func (j *jobRun) resultFrameProblemLocked(what string, step int32, seen map[int32]bool) string {
	u, ok := j.steps[step]
	switch {
	case !ok:
		return fmt.Sprintf("%s for step %d, which has not started", what, step)
	case u.State == ermite.StepState_STEP_STATE_COMPLETED:
		return fmt.Sprintf("%s for step %d, which is completed", what, step)
	case seen[step]:
		return fmt.Sprintf("%s for step %d, which already sent them", what, step)
	}
	return ""
}

// takeAnnotations returns the list recorded for step n, once.
func (j *jobRun) takeAnnotations(n int32) map[int32][]*ermite.Annotation {
	l, ok := j.annLists[n]
	if !ok {
		return nil
	}
	delete(j.annLists, n)
	return map[int32][]*ermite.Annotation{n: l}
}

// stepSummary queues UploadSummary for a step that wrote GITHUB_STEP_SUMMARY.
// The bytes count in the bounded memory of the outbox, like log lines: the call
// blocks while too much is queued.
func (j *jobRun) stepSummary(s *execpb.StepSummary) {
	j.mu.Lock()
	if j.violation != "" || !j.features.summaries {
		j.mu.Unlock()
		return
	}
	if why := j.resultFrameProblemLocked("a summary", s.Step, j.sumSeen); why != "" {
		j.mu.Unlock()
		j.violate("%s", why)
		return
	}
	j.sumSeen[s.Step] = true
	j.mu.Unlock()
	md := strings.ToValidUTF8(s.Markdown, "\ufffd")
	if len(md) > maxSummaryBytes {
		j.log.Warn().Int32("step", s.Step).Int("bytes", len(md)).Msg("a step summary over 1 MiB is dropped")
		return
	}
	j.outbox.Summary(s.Step, md)
}

// uploadSummaryClient is what the outbox needs for summaries.
var _ interface {
	UploadSummary(context.Context, *connect.Request[ermite.UploadSummaryRequest]) (*connect.Response[ermite.UploadSummaryResponse], error)
} = jobClient(nil)

// allPendingAnnotations are the annotations for CompleteJob: the lists no
// UpdateJob has carried, and those of steps that never completed (the
// executor was stopped), in step order.
func (j *jobRun) allPendingAnnotations() []*ermite.Annotation {
	m := map[int32][]*ermite.Annotation{}
	for _, a := range j.outbox.PendingAnnotations() {
		m[a.StepNumber] = append(m[a.StepNumber], a)
	}
	j.mu.Lock()
	for n, l := range j.annLists {
		m[n] = l
	}
	j.mu.Unlock()
	return flattenAnnotations(m)
}
