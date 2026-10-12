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

	"connectrpc.com/connect"
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
}

var _ jobClient = ermiteconnect.RunnerServiceClient(nil)

// jobRun is the host's side of one job: the assignment, the sandbox, and the
// calls to ermite with the job token.
type jobRun struct {
	r      *Runner
	job    *ermite.Job
	client jobClient
	t      Timing
	logf   func(string, ...any)

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

	mu      sync.Mutex
	steps   map[int32]*ermite.StepUpdate
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
		j.logf("first heartbeat failed, retrying: %v", err)
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
			j.logf("heartbeat failed, retrying in %s: %v", j.t.HeartbeatRetry, err)
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
			j.logf("WatchJob: %v", err)
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

func (j *jobRun) stepStarted(s *execpb.StepStarted) {
	u := &ermite.StepUpdate{Number: s.Number, Name: s.Name, State: ermite.StepState_STEP_STATE_IN_PROGRESS, StartedAt: tsOf(s.UnixNano)}
	j.mu.Lock()
	j.steps[s.Number] = u
	j.mu.Unlock()
	j.outbox.Step(u)
}

func (j *jobRun) stepCompleted(s *execpb.StepCompleted) {
	j.mu.Lock()
	u := j.steps[s.Number]
	if u == nil {
		u = &ermite.StepUpdate{Number: s.Number, StartedAt: tsOf(s.UnixNano)}
	} else {
		u = cloneStep(u)
	}
	if s.Name != "" {
		u.Name = s.Name
	}
	u.State = ermite.StepState_STEP_STATE_COMPLETED
	u.Conclusion = conclusionOf(s.Conclusion)
	if u.Conclusion == ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED || u.Conclusion == ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT {
		u.Conclusion = ermite.CiConclusion_CI_CONCLUSION_FAILURE
	}
	u.CompletedAt = tsOf(s.UnixNano)
	j.steps[s.Number] = u
	j.mu.Unlock()
	j.outbox.Step(u)
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

	// 1. The acknowledgement comes first, before anything else.
	if !j.ack(ctx) {
		j.logf("job %s: assignment lost before it started", j.job.JobId)
		return
	}
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); j.heartbeats(ctx) }()
	go func() { defer bg.Done(); j.watch(ctx) }()
	// The context is cancelled first, so that the heartbeat and watch loops end.
	defer func() { j.cancel(nil); bg.Wait() }()

	j.outbox = newOutbox(j.client, j.job.JobId, j.t, func(err error) { j.abandon(err) }, j.logf)
	j.outbox.start(ctx)

	// Two reasons to stop the job from the host, each a cancellation followed
	// by a hard stop of the sandbox if the executor does not answer: a shutdown
	// of the runner (ShutdownGrace), and the job timeout, which the executor
	// enforces itself but a stuck executor would not (TimeoutGrace after the
	// deadline: the escalation SIGINT, SIGTERM, SIGKILL takes 10 s).
	stopHard := make(chan struct{})
	var hardOnce sync.Once
	hard := func() { hardOnce.Do(func() { close(stopHard) }) }
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
		select {
		case <-time.After(j.t.ShutdownGrace):
			hard()
		case <-ctx.Done():
		}
	}()
	go func() {
		select {
		case <-time.After(time.Until(j.deadline())):
		case <-ctx.Done():
			return
		}
		j.logf("the job timeout of %d minutes has passed", j.timeoutMinutes())
		j.requestCancel("the job timeout has passed", execpb.CancelKind_CANCEL_KIND_TIMEOUT)
		select {
		case <-time.After(j.t.TimeoutGrace):
			hard()
		case <-ctx.Done():
		}
	}()

	conclusion, outputs, shutdownFlag := j.execute(ctx, rt, stopHard)

	// 2. Nothing more is sent to a job that is over.
	if err := j.abandoned(); err != nil {
		j.logf("job %s: not reporting a result: %v", j.job.JobId, err)
		return
	}
	// The report is made even when the runner is shutting down: it uses its own
	// deadline.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.t.CompleteDeadline)
	defer cancel()
	// Output limits are checked before the log is flushed: a job whose outputs
	// the server would refuse fails without outputs, with a line in the log
	// (runner.proto: CompleteJobRequest.outputs), and lines sent after
	// CompleteJob are rejected.
	if msg := checkOutputs(outputs); msg != "" {
		j.sandboxError(msg)
		outputs, conclusion, shutdownFlag = nil, ermite.CiConclusion_CI_CONCLUSION_FAILURE, false
	}
	j.outbox.Close(rctx)
	if err := j.abandoned(); err != nil {
		return
	}
	j.complete(rctx, conclusion, outputs, shutdownFlag)
}

