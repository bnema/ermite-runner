// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// jobClient is what a job needs from the RunnerService client (job token).
type jobClient interface {
	callClient
	Heartbeat(context.Context, *connect.Request[ermite.HeartbeatRequest]) (*connect.Response[ermite.HeartbeatResponse], error)
	WatchJob(context.Context, *connect.Request[ermite.WatchJobRequest]) (*connect.ServerStreamForClient[ermite.WatchJobResponse], error)
	CompleteJob(context.Context, *connect.Request[ermite.CompleteJobRequest]) (*connect.Response[ermite.CompleteJobResponse], error)
	ResolveActions(context.Context, *connect.Request[ermite.ResolveActionsRequest]) (*connect.Response[ermite.ResolveActionsResponse], error)
}

var _ jobClient = ermiteconnect.RunnerServiceClient(nil)

// jobRun is the host's side of one job: the assignment, the sandbox, and the
// calls to ermite with the job token.
type jobRun struct {
	r      *Runner
	job    *ermite.Job
	client jobClient
	t      Timing
	log    zerowrap.Logger // with job_id, run_id and attempt

	// ctx lives until the job is finished or abandoned. Shutdown does not end
	// it: a shutdown is a cancellation that the job reports.
	ctx    context.Context
	cancel context.CancelCauseFunc

	// receivedAt is when the host got the job: the job timeout counts from it
	// (runner.proto: Job.timeout_minutes).
	receivedAt time.Time

	cancelCh   chan *execpb.Cancel // the first value is sent to the executor
	cancelMu   sync.Mutex
	cancelAt   string            // reason of the first cancel request, "" if none
	cancelKind execpb.CancelKind // kind of the first cancel request
	stopping   bool              // the runner is shutting down

	mu    sync.Mutex
	steps map[int32]*ermite.StepUpdate
	// What the guest has told us, for the validation of the next frame: the
	// guest is untrusted code, its step numbers and lines are checked before
	// they reach the server (see violate).
	maxStarted  int32
	lastLogStep int32
	violation   string
	// contexts is Job.Contexts as handed to the executor: with the server URLs
	// pointed at the runner's URL when guest forge access is on.
	contexts string
	// Origins that are ermite for the archive tokens (see resolveResponse), and
	// the budget of resolution requests of the job (see resolveActions).
	publicOrigin, serverOrigin, guestForge string
	resolveRounds, resolveRefs             int
	resolved                               map[string]*execpb.ResolvedAction // by "uses", for the job
	// features is what the server accepts; annLists are the annotations the
	// executor sent for steps that have not completed yet (see results.go).
	features jobFeatures
	annLists map[int32][]*ermite.Annotation
	// annSeen and sumSeen are the steps that sent their annotations and their
	// summary: each is sent once.
	annSeen, sumSeen map[int32]bool
	annDropped       int // annotations dropped for lack of room, see reportDroppedAnnotations
	// hard stops the sandbox at once; set before the session starts.
	hard    func()
	result  *execpb.JobResult
	outbox  *outbox
	endedBy error // the server ended the job, or the assignment is gone
}

var errAbandoned = errors.New("the server ended the job or revoked the assignment")

func (j *jobRun) abandon(err error) {
	j.mu.Lock()
	if j.endedBy == nil {
		j.endedBy = err
	}
	j.mu.Unlock()
	j.cancel(errAbandoned)
}

func (j *jobRun) abandoned() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.endedBy
}

// requestCancel asks the executor to stop; only the first request counts.
func (j *jobRun) requestCancel(reason string, kind execpb.CancelKind) {
	j.cancelMu.Lock()
	defer j.cancelMu.Unlock()
	if j.cancelAt != "" {
		return
	}
	j.cancelAt = reason
	j.cancelKind = kind
	j.cancelCh <- &execpb.Cancel{Reason: reason, Kind: kind}
}

// firstCancelKind is the kind of the first cancel request.
func (j *jobRun) firstCancelKind() execpb.CancelKind {
	j.cancelMu.Lock()
	defer j.cancelMu.Unlock()
	return j.cancelKind
}

func (j *jobRun) cancelRequested() bool {
	j.cancelMu.Lock()
	defer j.cancelMu.Unlock()
	return j.cancelAt != ""
}

