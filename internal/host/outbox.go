// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/types/known/timestamppb"

	ermite "bnema.dev/ermite-api/gen/ermite"
)

// Limits of AppendLog (runner.proto): at most 1,000 lines and 64 KiB of text
// per request, all in the same step.
const (
	maxBatchLines = 1000
	maxBatchBytes = 64 * 1024
	// maxQueuedBytes is the log kept in memory while the server cannot be
	// reached; beyond it the executor is slowed down (the stream is not read)
	// rather than the host's memory filled.
	maxQueuedBytes = 16 << 20
)

type logLine struct {
	step      int32
	unixNano  int64
	text      string
	continued bool
}

// event is something to send, in the order it happened: a log line, or a
// snapshot of a step (UpdateJob).
type event struct {
	line *logLine
	step *ermite.StepUpdate
	// anns are the annotations that go with a step snapshot, by step number:
	// the whole list of each step they name (replace semantics).
	anns map[int32][]*ermite.Annotation
	// summary is a step summary for UploadSummary.
	summary *summary
}

// summary is the Markdown of one step.
type summary struct {
	step     int32
	markdown string
}

// outbox sends logs and step updates of one job to ermite, in order, one call
// at a time. Log lines are numbered from 0 across the whole job; the number of
// the next line is the offset of the next AppendLog, so that a retry after a
// lost response sends the same lines at the same offset (the server skips what
// it already holds).
type outbox struct {
	client callClient
	jobID  string
	t      Timing
	// onEnded is called once when the server says the job is over (JobEnded)
	// or the assignment is gone; the outbox then drops everything.
	onEnded func(error)
	log     zerowrap.Logger

	mu      sync.Mutex
	cond    *sync.Cond
	events  []event
	queued  int // bytes of log text in events
	closing bool
	dead    bool
	done    chan struct{}

	// pendingAnns are the annotations the server may not hold yet, by step: the
	// last list queued for the step, until an UpdateJob that carried it
	// succeeded. CompleteJob sends what is left.
	pendingAnns map[int32][]*ermite.Annotation

	offset int64 // lines the server holds, as far as we know
	// offsetMaybeStale is set when a send was cut short by its context while
	// the call may have been applied by the server with the response lost: the
	// offset is then possibly behind the server's count.
	offsetMaybeStale bool
}

// callClient is the part of the RunnerService client the job uses.
type callClient interface {
	UpdateJob(context.Context, *connect.Request[ermite.UpdateJobRequest]) (*connect.Response[ermite.UpdateJobResponse], error)
	UploadSummary(context.Context, *connect.Request[ermite.UploadSummaryRequest]) (*connect.Response[ermite.UploadSummaryResponse], error)
	AppendLog(context.Context, *connect.Request[ermite.AppendLogRequest]) (*connect.Response[ermite.AppendLogResponse], error)
}

func newOutbox(c callClient, jobID string, t Timing, onEnded func(error), log zerowrap.Logger) *outbox {
	o := &outbox{client: c, jobID: jobID, t: t, onEnded: onEnded, log: log, done: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	return o
}

// start runs the sender until Close or until ctx ends.
func (o *outbox) start(ctx context.Context) {
	go func() {
		defer close(o.done)
		o.loop(ctx)
	}()
	// A cancelled context must wake the loop and the producers.
	go func() {
		select {
		case <-ctx.Done():
			o.kill()
		case <-o.done:
		}
	}()
}

func (o *outbox) kill() {
	o.mu.Lock()
	o.dead = true
	o.events = nil
	o.queued = 0
	o.mu.Unlock()
	o.cond.Broadcast()
}

// Log queues a line of the job log. It blocks while too much is queued.
func (o *outbox) Log(l logLine) {
	o.mu.Lock()
	for o.queued > maxQueuedBytes && !o.dead && !o.closing {
		o.cond.Wait()
	}
	if o.dead {
		o.mu.Unlock()
		return
	}
	o.events = append(o.events, event{line: &l})
	o.queued += len(l.text) + 32
	o.mu.Unlock()
	o.cond.Broadcast()
}

// Step queues a step snapshot, with the annotations that changed (may be nil).
func (o *outbox) Step(s *ermite.StepUpdate, anns map[int32][]*ermite.Annotation) {
	o.mu.Lock()
	if o.dead {
		o.mu.Unlock()
		return
	}
	for n, l := range anns {
		if o.pendingAnns == nil {
			o.pendingAnns = map[int32][]*ermite.Annotation{}
		}
		o.pendingAnns[n] = l
	}
	o.events = append(o.events, event{step: s, anns: anns})
	o.mu.Unlock()
	o.cond.Broadcast()
}

// Summary queues the summary of a step. Its bytes count with the log text in
// the bounded memory of the outbox: it blocks while too much is queued.
func (o *outbox) Summary(step int32, markdown string) {
	o.mu.Lock()
	for o.queued > maxQueuedBytes && !o.dead && !o.closing {
		o.cond.Wait()
	}
	if o.dead {
		o.mu.Unlock()
		return
	}
	o.events = append(o.events, event{summary: &summary{step, markdown}})
	o.queued += len(markdown) + 32
	o.mu.Unlock()
	o.cond.Broadcast()
}

// PendingAnnotations returns the annotations that no successful UpdateJob has
// carried, for CompleteJob, in step order.
func (o *outbox) PendingAnnotations() []*ermite.Annotation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return flattenAnnotations(o.pendingAnns)
}

