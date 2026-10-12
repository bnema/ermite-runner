// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// Runner fetches jobs from ermite and runs each one in its own sandbox.
type Runner struct {
	Cfg    Config
	Driver sandbox.Driver
	// Poll is the client that carries the runner token (FetchJob).
	Poll ermiteconnect.RunnerServiceClient
	// Job builds the client of a job from its job token.
	Job    func(jobToken string) ermiteconnect.RunnerServiceClient
	Timing Timing
	Logf   func(format string, args ...any)
	// JobSpec sizes the sandbox of a job; the zero value takes the defaults.
	JobSpec func(*ermite.Job) sandbox.JobSpec

	leaked atomic.Bool
	active sync.WaitGroup
}

func (r *Runner) logf(format string, a ...any) {
	if r.Logf != nil {
		r.Logf(format, a...)
	}
}

func (r *Runner) spec(j *ermite.Job) sandbox.JobSpec {
	if r.JobSpec != nil {
		return r.JobSpec(j)
	}
	return sandbox.JobSpec{JobID: j.JobId}
}

// markLeaked records that a sandbox could not be cleaned up: no new job is
// taken until the running ones are over and Driver.Reap has succeeded.
func (r *Runner) markLeaked() { r.leaked.Store(true) }

// ErrRemoved is returned by Run when ermite no longer knows the runner token:
// the runner was removed or replaced.
var ErrRemoved = errors.New("ermite rejected the runner token: the runner was removed or replaced; register it again")

// newPollID is 16 random bytes in base64url: 22 characters of the set the
// protocol allows.
func newPollID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Run is the main loop. It returns nil after a graceful shutdown (ctx ended and
// every running job reported), or an error when the runner cannot go on.
//
// Capacity: a poll is made only when a slot is free. A lost response is retried
// with the same poll_id, so that the server hands back the same assignment.
func (r *Runner) Run(ctx context.Context) error {
	t := r.Timing.withDefaults()
	shutdown := make(chan struct{})
	slots := make(chan struct{}, max(1, r.Cfg.Capacity))
	defer func() {
		// Graceful shutdown: running jobs are cancelled with runner_shutdown and
		// get ShutdownGrace to stop; Run returns once they have reported.
		close(shutdown)
		r.active.Wait()
	}()

	if err := r.Driver.Reap(ctx); err != nil && !errors.Is(err, sandbox.ErrBusy) {
		r.logf("reaping what a previous run left: %v", err)
	}

	var pollID string
	bo := backoff{min: t.BackoffMin, max: t.BackoffMax}
	for ctx.Err() == nil {
		// Wait for a free slot, and for the end of a leak.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		if r.leaked.Load() {
			<-slots
			if !r.recover(ctx, slots) {
				return nil
			}
			continue
		}
		if pollID == "" {
			pollID = newPollID()
		}
		pctx, cancel := context.WithTimeout(ctx, t.PollTimeout)
		resp, err := r.Poll.FetchJob(pctx, connect.NewRequest(&ermite.FetchJobRequest{PollId: pollID}))
		cancel()
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return nil
			}
			if connect.CodeOf(err) == connect.CodeUnauthenticated {
				return ErrRemoved
			}
			if connect.CodeOf(err) == connect.CodeInvalidArgument {
				pollID = "" // our id was refused: a new one
			}
			d := bo.next()
			r.logf("FetchJob failed, retrying in %s: %v", d, err)
			if !sleep(ctx, d) {
				return nil
			}
			continue
		}
		bo.cur = 0
		pollID = "" // answered: the next poll is a new one
		if resp.Msg.Job == nil {
			<-slots
			d := t.NoJobPause
			if ra := resp.Msg.RetryAfter; ra != nil && ra.AsDuration() > d {
				d = ra.AsDuration()
			}
			if !sleep(ctx, d) {
				return nil
			}
			continue
		}
		r.startJob(ctx, resp.Msg.Job, t, shutdown, slots)
		if ra := resp.Msg.RetryAfter; ra != nil && ra.AsDuration() > 0 {
			if !sleep(ctx, ra.AsDuration()) {
				return nil
			}
		}
	}
	return nil
}

// recover waits until no job runs, then reaps. It returns false when ctx ends.
func (r *Runner) recover(ctx context.Context, slots chan struct{}) bool {
	r.logf("a sandbox could not be cleaned up: waiting for the running jobs, then reaping")
	for ctx.Err() == nil {
		if len(slots) == 0 {
			if err := r.Driver.Reap(ctx); err == nil {
				r.leaked.Store(false)
				r.logf("reaped: taking jobs again")
				return true
			} else if !errors.Is(err, sandbox.ErrBusy) {
				r.logf("reap failed, retrying: %v", err)
			}
		}
		if !sleep(ctx, 5*time.Second) {
			return false
		}
	}
	return false
}

func (r *Runner) startJob(ctx context.Context, job *ermite.Job, t Timing, shutdown <-chan struct{}, slots chan struct{}) {
	jctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	jr := &jobRun{
		r: r, job: job, client: r.Job(job.JobToken), t: t, ctx: jctx, cancel: cancel,
		logf:     func(f string, a ...any) { r.logf("job "+job.JobId+": "+f, a...) },
		cancelCh: make(chan *execpb.Cancel, 1), steps: map[int32]*ermite.StepUpdate{},
	}
	r.logf("job %s (run %d, attempt %d, %s) assigned", job.JobId, job.RunId, job.Attempt, job.JobKey)
	r.active.Add(1)
	go func() {
		defer r.active.Done()
		defer func() { <-slots }()
		jr.run(r, shutdown)
		r.logf("job %s finished", job.JobId)
	}()
}

// ---- registration

// Isolation maps the driver's isolation to the protocol's.
func Isolation(i sandbox.Isolation) ermite.RunnerIsolation {
	switch i {
	case sandbox.IsolationVM:
		return ermite.RunnerIsolation_RUNNER_ISOLATION_VM
	case sandbox.IsolationContainerSysbox:
		return ermite.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_SYSBOX
	case sandbox.IsolationContainerPrivileged:
		return ermite.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_PRIVILEGED
	}
	return ermite.RunnerIsolation_RUNNER_ISOLATION_UNSPECIFIED
}

// GoArch is the architecture in GitHub's names.
func GoArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

// Register registers the runner with ermite using a token that has the
// runner:register scope, and stores the runner token (mode 0600) in the state
// directory. replace removes a runner of the same name first.
func Register(ctx context.Context, cfg Config, client ermiteconnect.RunnerServiceClient, isolation sandbox.Isolation, replace bool) (State, error) {
	iso := Isolation(isolation)
	if iso == ermite.RunnerIsolation_RUNNER_ISOLATION_UNSPECIFIED {
		return State{}, fmt.Errorf("unknown isolation %q", isolation)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.RegisterRunner(rctx, connect.NewRequest(&ermite.RegisterRunnerRequest{
		Name: cfg.Name, Labels: cfg.Labels, Version: cfg.Version, Isolation: iso, Arch: GoArch(), Os: "linux", Replace: replace,
	}))
	if err != nil {
		return State{}, fmt.Errorf("register: %w", err)
	}
	st := State{URL: cfg.URL, RunnerID: resp.Msg.RunnerId, Name: cfg.Name, RunnerToken: resp.Msg.RunnerToken}
	if err := SaveState(cfg.StateDir, st); err != nil {
		return State{}, fmt.Errorf("the runner is registered but its token could not be stored: %w", err)
	}
	return st, nil
}