// heartbeat sends the acknowledgement. It retries every HeartbeatRetry until
// it succeeds, so that the 30 s of the server are used well; it returns false
// when the assignment is gone.
func (j *jobRun) ack(ctx context.Context) bool {
	for {
		err := j.beat(ctx)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			j.abandon(err)
			return false
		}
		j.log.Warn().Err(err).Msg("first heartbeat failed, retrying")
		if !sleep(ctx, j.t.HeartbeatRetry) {
			return false
		}
	}
}

func (j *jobRun) beat(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, j.t.CallTimeout)
	defer cancel()
	_, err := j.client.Heartbeat(cctx, connect.NewRequest(&ermite.HeartbeatRequest{JobId: j.job.JobId}))
	return err
}

// heartbeats keeps the job alive: every HeartbeatEvery, and after a failure
// every HeartbeatRetry until one succeeds (runner.proto: Heartbeat).
func (j *jobRun) heartbeats(ctx context.Context) {
	wait := j.t.HeartbeatEvery
	for {
		if !sleep(ctx, wait) {
			return
		}
		if err := j.beat(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			switch classify(err) {
			case kindEnded, kindAbandoned:
				j.abandon(err)
				return
			}
			j.log.Warn().Err(err).Dur("retry_in", j.t.HeartbeatRetry).Msg("heartbeat failed")
			wait = j.t.HeartbeatRetry
			continue
		}
		wait = j.t.HeartbeatEvery
	}
}

// watch follows WatchJob: a cancel request stops the job, an Ended message or
// the end-of-job error abandons it. The stream is reopened after any other
// error and after a clean close without Ended.
func (j *jobRun) watch(ctx context.Context) {
	for ctx.Err() == nil {
		stream, err := j.client.WatchJob(ctx, connect.NewRequest(&ermite.WatchJobRequest{JobId: j.job.JobId}))
		if err == nil {
			for stream.Receive() {
				switch m := stream.Msg().Msg.(type) {
				case *ermite.WatchJobResponse_Cancel:
					j.requestCancel(cancelText(m.Cancel), kindOf(m.Cancel.GetReason()))
				case *ermite.WatchJobResponse_Ended:
					_ = stream.Close()
					j.abandon(fmt.Errorf("job ended: %s", m.Ended.GetConclusion()))
					return
				}
			}
			err = stream.Err()
			_ = stream.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			switch classify(err) {
			case kindEnded, kindAbandoned:
				j.abandon(err)
				return
			}
			j.log.Warn().Err(err).Msg("WatchJob failed, reopening")
		}
		if !sleep(ctx, j.t.WatchRetry) {
			return
		}
	}
}

func cancelText(c *ermite.CancelRequest) string {
	if c.GetMessage() != "" {
		return c.GetMessage()
	}
	return c.GetReason().String()
}

func kindOf(r ermite.CancelReason) execpb.CancelKind {
	if r == ermite.CancelReason_CANCEL_REASON_TIMEOUT {
		return execpb.CancelKind_CANCEL_KIND_TIMEOUT
	}
	// User, fail-fast, and any reason this runner does not know: cancelled.
	return execpb.CancelKind_CANCEL_KIND_USER
}

// ---- steps

// Limits on what the guest may send, which keep the memory of the host bounded
// whatever the guest does: runner.proto allows 1,000 steps per job, names of
// 1,024 characters, and lines of 64 KiB.
const (
	maxSteps    = 1000
	maxNameLen  = 1024
	maxLineText = 64 * 1024
)

// violate fails the job because the guest broke the protocol: it writes the
// reason in the log, asks the executor to stop and destroys the sandbox. Only
// the first violation counts; the frames that follow are dropped.
func (j *jobRun) violate(format string, a ...any) {
	j.mu.Lock()
	if j.violation != "" {
		j.mu.Unlock()
		return
	}
	j.violation = fmt.Sprintf(format, a...)
	msg := j.violation
	j.mu.Unlock()
	j.log.Error().Str("violation", msg).Msg("the executor broke the protocol")
	j.sandboxError("The runner stopped the job: the executor broke the protocol: " + msg)
	if j.hard != nil {
		j.hard()
	}
}

func (j *jobRun) violated() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.violation != ""
}

