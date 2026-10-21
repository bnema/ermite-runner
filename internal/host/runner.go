// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/actionscache"
	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/resultssvc"
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
	// Features lists the features of the server (GetVersion). Annotations and
	// step summaries are sent only when it says "ci.annotations" and
	// "ci.summaries". Nil means neither is sent.
	Features ServerFeatures
	// Cache, when not nil, is the cache service of the runner; the sandbox
	// drivers serve it on a channel of each sandbox (see sandbox.Config.Cache),
	// which the guest reaches on its loopback (cachesvc.DefaultPort), and each
	// job gets a token scoped to its repository and refs.
	Cache *cachesvc.Service
	// Archives, when not nil (and Cache is), is the host's cache of action
	// archives: the host downloads them and the sandbox reads them from the cache
	// channel (see cacheArchives). ArchiveHTTP is its HTTP client (tests); nil
	// means the default transport.
	Archives    *actionscache.Cache
	ArchiveHTTP *http.Client
	// Results, when not nil, is the results service of the runner (artifacts
	// v4). A job gets it when Cache is set too (it is served on the cache
	// channel and checks the cache's runtime tokens), Artifacts is set and the
	// server has the feature ci.artifacts.
	Results *resultssvc.Service
	// Artifacts builds the JobArtifactService client of a job from its job
	// token.
	Artifacts func(jobToken string) ermiteconnect.JobArtifactServiceClient

	// now returns the current time; tests replace it.
	now func() time.Time

	leaked atomic.Bool
	active sync.WaitGroup
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// spec is the sandbox request of a job. It names the job and leaves CPUs,
// memory and disk at zero: the driver fills them from its configuration
// (sandbox.Config.Resolve in Start), that is from ERMITE_RUNNER_JOB_CPUS,
// _JOB_MEMORY and _JOB_DISK, so that the limits are one setting of the runner
// and not something a job or this package can change.
//
// results is the job's results service, nil without one.
func (r *Runner) spec(j *ermite.Job, results http.Handler) sandbox.JobSpec {
	return sandbox.JobSpec{JobID: j.JobId, Results: results}
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
	log := zerowrap.FromCtx(ctx)
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
		log.Error().Err(err).Msg("reaping what a previous run left")
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
			if !r.recover(ctx, log, slots, t.ReapRetry) {
				return nil
			}
			continue
		}
		if pollID == "" {
			pollID = newPollID()
		}
		pctx, cancel := context.WithTimeout(ctx, t.PollTimeout)
		resp, err := r.Poll.FetchJob(pctx, connect.NewRequest(&ermite.FetchJobRequest{PollId: pollID, Version: r.Cfg.Version}))
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
			log.Warn().Err(err).Dur("retry_in", d).Msg("FetchJob failed")
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
		r.startJob(ctx, log, resp.Msg.Job, t, shutdown, slots)
		if ra := resp.Msg.RetryAfter; ra != nil && ra.AsDuration() > 0 {
			if !sleep(ctx, ra.AsDuration()) {
				return nil
			}
		}
	}
	return nil
}

// recover waits until no job runs, then reaps. It returns false when ctx ends.
func (r *Runner) recover(ctx context.Context, log zerowrap.Logger, slots chan struct{}, retry time.Duration) bool {
	log.Error().Msg("a sandbox could not be cleaned up: no new job until the running ones end and the leftovers are reaped")
	for ctx.Err() == nil {
		if len(slots) == 0 {
			if err := r.Driver.Reap(ctx); err == nil {
				r.leaked.Store(false)
				log.Info().Msg("reaped: taking jobs again")
				return true
			} else if !errors.Is(err, sandbox.ErrBusy) {
				log.Error().Err(err).Msg("reaping failed, retrying")
			}
		}
		if !sleep(ctx, retry) {
			return false
		}
	}
	return false
}

func (r *Runner) startJob(ctx context.Context, log zerowrap.Logger, job *ermite.Job, t Timing, shutdown <-chan struct{}, slots chan struct{}) {
	jctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	jr := &jobRun{
		r: r, job: job, client: r.Job(job.JobToken), t: t, ctx: jctx, cancel: cancel,
		log:        zerowrap.Logger{Logger: log.With().Str("job_id", job.JobId).Int64("run_id", job.RunId).Int32("attempt", job.Attempt).Logger()},
		receivedAt: r.clock(), cancelCh: make(chan *execpb.Cancel, 1), steps: map[int32]*ermite.StepUpdate{},
		annSeen: map[int32]bool{}, sumSeen: map[int32]bool{}, timeline: timeline{},
	}
	jr.log.Info().Str("job_key", job.JobKey).Msg("job assigned")
	r.active.Add(1)
	go func() {
		defer r.active.Done()
		defer func() { <-slots }()
		jr.run(r, shutdown)
		jr.log.Info().Fields(jr.timingFields()).Msg("job finished")
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
