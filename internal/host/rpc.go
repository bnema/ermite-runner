// SPDX-License-Identifier: MIT

// Package host is the host side of ermite-runner: it registers the runner with
// ermite, fetches jobs, runs each one in a sandbox through the executor, and
// reports logs, steps and the result with the job token. The sandbox never sees
// a token.
package host

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// bearer adds "Authorization: Bearer <token>" to every request. The token is
// never logged.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// NewClient returns a RunnerService client that authenticates with token. The
// HTTP client has no overall timeout: calls carry their own context deadline
// (WatchJob is a long stream).
func NewClient(baseURL, token string, base http.RoundTripper) ermiteconnect.RunnerServiceClient {
	return ermiteconnect.NewRunnerServiceClient(&http.Client{Transport: bearer{token, base}}, strings.TrimRight(baseURL, "/"))
}

// ClientFactory builds a client for a token; tests replace it.
type ClientFactory func(token string) ermiteconnect.RunnerServiceClient

// jobEnded extracts the JobEnded detail of a failed_precondition error.
func jobEnded(err error) (*ermite.JobEnded, bool) {
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		return nil, false
	}
	for _, d := range ce.Details() {
		m, derr := d.Value()
		if derr != nil {
			continue
		}
		if je, ok := m.(*ermite.JobEnded); ok {
			return je, true
		}
	}
	return nil, false
}

// kind classifies the error of a job call.
type kind int

const (
	kindRetry     kind = iota // network error, unavailable, internal...: try again
	kindEnded                 // failed_precondition with JobEnded: stop, send nothing more
	kindAbandoned             // unauthenticated: the assignment is gone, stop at once
	kindPermanent             // the request itself is wrong: do not repeat it
)

func classify(err error) kind {
	if err == nil {
		return kindRetry
	}
	if _, ok := jobEnded(err); ok {
		return kindEnded
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		return kindAbandoned
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeOutOfRange,
		connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeUnimplemented, connect.CodeAlreadyExists:
		return kindPermanent
	}
	// Canceled by our own context is handled by the callers through ctx.Err().
	return kindRetry
}

// Timing holds the delays of the host loop. Zero fields take the defaults,
// which are the protocol's.
type Timing struct {
	// PollTimeout bounds one FetchJob call (the server holds it up to 50 s).
	PollTimeout time.Duration
	// HeartbeatEvery is the heartbeat period (30 s); HeartbeatRetry the delay
	// after a failed heartbeat (5 s).
	HeartbeatEvery, HeartbeatRetry time.Duration
	// CallTimeout bounds one unary job call.
	CallTimeout time.Duration
	// BackoffMin and BackoffMax bound the retry delay of calls that fail
	// without a verdict.
	BackoffMin, BackoffMax time.Duration
	// LogFlush is how long log lines are held to fill a batch.
	LogFlush time.Duration
	// WatchRetry is the delay before WatchJob is reopened.
	WatchRetry time.Duration
	// ShutdownGrace is how long jobs get to stop after a shutdown signal before
	// their sandbox is destroyed.
	ShutdownGrace time.Duration
	// CompleteDeadline bounds the flush of logs and CompleteJob at the end of
	// a job.
	CompleteDeadline time.Duration
	// NoJobPause is the pause after a poll without a job and without
	// retry_after.
	NoJobPause time.Duration
}

func (t Timing) withDefaults() Timing {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&t.PollTimeout, 60*time.Second)
	def(&t.HeartbeatEvery, 30*time.Second)
	def(&t.HeartbeatRetry, 5*time.Second)
	def(&t.CallTimeout, 30*time.Second)
	def(&t.BackoffMin, time.Second)
	def(&t.BackoffMax, 30*time.Second)
	def(&t.LogFlush, 250*time.Millisecond)
	def(&t.WatchRetry, time.Second)
	def(&t.ShutdownGrace, 30*time.Second)
	def(&t.CompleteDeadline, 5*time.Minute)
	def(&t.NoJobPause, 0)
	return t
}

// sleep waits d or until ctx ends; it reports whether the full delay passed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// backoff doubles from min to max.
type backoff struct{ min, max, cur time.Duration }

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else if b.cur *= 2; b.cur > b.max {
		b.cur = b.max
	}
	return b.cur
}
