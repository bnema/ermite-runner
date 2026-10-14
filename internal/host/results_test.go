// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func annFrame(c *execstream.Conn, step int, as ...*execpb.Annotation) {
	send(c, &execpb.Frame{Body: &execpb.Frame_StepAnnotations{StepAnnotations: &execpb.StepAnnotations{Step: int32(step), Annotations: as}}})
}

func sumFrame(c *execstream.Conn, step int, md string) {
	send(c, &execpb.Frame{Body: &execpb.Frame_StepSummary{StepSummary: &execpb.StepSummary{Step: int32(step), Markdown: md}}})
}

func stepDoneOutcome(c *execstream.Conn, n int, name string, concl, outcome execpb.StepConclusion) {
	send(c, &execpb.Frame{Body: &execpb.Frame_StepCompleted{StepCompleted: &execpb.StepCompleted{Number: int32(n), Name: name, Conclusion: concl, Outcome: outcome, UnixNano: time.Now().UnixNano()}}})
}

const (
	lWarn = execpb.AnnotationLevel_ANNOTATION_LEVEL_WARNING
	lFail = execpb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE
)

// resultsScript runs a job whose step 2 has annotations, a summary and an
// outcome that differs from its conclusion (continue-on-error).
func resultsScript(extra func(c *execstream.Conn)) script {
	return func(_ *testing.T, c *execstream.Conn, run *execpb.Run, _ <-chan *execpb.Cancel) {
		seenRun.Store(run)
		stepStart(c, 1, "Set up job")
		stepDone(c, 1, "Set up job", cOK)
		stepStart(c, 2, "Run it")
		logLineFrame(c, 2, "hi", false)
		annFrame(c, 2,
			&execpb.Annotation{Level: lWarn, Title: "T", Message: "careful", File: "a.go", StartLine: 3, EndLine: 4},
			&execpb.Annotation{Level: lFail, Message: "broken"})
		sumFrame(c, 2, "# Summary\n")
		stepDoneOutcome(c, 2, "Run it", cOK, cFail)
		if extra != nil {
			extra(c)
		}
		stepStart(c, 3, "Complete job")
		stepDone(c, 3, "Complete job", cOK)
		jobDone(c, cOK, nil, false)
	}
}

// seenRun is the last Run frame the scripted executor received.
var seenRun atomic.Pointer[execpb.Run]

func runResultsJob(t *testing.T, sc script, tweak func(*harness)) *harness {
	t.Helper()
	h := newHarness(t, sc)
	if tweak != nil {
		tweak(h)
	}
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	stop := h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAnnotationsSummariesAndOutcomeAreForwarded(t *testing.T) {
	h := runResultsJob(t, resultsScript(nil), nil)
	if r := seenRun.Load(); r == nil || !r.Job.SendAnnotations || !r.Job.SendSummaries {
		t.Errorf("the executor was not told to send: %+v", r)
	}
	s := h.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	// the annotations travel with the UpdateJob that completes the step
	var withAnn *ermite.UpdateJobRequest
	for _, u := range s.updates {
		if len(u.Annotations) > 0 {
			withAnn = u
		}
	}
	if withAnn == nil || len(withAnn.Steps) != 1 || withAnn.Steps[0].Number != 2 || withAnn.Steps[0].State != ermite.StepState_STEP_STATE_COMPLETED {
		t.Fatalf("annotations did not ride the completion of step 2: %+v", s.updates)
	}
	got := s.annots[2]
	if len(got) != 2 || got[0].Level != ermite.AnnotationLevel_ANNOTATION_LEVEL_WARNING || got[0].Title != "T" || got[0].File != "a.go" ||
		got[0].StartLine != 3 || got[0].EndLine != 4 || got[0].StepNumber != 2 || got[1].Level != ermite.AnnotationLevel_ANNOTATION_LEVEL_FAILURE {
		t.Errorf("annotations %+v", got)
	}
	if s.summaries[2] != "# Summary\n" {
		t.Errorf("summaries %v", s.summaries)
	}
	u := s.steps[2]
	if u.Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS || u.Outcome != ermite.CiConclusion_CI_CONCLUSION_FAILURE {
		t.Errorf("conclusion %v outcome %v", u.Conclusion, u.Outcome)
	}
	if o := s.steps[3].Outcome; o != ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED {
		t.Errorf("a step that sends no outcome carries none: %v", o)
	}
	// nothing is left for CompleteJob when the UpdateJob went through
	if n := len(s.completes[0].Annotations); n != 0 {
		t.Errorf("CompleteJob repeats %d annotations", n)
	}
}

// Without the server features, nothing is sent.
func TestResultsAreNotSentWithoutTheFeatures(t *testing.T) {
	h := runResultsJob(t, resultsScript(nil), func(h *harness) {
		h.runner.Features = func(context.Context) ([]string, error) { return []string{"ci.runners"}, nil }
	})
	if r := seenRun.Load(); r == nil || r.Job.SendAnnotations || r.Job.SendSummaries {
		t.Errorf("the executor was told to send: %+v", r)
	}
	s := h.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.annots) != 0 || len(s.summaries) != 0 {
		t.Errorf("annotations %v summaries %v", s.annots, s.summaries)
	}
	if len(s.completes[0].Annotations) != 0 {
		t.Error("CompleteJob carries annotations")
	}
	// the outcome is part of the StepUpdate, not of a feature
	if s.steps[2].Outcome != ermite.CiConclusion_CI_CONCLUSION_FAILURE {
		t.Error("outcome")
	}
}