func (j *jobRun) stepStarted(s *execpb.StepStarted) {
	j.mu.Lock()
	switch {
	case j.violation != "":
		j.mu.Unlock()
		return
	case s.Number < 1 || s.Number > maxSteps:
		j.mu.Unlock()
		j.violate("step number %d is outside 1..%d", s.Number, maxSteps)
		return
	case s.Number <= j.maxStarted:
		j.mu.Unlock()
		j.violate("step %d started after step %d", s.Number, j.maxStarted)
		return
	}
	j.maxStarted = s.Number
	u := &ermite.StepUpdate{Number: s.Number, Name: cutName(s.Name), State: ermite.StepState_STEP_STATE_IN_PROGRESS, StartedAt: tsOf(s.UnixNano)}
	j.steps[s.Number] = u
	j.mu.Unlock()
	j.outbox.Step(u, nil)
}

func (j *jobRun) stepCompleted(s *execpb.StepCompleted) {
	j.mu.Lock()
	if j.violation != "" {
		j.mu.Unlock()
		return
	}
	old := j.steps[s.Number]
	switch {
	case old == nil:
		j.mu.Unlock()
		j.violate("step %d completed without having started", s.Number)
		return
	case old.State == ermite.StepState_STEP_STATE_COMPLETED:
		j.mu.Unlock()
		j.violate("step %d completed twice", s.Number)
		return
	}
	u := cloneStep(old)
	if s.Name != "" {
		u.Name = cutName(s.Name)
	}
	u.State = ermite.StepState_STEP_STATE_COMPLETED
	u.Conclusion = conclusionOf(s.Conclusion)
	if u.Conclusion == ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED || u.Conclusion == ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT {
		u.Conclusion = ermite.CiConclusion_CI_CONCLUSION_FAILURE
	}
	u.CompletedAt = tsOf(s.UnixNano)
	// The outcome is how the step ended before continue-on-error; a runner that
	// does not say leaves it unspecified, which means the conclusion.
	if o := conclusionOf(s.Outcome); o != ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED {
		if o == ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT {
			o = ermite.CiConclusion_CI_CONCLUSION_FAILURE
		}
		u.Outcome = o
	}
	j.steps[s.Number] = u
	anns := j.takeAnnotations(s.Number)
	j.mu.Unlock()
	j.outbox.Step(u, anns)
}

// logLine checks and queues a line of the log. The step must be one that was
// started and not completed, and not lower than the step of the previous line
// (the server refuses a log that goes back); the text is made valid for the
// server: UTF-8, no line feed (a line feed in a line makes more lines).
func (j *jobRun) logLine(l *execpb.Log) {
	j.mu.Lock()
	if j.violation != "" {
		j.mu.Unlock()
		return
	}
	st := j.steps[l.Step]
	switch {
	case l.Step < 1 || l.Step > maxSteps || st == nil:
		j.mu.Unlock()
		j.violate("a log line for step %d, which has not started", l.Step)
		return
	case st.State == ermite.StepState_STEP_STATE_COMPLETED:
		j.mu.Unlock()
		j.violate("a log line for step %d, which is completed", l.Step)
		return
	case l.Step < j.lastLogStep:
		j.mu.Unlock()
		j.violate("a log line for step %d after one for step %d", l.Step, j.lastLogStep)
		return
	case len(l.Text) > maxLineText:
		j.mu.Unlock()
		j.violate("a log line of %d bytes (the limit is %d)", len(l.Text), maxLineText)
		return
	}
	j.lastLogStep = l.Step
	j.mu.Unlock()
	text := l.Text
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\ufffd")
	}
	for i, part := range strings.Split(text, "\n") {
		j.outbox.Log(logLine{step: l.Step, unixNano: l.UnixNano, text: part, continued: l.Continued && i == 0})
	}
}

// cutName bounds a step name to what the server keeps, on a character boundary.
// The result is a copy: a substring would keep the whole name the guest sent
// alive (see cutRunes).
func cutName(s string) string {
	if len(s) <= maxNameLen {
		return strings.ToValidUTF8(s, "\ufffd")
	}
	s = s[:maxNameLen]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return strings.Clone(s)
}

func cloneStep(u *ermite.StepUpdate) *ermite.StepUpdate {
	return proto.Clone(u).(*ermite.StepUpdate)
}

func tsOf(unixNano int64) *timestamppb.Timestamp {
	if unixNano == 0 {
		return timestamppb.Now()
	}
	return timestamppb.New(time.Unix(0, unixNano))
}