func flattenAnnotations(m map[int32][]*ermite.Annotation) []*ermite.Annotation {
	steps := make([]int32, 0, len(m))
	for n := range m {
		steps = append(steps, n)
	}
	sort.Slice(steps, func(a, b int) bool { return steps[a] < steps[b] })
	var out []*ermite.Annotation
	for _, n := range steps {
		out = append(out, m[n]...)
	}
	return out
}

// Close sends what is queued, then stops the sender. It waits at most until
// ctx ends.
func (o *outbox) Close(ctx context.Context) {
	o.mu.Lock()
	o.closing = true
	o.mu.Unlock()
	o.cond.Broadcast()
	select {
	case <-o.done:
	case <-ctx.Done():
		o.kill()
		<-o.done
	}
}

// Lines returns how many lines the server holds (as far as we know).
func (o *outbox) Lines() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.offset
}

// next waits for a batch of events. It returns nil when the outbox is done.
func (o *outbox) next(ctx context.Context) []event {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.events) == 0 {
		if o.dead || o.closing {
			return nil
		}
		o.cond.Wait()
	}
	// Let a burst of lines fill the batch, unless it is full, a step event is
	// at the front, or we are closing.
	if o.events[0].line != nil && !o.closing && o.queued < maxBatchBytes && len(o.events) < maxBatchLines {
		o.mu.Unlock()
		sleep(ctx, o.t.LogFlush)
		o.mu.Lock()
		if o.dead {
			return nil
		}
	}
	if len(o.events) == 0 {
		return nil
	}
	first := o.events[0]
	if first.step != nil || first.summary != nil {
		o.events = o.events[1:]
		if first.summary != nil {
			o.queued -= len(first.summary.markdown) + 32
			o.cond.Broadcast() // producers waiting for room
		}
		return []event{first}
	}
	// A run of lines of the same step, within the limits.
	n, bytes := 0, 0
	step := first.line.step
	for _, e := range o.events {
		if e.line == nil || e.line.step != step || n == maxBatchLines || (n > 0 && bytes+len(e.line.text) > maxBatchBytes) {
			break
		}
		n++
		bytes += len(e.line.text)
	}
	batch := append([]event(nil), o.events[:n]...)
	o.events = o.events[n:]
	for _, e := range batch {
		o.queued -= len(e.line.text) + 32
	}
	o.cond.Broadcast() // producers waiting for room
	return batch
}

func (o *outbox) loop(ctx context.Context) {
	for {
		batch := o.next(ctx)
		if batch == nil {
			return
		}
		switch {
		case batch[0].step != nil:
			o.sendStep(ctx, batch[0].step, batch[0].anns)
		case batch[0].summary != nil:
			o.sendSummary(ctx, batch[0].summary)
		default:
			o.sendLines(ctx, batch)
		}
		o.mu.Lock()
		dead := o.dead
		o.mu.Unlock()
		if dead {
			return
		}
	}
}

// retry runs call until it succeeds, the server gives a verdict, or ctx ends.
// It returns nil on success.
func (o *outbox) retry(ctx context.Context, what string, call func(context.Context) error) error {
	b := backoff{min: o.t.BackoffMin, max: o.t.BackoffMax}
	for {
		cctx, cancel := context.WithTimeout(ctx, o.t.CallTimeout)
		err := call(cctx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			o.onEnded(err)
			o.kill()
			return err
		case kindPermanent:
			return err
		}
		o.log.Warn().Err(err).Str("call", what).Msg("call failed, retrying")
		if !sleep(ctx, b.next()) {
			return ctx.Err()
		}
	}
}