func TestFeaturesUnreachableMeansNothingIsSent(t *testing.T) {
	h := runResultsJob(t, resultsScript(nil), func(h *harness) {
		h.runner.Features = func(context.Context) ([]string, error) { return nil, errors.New("down") }
	})
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.annots) != 0 || len(h.srv.summaries) != 0 {
		t.Error("sent without knowing the features")
	}
}

// The host enforces the limits of the contract on what the guest sends: ten
// per level, 4096 characters, titles of 255, valid numbers.
func TestGuestAnnotationsAreBoundedByTheHost(t *testing.T) {
	var many []*execpb.Annotation
	for range 12 {
		many = append(many, &execpb.Annotation{Level: lWarn, Message: "w"})
	}
	many = append(many,
		&execpb.Annotation{Level: lFail, Message: strings.Repeat("é", 4097)},                                  // too long: dropped
		&execpb.Annotation{Level: lFail, Message: strings.Repeat("é", 4096), Title: strings.Repeat("t", 300)}, // kept, title cut
		&execpb.Annotation{Level: lFail, Message: ""},                                                         // empty: dropped
		&execpb.Annotation{Level: execpb.AnnotationLevel_ANNOTATION_LEVEL_UNSPECIFIED, Message: "no level"},   // dropped
		&execpb.Annotation{Level: lFail, Message: "bad numbers", StartLine: -4, EndLine: -1, StartColumn: 5},  // numbers made valid
		&execpb.Annotation{Level: lFail, Message: "end before", StartLine: 9, EndLine: 3},
	)
	sc := func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		annFrame(c, 1, many...)
		stepDone(c, 1, "Set up job", cOK)
		jobDone(c, cOK, nil, false)
	}
	h := runResultsJob(t, sc, nil)
	s := h.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	count := map[ermite.AnnotationLevel]int{}
	for _, a := range s.annots[1] {
		count[a.Level]++
	}
	if count[ermite.AnnotationLevel_ANNOTATION_LEVEL_WARNING] != 10 || count[ermite.AnnotationLevel_ANNOTATION_LEVEL_FAILURE] != 3 {
		t.Fatalf("counts %v", count)
	}
	var cut, neg, end *ermite.Annotation
	for _, a := range s.annots[1] {
		switch a.Message {
		case strings.Repeat("é", 4096):
			cut = a
		case "bad numbers":
			neg = a
		case "end before":
			end = a
		}
	}
	if cut == nil || len([]rune(cut.Title)) != 255 {
		t.Errorf("title: %v", cut)
	}
	if neg == nil || neg.StartLine != 0 || neg.EndLine != 0 || neg.StartColumn != 5 {
		t.Errorf("%+v", neg)
	}
	if end == nil || end.StartLine != 9 || end.EndLine != 0 {
		t.Errorf("%+v", end)
	}
	if v := s.violations; len(v) != 0 {
		t.Error(v)
	}
}