func conclusionOf(c execpb.StepConclusion) ermite.CiConclusion {
	switch c {
	case execpb.StepConclusion_STEP_CONCLUSION_SUCCESS:
		return ermite.CiConclusion_CI_CONCLUSION_SUCCESS
	case execpb.StepConclusion_STEP_CONCLUSION_FAILURE:
		return ermite.CiConclusion_CI_CONCLUSION_FAILURE
	case execpb.StepConclusion_STEP_CONCLUSION_CANCELLED:
		return ermite.CiConclusion_CI_CONCLUSION_CANCELLED
	case execpb.StepConclusion_STEP_CONCLUSION_SKIPPED:
		return ermite.CiConclusion_CI_CONCLUSION_SKIPPED
	case execpb.StepConclusion_STEP_CONCLUSION_TIMED_OUT:
		return ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT
	}
	return ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED
}

// finalSteps closes the steps that never completed (the executor died or was
// stopped) with the conclusion of the job, and returns all snapshots in order.
func (j *jobRun) finalSteps(jobConclusion ermite.CiConclusion) []*ermite.StepUpdate {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := ermite.CiConclusion_CI_CONCLUSION_FAILURE
	if jobConclusion == ermite.CiConclusion_CI_CONCLUSION_CANCELLED || jobConclusion == ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT {
		end = ermite.CiConclusion_CI_CONCLUSION_CANCELLED
	}
	var out []*ermite.StepUpdate
	for _, u := range j.steps {
		if u.State != ermite.StepState_STEP_STATE_COMPLETED {
			u = cloneStep(u)
			u.State, u.Conclusion, u.CompletedAt = ermite.StepState_STEP_STATE_COMPLETED, end, timestamppb.Now()
		}
		out = append(out, u)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
	return out
}

// ---- the job

// run runs the whole job: acknowledgement, sandbox, executor session, final
// report. It returns when everything is over and the sandbox is gone.
func (j *jobRun) run(rt *Runner, shutdown <-chan struct{}) {
	defer j.cancel(nil)
	ctx := j.ctx

	if !j.acknowledge(ctx, shutdown) {
		return
	}
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); j.heartbeats(ctx) }()
	go func() { defer bg.Done(); j.watch(ctx) }()
	// The context is cancelled first, so that the heartbeat and watch loops end.
	defer func() { j.cancel(nil); bg.Wait() }()

	j.outbox = newOutbox(j.client, j.job.JobId, j.t, func(err error) { j.abandon(err) }, j.log)
	j.outbox.start(ctx)

	stopHard := j.startStopTimers(ctx, shutdown)
	// The hard stop frees the reader of the stream if it waits for room in the
	// outbox (the server keeps failing): nothing else would wake it.
	go func() {
		select {
		case <-stopHard:
			j.outbox.releaseProducers()
		case <-ctx.Done():
		}
	}()
	out := j.execute(ctx, rt, stopHard)
	j.report(ctx, out, shutdown)
}

// acknowledge sends the first heartbeat, before anything else. A shutdown ends
// the wait: the job is not started and not acknowledged, and the server gives it
// to another runner when no heartbeat comes within 30 s (runner.proto, FetchJob:
// this is not a failure of the job). It reports whether the job goes on.
func (j *jobRun) acknowledge(ctx context.Context, shutdown <-chan struct{}) bool {
	ackCtx, stopAck := context.WithCancel(ctx)
	defer stopAck()
	go func() {
		select {
		case <-shutdown:
			stopAck()
		case <-ackCtx.Done():
		}
	}()
	if j.ack(ackCtx) {
		return true
	}
	select {
	case <-shutdown:
		j.log.Warn().Msg("the runner is shutting down before the job was acknowledged: the server will give it to another runner")
	default:
		j.log.Warn().Msg("the assignment was lost before the job started")
	}
	return false
}