// execute starts the sandbox, runs the job in it and returns the conclusion.
func (j *jobRun) execute(ctx context.Context, rt *Runner, stopHard <-chan struct{}) (ermite.CiConclusion, map[string]string, bool) {
	fail := func(format string, a ...any) (ermite.CiConclusion, map[string]string, bool) {
		j.failEarly(fmt.Sprintf(format, a...))
		return ermite.CiConclusion_CI_CONCLUSION_FAILURE, nil, false
	}
	sb, err := rt.Driver.New()
	if err != nil {
		return fail("The runner has no free sandbox: %v", err)
	}
	// Stop runs to completion even when the job context is done.
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Stop(sctx); err != nil {
			j.logf("job %s: stopping the sandbox: %v", j.job.JobId, err)
			if errors.Is(err, sandbox.ErrLeaked) {
				rt.markLeaked()
			}
		}
	}()

	// Starting the sandbox is bounded by the job context and a hard stop.
	startCtx, cancelStart := context.WithCancel(ctx)
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-stopHard:
			cancelStart()
		case <-stopWatch:
		}
	}()
	conn, err := sb.Start(startCtx, rt.spec(j.job))
	close(stopWatch)
	cancelStart()
	if err != nil {
		if ctx.Err() != nil || j.cancelRequested() {
			return ermite.CiConclusion_CI_CONCLUSION_CANCELLED, nil, j.isStopping()
		}
		return fail("The sandbox did not start: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Hard stops (grace elapsed, or the job was abandoned) close the stream.
	go func() {
		select {
		case <-stopHard:
		case <-ctx.Done():
		}
		_ = conn.Close()
	}()

	run := &execpb.Run{Job: &execpb.JobSpec{
		WorkflowPath: j.job.WorkflowPath, WorkflowYaml: j.job.WorkflowYaml, JobKey: j.job.JobKey,
		Matrix: j.job.Matrix, Contexts: j.job.Contexts, Repo: j.job.Repo, TimeoutMinutes: int32(j.timeoutMinutes()),
		DeadlineMs: max(1, time.Until(j.deadline()).Milliseconds()),
		RunnerName: rt.Cfg.Name, RunnerVersion: rt.Cfg.Version, RunnerEnvironment: "self-hosted",
	}}
	_, exit, err := execstream.DriveJob(conn, run, j.cancelCh, execstream.JobHandler{
		StepStarted: j.stepStarted, StepCompleted: j.stepCompleted,
		Log: func(l *execpb.Log) {
			j.outbox.Log(logLine{step: l.Step, unixNano: l.UnixNano, text: l.Text, continued: l.Continued})
		},
		JobResult: func(r *execpb.JobResult) { j.mu.Lock(); j.result = r; j.mu.Unlock() },
	})
	j.mu.Lock()
	res := j.result
	j.mu.Unlock()

	switch {
	case res != nil:
		c := conclusionOf(res.Conclusion)
		if c == ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED || c == ermite.CiConclusion_CI_CONCLUSION_SKIPPED {
			c = ermite.CiConclusion_CI_CONCLUSION_FAILURE
		}
		if res.Error != "" {
			j.logf("job %s: %s", j.job.JobId, res.Error)
		}
		return c, res.Outputs, res.RunnerShutdown && c == ermite.CiConclusion_CI_CONCLUSION_CANCELLED
	case j.cancelRequested() || ctx.Err() != nil:
		// The executor did not report: it was stopped. A timeout wins over a
		// shutdown or a user cancel that came later, as on the server (the first
		// reason decides, and the timeout is the first when it fired first).
		switch {
		case j.firstCancelKind() == execpb.CancelKind_CANCEL_KIND_TIMEOUT:
			return ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT, nil, false
		case j.isStopping():
			return ermite.CiConclusion_CI_CONCLUSION_CANCELLED, nil, true
		}
		return ermite.CiConclusion_CI_CONCLUSION_CANCELLED, nil, false
	}
	why := "the executor ended without a result"
	if err != nil {
		why = err.Error()
	} else if exit != nil && exit.Error != "" {
		why = exit.Error
	}
	j.sandboxError(fmt.Sprintf("The job's sandbox ended unexpectedly: %s", why))
	return ermite.CiConclusion_CI_CONCLUSION_FAILURE, nil, false
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
	}
	b := backoff{min: j.t.BackoffMin, max: j.t.BackoffMax}
	for {
		cctx, cancel := context.WithTimeout(ctx, j.t.CallTimeout)
		resp, err := j.client.CompleteJob(cctx, connect.NewRequest(req))
		cancel()
		if err == nil {
			if resp.Msg.Conclusion != conclusion {
				j.logf("job %s: ended as %s, the server holds %s", j.job.JobId, conclusion, resp.Msg.Conclusion)
			}
			return
		}
		if ctx.Err() != nil {
			j.logf("job %s: could not report the result: %v", j.job.JobId, ctx.Err())
			return
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			return
		case kindPermanent:
			j.logf("job %s: CompleteJob refused: %v", j.job.JobId, err)
			return
		}
		j.logf("CompleteJob failed, retrying: %v", err)
		if !sleep(ctx, b.next()) {
			return
		}
	}
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