func (o *outbox) sendStep(ctx context.Context, s *ermite.StepUpdate, anns map[int32][]*ermite.Annotation) {
	req := &ermite.UpdateJobRequest{JobId: o.jobID, Steps: []*ermite.StepUpdate{s}, Annotations: flattenAnnotations(anns)}
	err := o.retry(ctx, "UpdateJob", func(c context.Context) error {
		_, err := o.client.UpdateJob(c, connect.NewRequest(req))
		return err
	})
	if err == nil {
		// The server holds these lists now, unless a later one replaced them.
		o.mu.Lock()
		for n, l := range anns {
			if cur := o.pendingAnns[n]; len(cur) == len(l) && (len(l) == 0 || &cur[0] == &l[0]) {
				delete(o.pendingAnns, n)
			}
		}
		o.mu.Unlock()
		return
	}
	if ctx.Err() == nil && classify(err) == kindPermanent {
		o.log.Error().Err(err).Int32("step", s.Number).Msg("UpdateJob refused")
		if len(anns) > 0 {
			// The annotations may be what the server refused: the step update
			// alone must not be lost with them.
			o.mu.Lock()
			for n := range anns {
				delete(o.pendingAnns, n)
			}
			o.mu.Unlock()
			if err := o.retry(ctx, "UpdateJob", func(c context.Context) error {
				_, err := o.client.UpdateJob(c, connect.NewRequest(&ermite.UpdateJobRequest{JobId: o.jobID, Steps: []*ermite.StepUpdate{s}}))
				return err
			}); err != nil && ctx.Err() == nil && classify(err) == kindPermanent {
				o.log.Error().Err(err).Int32("step", s.Number).Msg("UpdateJob refused without annotations")
			}
		}
	}
}

func (o *outbox) sendSummary(ctx context.Context, s *summary) {
	err := o.retry(ctx, "UploadSummary", func(c context.Context) error {
		_, err := o.client.UploadSummary(c, connect.NewRequest(&ermite.UploadSummaryRequest{JobId: o.jobID, StepNumber: s.step, Markdown: s.markdown}))
		return err
	})
	if err != nil && ctx.Err() == nil && classify(err) == kindPermanent {
		o.log.Error().Err(err).Int32("step", s.step).Msg("UploadSummary refused, the summary is dropped")
	}
}

func (o *outbox) sendLines(ctx context.Context, batch []event) {
	req := &ermite.AppendLogRequest{JobId: o.jobID, Step: batch[0].line.step}
	for _, e := range batch {
		req.Lines = append(req.Lines, &ermite.LogLine{Time: timestamppb.New(time.Unix(0, e.line.unixNano)), Text: e.line.text, Continued: e.line.continued})
	}
	// The offset is fixed before the first attempt: every retry sends the same
	// lines at the same offset.
	req.Offset = o.offset
	var next int64
	err := o.retry(ctx, "AppendLog", func(c context.Context) error {
		resp, err := o.client.AppendLog(c, connect.NewRequest(req))
		if err != nil {
			return err
		}
		next = resp.Msg.NextOffset
		return nil
	})
	switch {
	case err == nil:
		o.mu.Lock()
		o.offsetMaybeStale = false
		// The server says how many lines it holds: that is the truth. A
		// difference means lines were stored that we did not count (a retry
		// whose answer we lost) or dropped; continue from the server's count,
		// so that the next offset is never a gap (out_of_range) and never a
		// repeat of lines that would be skipped.
		if want := req.Offset + int64(len(req.Lines)); next != want {
			o.log.Warn().Int64("server_lines", next).Int64("expected", want).Msg("AppendLog: the server holds another number of lines, continuing from its count")
		}
		o.offset = next
		o.mu.Unlock()
	case ctx.Err() != nil:
		// Cut short in the middle of a call or of its retries: the server may
		// hold these lines without our having seen the answer.
		o.mu.Lock()
		o.offsetMaybeStale = true
		o.mu.Unlock()
	case classify(err) == kindPermanent:
		// The server refused these lines; they are dropped and the offset stays
		// where the server is.
		o.log.Error().Err(err).Int("lines", len(req.Lines)).Int32("step", req.Step).Msg("AppendLog refused, the lines are dropped")
	}
}

// AppendNow sends one line synchronously, after Close: the sender has stopped,
// so nothing else uses the offset. It is for the last words of a job (an error
// that explains why the result could not be reported).
//
// If Close gave up in the middle of a send, the offset may be behind the
// server's: the line is sent anyway (the server answers with its own count, and
// an offset that is too low makes it skip the line as a repeat or refuse it),
// and the warning says why it may be missing.
func (o *outbox) AppendNow(ctx context.Context, l logLine) {
	o.mu.Lock()
	stale := o.offsetMaybeStale
	offset := o.offset
	o.mu.Unlock()
	if stale {
		o.log.Warn().Int64("offset", offset).Msg("the log offset may be stale: Close was cut short in the middle of a send, so the last line may be skipped or refused by the server")
	}
	o.sendLines(ctx, []event{{line: &l}})
}