// startStopTimers arms the two reasons to stop the job from the host, each a
// cancellation followed by a hard stop of the sandbox (the returned channel is
// closed) if the executor does not answer: a shutdown of the runner
// (ShutdownGrace), and the job timeout, which the executor enforces itself but
// a stuck executor would not (TimeoutGrace after the deadline: the escalation
// SIGINT, SIGTERM, SIGKILL takes 10 s). The goroutines end with ctx.
func (j *jobRun) startStopTimers(ctx context.Context, shutdown <-chan struct{}) <-chan struct{} {
	stopHard := make(chan struct{})
	var hardOnce sync.Once
	hard := func() { hardOnce.Do(func() { close(stopHard) }) }
	j.hard = hard
	// after waits grace, then stops the sandbox; it returns early with ctx.
	after := func(grace time.Duration) {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
			hard()
		case <-ctx.Done():
		}
	}
	go func() {
		select {
		case <-shutdown:
		case <-ctx.Done():
			return
		}
		j.cancelMu.Lock()
		j.stopping = true
		j.cancelMu.Unlock()
		j.requestCancel("the runner is shutting down", execpb.CancelKind_CANCEL_KIND_SHUTDOWN)
		after(j.t.ShutdownGrace)
	}()
	go func() {
		wait := time.NewTimer(time.Until(j.deadline()))
		defer wait.Stop()
		select {
		case <-wait.C:
		case <-ctx.Done():
			return
		}
		j.log.Warn().Int("timeout_minutes", j.timeoutMinutes()).Msg("the job timeout has passed")
		j.requestCancel("the job timeout has passed", execpb.CancelKind_CANCEL_KIND_TIMEOUT)
		after(j.t.TimeoutGrace)
	}()
	return stopHard
}

// report ends the job towards the server: the log is flushed, then CompleteJob.
// Nothing is sent to a job that the server has ended.
func (j *jobRun) report(ctx context.Context, out outcome, shutdown <-chan struct{}) {
	if err := j.abandoned(); err != nil {
		j.log.Warn().Err(err).Msg("the server ended the job: no result is reported")
		return
	}
	// The report is made even when the runner is shutting down: it uses its own
	// deadline.
	budget := j.t.CompleteDeadline
	select {
	case <-shutdown:
		// The service manager waits only so long (see Timing): once the runner's
		// shutdown channel is closed, whatever the job's outcome, the report gets
		// what is left of the budget. The channel is read here, not the flag set
		// by the stop timers, which may not have run yet.
		budget = min(budget, j.t.ShutdownReport)
	default:
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	// Output limits are checked before the log is flushed: a job whose outputs
	// the server would refuse fails without outputs, with a line in the log
	// (runner.proto: CompleteJobRequest.outputs), and lines sent after
	// CompleteJob are rejected.
	if msg := checkOutputs(out.outputs); msg != "" {
		j.sandboxError(msg)
		out = failure()
	}
	// The flush gets half of the budget: when the server keeps failing the
	// uploads, the other half is still there for CompleteJob, which is what
	// ends the job for the server.
	fctx, fcancel := context.WithTimeout(rctx, budget/2)
	j.outbox.Close(fctx)
	fcancel()
	if err := j.abandoned(); err != nil {
		return
	}
	j.reportDroppedAnnotations(rctx, budget/4)
	j.complete(rctx, out.conclusion, out.outputs, out.shutdown)
}

// outcome is how a job ended, as far as the host can tell.
type outcome struct {
	conclusion ermite.CiConclusion
	outputs    map[string]string
	// shutdown is true when the runner's shutdown cut the job short.
	shutdown bool
}

func failure() outcome { return outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_FAILURE} }

// execute starts the sandbox, runs the job in it and returns how it ended.
func (j *jobRun) execute(ctx context.Context, rt *Runner, stopHard <-chan struct{}) outcome {
	var err error
	j.setOrigins(j.job.Contexts, rt.Cfg.URL, rt.Cfg.GuestForgeOrigin)
	if j.contexts, err = RewriteServerURLs(j.job.Contexts, rt.Cfg.GuestForgeOrigin); err != nil {
		j.failEarly(fmt.Sprintf("The job's contexts cannot be prepared: %v", err))
		return failure()
	}
	if j.contexts, err = SetGitHubToken(j.contexts, j.job.GithubToken); err != nil {
		j.failEarly(fmt.Sprintf("The job's contexts cannot be prepared: %v", err))
		return failure()
	}
	sb, err := rt.Driver.New()
	if err != nil {
		j.failEarly(fmt.Sprintf("The runner has no free sandbox: %v", err))
		return failure()
	}
	// Stop runs to completion even when the job context is done.
	defer j.stopSandbox(ctx, rt, sb)
	j.features = j.loadFeatures(ctx, rt)

	conn, err := j.startSandbox(ctx, rt, sb, stopHard)
	if err != nil {
		if ctx.Err() != nil || j.cancelRequested() {
			return outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_CANCELLED, shutdown: j.isStopping()}
		}
		j.failEarly(fmt.Sprintf("The sandbox did not start: %v", err))
		return failure()
	}
	defer func() { _ = conn.Close() }()

	// Hard stops (grace elapsed, or the job was abandoned) close the stream. The
	// goroutine ends when this function returns, whatever happens to ctx.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-stopHard:
			_ = conn.Close()
		case <-ctx.Done():
			_ = conn.Close()
		case <-sessionDone:
		}
	}()

	exit, err := j.drive(conn, rt)
	return j.outcomeOf(ctx, exit, err)
}