func TestGuestResultsForAnUnstartedStepFailTheJob(t *testing.T) {
	h := hostile(t, "annotations for step 9, which has not started", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		annFrame(c, 9, &execpb.Annotation{Level: lWarn, Message: "x"})
		time.Sleep(50 * time.Millisecond)
	})
	_ = h
}

// The annotations of a step that never completes (the executor is stopped) are
// sent with CompleteJob.
func TestAnnotationsOfAnUnfinishedStepGoWithCompleteJob(t *testing.T) {
	sc := func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepDone(c, 1, "Set up job", cOK)
		stepStart(c, 2, "Run it")
		annFrame(c, 2, &execpb.Annotation{Level: lWarn, Message: "last words"})
		// the executor dies here, without completing step 2 or the job
	}
	h := runResultsJob(t, sc, nil)
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.completes[0].Annotations) != 1 || h.srv.completes[0].Annotations[0].Message != "last words" {
		t.Errorf("%+v", h.srv.completes[0].Annotations)
	}
}

// UpdateJob that fails and is retried: the annotations are not duplicated,
// since the list replaces the stored one.
func TestAnnotationsSurviveARetriedUpdateJob(t *testing.T) {
	var fails atomic.Int32
	h := runResultsJob(t, resultsScript(nil), func(h *harness) {
		h.srv.updateHook = func(_ int, r *ermite.UpdateJobRequest) error {
			if len(r.Annotations) > 0 && fails.Add(1) <= 2 {
				return connect.NewError(connect.CodeUnavailable, nil)
			}
			return nil
		}
	})
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.annots[2]) != 2 {
		t.Errorf("%d annotations after retries", len(h.srv.annots[2]))
	}
}

// A refused UpdateJob (invalid_argument) does not lose the step update: the
// step is resent without its annotations.
func TestRefusedAnnotationsDoNotLoseTheStep(t *testing.T) {
	h := runResultsJob(t, resultsScript(nil), func(h *harness) {
		h.srv.updateHook = func(_ int, r *ermite.UpdateJobRequest) error {
			if len(r.Annotations) > 0 {
				return connect.NewError(connect.CodeInvalidArgument, nil)
			}
			return nil
		}
	})
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if u := h.srv.steps[2]; u == nil || u.State != ermite.StepState_STEP_STATE_COMPLETED {
		t.Errorf("step 2: %v", u)
	}
	if len(h.srv.annots[2]) != 0 {
		t.Error("refused annotations stored")
	}
	if len(h.srv.completes[0].Annotations) != 0 {
		t.Error("a refused list is sent again with CompleteJob")
	}
}

func TestSummaryOverOneMiBIsDropped(t *testing.T) {
	sc := func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		sumFrame(c, 1, strings.Repeat("x", 1<<20+1))
		stepDone(c, 1, "Set up job", cOK)
		stepStart(c, 2, "Run it")
		sumFrame(c, 2, strings.Repeat("y", 1<<20))
		stepDone(c, 2, "Run it", cOK)
		jobDone(c, cOK, nil, false)
	}
	h := runResultsJob(t, sc, nil)
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if _, has := h.srv.summaries[1]; has {
		t.Error("a summary over 1 MiB was sent")
	}
	if len(h.srv.summaries[2]) != 1<<20 || h.srv.summaries[2][0] != 'y' {
		t.Errorf("summary of %d bytes", len(h.srv.summaries[2]))
	}
}

// One summary and one list of annotations per step, sent while the step runs
// (GO-002): a second one, or one for a completed step, is the guest
// misbehaving, and it would let it queue unbounded data.
func TestGuestSecondSummaryFailsTheJob(t *testing.T) {
	hostile(t, "a summary for step 1, which already sent them", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		sumFrame(c, 1, "one")
		sumFrame(c, 1, "two")
		time.Sleep(50 * time.Millisecond)
	})
}

func TestGuestSummaryForACompletedStepFailsTheJob(t *testing.T) {
	hostile(t, "a summary for step 1, which is completed", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepDone(c, 1, "Set up job", cOK)
		sumFrame(c, 1, "late")
		time.Sleep(50 * time.Millisecond)
	})
}