// stopSandbox destroys the sandbox; a cleanup that left something behind stops
// the runner from taking jobs until it is reaped.
func (j *jobRun) stopSandbox(ctx context.Context, rt *Runner, sb sandbox.Sandbox) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.t.StopDeadline)
	defer cancel()
	if err := sb.Stop(sctx); err != nil {
		j.log.Error().Err(err).Msg("stopping the sandbox")
		if errors.Is(err, sandbox.ErrLeaked) {
			rt.markLeaked()
		}
	}
}

// startSandbox starts the sandbox; the start is bounded by the job context and
// by the hard stop.
func (j *jobRun) startSandbox(ctx context.Context, rt *Runner, sb sandbox.Sandbox, stopHard <-chan struct{}) (sandbox.Conn, error) {
	startCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-stopHard:
			cancelStart()
		case <-stopWatch:
		}
	}()
	return sb.Start(startCtx, rt.spec(j.job))
}

// drive sends the job to the executor and handles its frames until it exits.
func (j *jobRun) drive(conn sandbox.Conn, rt *Runner) (*execpb.Exit, error) {
	run := &execpb.Run{Job: &execpb.JobSpec{
		WorkflowPath: j.job.WorkflowPath, WorkflowYaml: j.job.WorkflowYaml, JobKey: j.job.JobKey,
		Matrix: j.job.Matrix, Contexts: j.contexts, Repo: j.job.Repo, TimeoutMinutes: int32(j.timeoutMinutes()),
		DeadlineMs: max(1, time.Until(j.deadline()).Milliseconds()),
		RunnerName: rt.Cfg.Name, RunnerVersion: rt.Cfg.Version, RunnerEnvironment: rt.Cfg.environment(),
		SendAnnotations: j.features.annotations, SendSummaries: j.features.summaries,
	}}
	run.Job.CachePort, run.Job.CacheToken = j.cacheFor(rt)
	_, exit, err := execstream.DriveJob(conn, run, j.cancelCh, execstream.JobHandler{
		StepStarted: j.stepStarted, StepCompleted: j.stepCompleted,
		Log:             j.logLine,
		JobResult:       func(r *execpb.JobResult) { j.mu.Lock(); j.result = r; j.mu.Unlock() },
		Resolve:         j.resolveActions,
		StepAnnotations: j.stepAnnotations, StepSummary: j.stepSummary,
	})
	return exit, err
}

// outcomeOf decides how the job ended from what the executor reported and what
// the host asked of it.
func (j *jobRun) outcomeOf(ctx context.Context, exit *execpb.Exit, driveErr error) outcome {
	j.mu.Lock()
	res := j.result
	j.mu.Unlock()

	switch {
	case j.violated():
		// The step numbers or lines of the executor were wrong: whatever it
		// reported is not trusted, the job fails (the reason is in the log).
		return failure()
	case res != nil:
		c := conclusionOf(res.Conclusion)
		if c == ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED || c == ermite.CiConclusion_CI_CONCLUSION_SKIPPED {
			c = ermite.CiConclusion_CI_CONCLUSION_FAILURE
		}
		if res.Error != "" {
			j.log.Info().Str("error", res.Error).Msg("the job reported an error")
		}
		return outcome{conclusion: c, outputs: res.Outputs, shutdown: res.RunnerShutdown && c == ermite.CiConclusion_CI_CONCLUSION_CANCELLED}
	case j.cancelRequested() || ctx.Err() != nil:
		// The executor did not report: it was stopped. A timeout wins over a
		// shutdown or a user cancel that came later, as on the server (the first
		// reason decides, and the timeout is the first when it fired first).
		switch {
		case j.firstCancelKind() == execpb.CancelKind_CANCEL_KIND_TIMEOUT:
			return outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT}
		case j.isStopping():
			return outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_CANCELLED, shutdown: true}
		}
		return outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_CANCELLED}
	}
	why := "the executor ended without a result"
	if driveErr != nil {
		why = driveErr.Error()
	} else if exit != nil && exit.Error != "" {
		why = exit.Error
	}
	j.sandboxError(fmt.Sprintf("The job's sandbox ended unexpectedly: %s", why))
	return failure()
}

// timeoutMinutes is the job timeout: at least 1, 360 when the job has none
// (runner.proto: Job.timeout_minutes).
func (j *jobRun) timeoutMinutes() int {
	if m := int(j.job.TimeoutMinutes); m > 0 {
		return m
	}
	return 360
}

// deadline is when the job times out, counted from its receipt.
func (j *jobRun) deadline() time.Time {
	return j.receivedAt.Add(time.Duration(j.timeoutMinutes()) * time.Minute)
}

func (j *jobRun) isStopping() bool {
	j.cancelMu.Lock()
	defer j.cancelMu.Unlock()
	return j.stopping
}

// failEarly reports a job that could not start: step 1 fails with the reason
// in its log.
func (j *jobRun) failEarly(msg string) {
	now := time.Now()
	j.stepStarted(&execpb.StepStarted{Number: 1, Name: "Set up job", UnixNano: now.UnixNano()})
	j.outbox.Log(logLine{step: 1, unixNano: now.UnixNano(), text: "##[error]" + msg})
	j.stepCompleted(&execpb.StepCompleted{Number: 1, Name: "Set up job", Conclusion: execpb.StepConclusion_STEP_CONCLUSION_FAILURE, UnixNano: time.Now().UnixNano()})
}

// sandboxError writes the reason into the log of the step that was running.
func (j *jobRun) sandboxError(msg string) {
	j.mu.Lock()
	var last int32 = 1
	for n := range j.steps {
		last = max(last, n)
	}
	j.mu.Unlock()
	j.outbox.Log(logLine{step: last, unixNano: time.Now().UnixNano(), text: "##[error]" + msg})
}

// complete reports the end of the job. CompleteJob is idempotent, so a lost
// response is retried until it is answered (applied or not).
func (j *jobRun) complete(ctx context.Context, conclusion ermite.CiConclusion, outputs map[string]string, shutdown bool) {
	req := &ermite.CompleteJobRequest{
		JobId: j.job.JobId, Conclusion: conclusion, Outputs: outputs,
		RunnerShutdown: shutdown && conclusion == ermite.CiConclusion_CI_CONCLUSION_CANCELLED,
		Steps:          j.finalSteps(conclusion),
		// The annotations no UpdateJob has carried: all of them with the
		// step they belong to, whole lists (replace semantics).
		Annotations: j.allPendingAnnotations(),
	}
	err := j.sendComplete(ctx, req, conclusion)
	if err == nil || ctx.Err() != nil || classify(err) != kindPermanent {
		return
	}
	// The server refused the result itself (invalid_argument, or
	// failed_precondition that is not the end of the job). CompleteJob is
	// "applied whole or not at all", so the job is still open: say why in the
	// log, then report a plain failure without outputs and with steps that are
	// valid by construction. Without this the job would stay running until the
	// heartbeat deadline of the server and then be reported as a lost runner.
	j.errorLog(ctx, fmt.Sprintf("The runner could not report the result of the job: %s. The job is reported as failed.", connect.CodeOf(err)))
	j.log.Error().Err(err).Msg("CompleteJob refused: retrying as a failure without outputs")
	req = &ermite.CompleteJobRequest{
		JobId: j.job.JobId, Conclusion: ermite.CiConclusion_CI_CONCLUSION_FAILURE,
		Steps: j.rebuiltSteps(),
	}
	if err := j.sendComplete(ctx, req, req.Conclusion); err != nil && ctx.Err() == nil {
		j.log.Error().Err(err).Msg("CompleteJob refused again, giving up")
	}
}