func TestGuestSecondAnnotationListFailsTheJob(t *testing.T) {
	hostile(t, "annotations for step 1, which already sent them", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		annFrame(c, 1, &execpb.Annotation{Level: lWarn, Message: "a"})
		annFrame(c, 1, &execpb.Annotation{Level: lWarn, Message: "b"})
		time.Sleep(50 * time.Millisecond)
	})
}

func TestGuestAnnotationsForACompletedStepFailTheJob(t *testing.T) {
	hostile(t, "annotations for step 1, which is completed", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepDone(c, 1, "Set up job", cOK)
		annFrame(c, 1, &execpb.Annotation{Level: lWarn, Message: "late"})
		time.Sleep(50 * time.Millisecond)
	})
}

// The summaries count in the memory the outbox bounds, and backpressure holds
// the producer while the server does not answer (GO-002).
func TestSummaryBytesAreBoundedByTheOutbox(t *testing.T) {
	release := make(chan struct{})
	cl := &blockingSummaryClient{release: release}
	o := newOutbox(cl, "j1", fastTiming(), func(error) {}, zerowrapNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.start(ctx)
	defer close(release)
	big := strings.Repeat("x", 1<<20)
	go func() {
		for i := 1; i <= 40; i++ { // 40 MiB offered, 16 MiB allowed in memory
			o.Summary(int32(i), big)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	o.mu.Lock()
	queued := o.queued
	o.mu.Unlock()
	if queued > maxQueuedBytes+len(big)+64 {
		t.Errorf("%d bytes queued, the bound is %d", queued, maxQueuedBytes)
	}
	if queued < maxQueuedBytes/2 {
		t.Errorf("only %d bytes queued: the summaries are not counted?", queued)
	}
}

type blockingSummaryClient struct {
	stubClient
	release chan struct{}
}

func (c *blockingSummaryClient) UploadSummary(ctx context.Context, _ *connect.Request[ermite.UploadSummaryRequest]) (*connect.Response[ermite.UploadSummaryResponse], error) {
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return connect.NewResponse(&ermite.UploadSummaryResponse{}), nil
}

// A string cut by the host must not keep the allocation of the frame the guest
// sent it in (GO-003): the cut copy has its own memory.
func TestCutStringsDoNotRetainTheGuestAllocation(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	t0 := cutRunes(big, 255)
	if len(t0) != 255 {
		t.Fatal(len(t0))
	}
	if unsafe.StringData(t0) == unsafe.StringData(big) {
		t.Error("cutRunes returns a substring of its argument")
	}
	name := cutName(big)
	if len(name) != maxNameLen || unsafe.StringData(name) == unsafe.StringData(big) {
		t.Error("cutName returns a substring of its argument")
	}
	// an annotation that is kept whole (not cut) is not copied: its message is
	// within 4096 characters, a frame cannot make it retain more than that
	// through a substring because the host does not slice it.
	a := annotationsFrom(1, []*execpb.Annotation{{Level: lWarn, Message: "m", Title: big, File: strings.Repeat("f", 2000)}})
	if len(a) != 1 || len(a[0].Title) != 255 || unsafe.StringData(a[0].Title) == unsafe.StringData(big) {
		t.Errorf("title %d", len(a[0].Title))
	}
	if a[0].File != "" {
		t.Error("a file over 1,024 characters is dropped")
	}
}

// A message of the frame is bounded by execstream.MaxFrame: a summary of 1 MiB
// fits.
func TestSummaryFitsAFrame(t *testing.T) {
	f := &execpb.Frame{Body: &execpb.Frame_StepSummary{StepSummary: &execpb.StepSummary{Step: 1, Markdown: strings.Repeat("x", 1<<20)}}}
	c := execstream.NewConn(&discard{})
	if err := c.Send(f); err != nil {
		t.Fatal(err)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Read([]byte) (int, error)    { select {} }

func zerowrapNop() zerowrap.Logger {
	return zerowrap.New(zerowrap.Config{Level: "error", Format: "json", Output: io.Discard})
}