// sendComplete sends the request until the server answers: success, the end of
// the job, a lost assignment, or a refusal (returned).
func (j *jobRun) sendComplete(ctx context.Context, req *ermite.CompleteJobRequest, conclusion ermite.CiConclusion) error {
	b := backoff{min: j.t.BackoffMin, max: j.t.BackoffMax}
	for {
		cctx, cancel := context.WithTimeout(ctx, j.t.CallTimeout)
		resp, err := j.client.CompleteJob(cctx, connect.NewRequest(req))
		cancel()
		if err == nil {
			if resp.Msg.Conclusion != conclusion {
				j.log.Warn().Str("reported", conclusion.String()).Str("stored", resp.Msg.Conclusion.String()).Msg("the server holds another conclusion")
			}
			return nil
		}
		if ctx.Err() != nil {
			j.log.Error().Err(ctx.Err()).Msg("could not report the result")
			return err
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			return nil
		case kindPermanent:
			return err
		}
		j.log.Warn().Err(err).Msg("CompleteJob failed, retrying")
		if !sleep(ctx, b.next()) {
			return err
		}
	}
}

// errorLog appends a line to the log after the outbox is closed.
func (j *jobRun) errorLog(ctx context.Context, msg string) {
	j.appendLogLine(ctx, "##[error]"+msg)
}

// appendLogLine appends one line, in the last step, after the outbox is closed.
func (j *jobRun) appendLogLine(ctx context.Context, text string) {
	j.mu.Lock()
	var last int32 = 1
	for n := range j.steps {
		last = max(last, n)
	}
	j.mu.Unlock()
	j.outbox.AppendNow(ctx, logLine{step: last, unixNano: time.Now().UnixNano(), text: text})
}

// rebuiltSteps returns every known step as a valid, completed snapshot: number
// in 1..maxSteps, a name of at most 1,024 bytes, a conclusion the server
// accepts (success, failure, cancelled, skipped), both times set, and no step
// that finished before it began. A step that was running is failed.
func (j *jobRun) rebuiltSteps() []*ermite.StepUpdate {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := timestamppb.Now()
	var out []*ermite.StepUpdate
	for n, u := range j.steps {
		if n < 1 || n > maxSteps {
			continue
		}
		c := proto.Clone(u).(*ermite.StepUpdate)
		c.Name = cutName(c.Name)
		if c.StartedAt == nil {
			c.StartedAt = now
		}
		if c.State != ermite.StepState_STEP_STATE_COMPLETED {
			c.Conclusion = ermite.CiConclusion_CI_CONCLUSION_FAILURE
			c.CompletedAt = now
		}
		switch c.Conclusion {
		case ermite.CiConclusion_CI_CONCLUSION_SUCCESS, ermite.CiConclusion_CI_CONCLUSION_FAILURE,
			ermite.CiConclusion_CI_CONCLUSION_CANCELLED, ermite.CiConclusion_CI_CONCLUSION_SKIPPED:
		default:
			c.Conclusion = ermite.CiConclusion_CI_CONCLUSION_FAILURE
		}
		c.State = ermite.StepState_STEP_STATE_COMPLETED
		if c.CompletedAt == nil || c.CompletedAt.AsTime().Before(c.StartedAt.AsTime()) {
			c.CompletedAt = c.StartedAt
		}
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
	return out
}

// Limits of CompleteJobRequest.outputs.
const (
	maxOutputBytes = 1 << 20
	maxOutputName  = 255
)

// checkOutputs returns a message when the server would refuse the outputs: a
// name of 1 to 255 letters, digits, "_" and "-" starting with a letter or "_",
// no two names equal ignoring case, each value and the sum of names and values
// at most 1 MiB.
func checkOutputs(outputs map[string]string) string {
	total := 0
	seen := map[string]string{}
	names := make([]string, 0, len(outputs))
	for n := range outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := outputs[n]
		if !validOutputName(n) {
			return fmt.Sprintf("Job output name %q is not valid: use 1 to 255 letters, digits, '_' and '-', starting with a letter or '_'.", n)
		}
		if prev, dup := seen[strings.ToLower(n)]; dup {
			return fmt.Sprintf("Job outputs %q and %q differ only by case.", prev, n)
		}
		seen[strings.ToLower(n)] = n
		if len(v) > maxOutputBytes {
			return fmt.Sprintf("Job output %q is larger than 1 MiB.", n)
		}
		total += len(n) + len(v)
	}
	if total > maxOutputBytes {
		return "The job outputs are larger than 1 MiB in total."
	}
	return ""
}

func validOutputName(n string) bool {
	if n == "" || len(n) > maxOutputName {
		return false
	}
	for i, r := range n {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_'
		if ok := letter || (i > 0 && (r >= '0' && r <= '9' || r == '-')); !ok {
			return false
		}
	}
	return true
}
