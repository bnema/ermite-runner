// SPDX-License-Identifier: MIT

// Package host is the host side of ermite-runner: it registers the runner with
// ermite, fetches jobs, runs each one in a sandbox through the executor, and
// reports logs, steps and the result with the job token. The sandbox never sees
// a token.
package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// bearer adds "Authorization: Bearer <token>" to every request. The token is
// never logged. It refuses to send the token over a connection that is not
// protected: plain http is only for loopback, unless AllowInsecure is set.
type bearer struct {
	token         string
	base          http.RoundTripper
	allowInsecure bool
}

// ErrInsecureURL is returned for a server URL that is not https and not
// loopback: the runner token and the job tokens would travel in clear.
var ErrInsecureURL = errors.New("the server URL must be https (plain http is only accepted for localhost; ERMITE_RUNNER_INSECURE_HTTP=1 allows it, for tests, and sends every token in clear)")

// CheckURL validates the URL of the server: http or https, a host, no
// credentials, and https unless the host is loopback or allowInsecure is set.
func CheckURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	if u.User != nil {
		return errors.New("the server URL must not contain credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopback(u.Hostname()) {
			return nil
		}
		return ErrInsecureURL
	}
	return fmt.Errorf("%q: the scheme must be https", raw)
}

func isLoopback(host string) bool {
	// Host names are not case sensitive (LOCALHOST, Localhost).
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := CheckURL(r.URL.String(), b.allowInsecure); err != nil {
		return nil, err
	}
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
// (WatchJob is a long stream). It never follows a redirect: a response from the
// server that points elsewhere is returned as is, so that the token is not
// carried to another host (Go would keep the Authorization header on a redirect
// to the same host and drop it for others, but the client has no business
// following any).
func NewClient(baseURL, token string, base http.RoundTripper) ermiteconnect.RunnerServiceClient {
	return newClient(baseURL, token, base, false)
}

// NewInsecureClient is NewClient for a server reached over plain http that is
// not loopback. Only for tests and trusted networks.
func NewInsecureClient(baseURL, token string, base http.RoundTripper) ermiteconnect.RunnerServiceClient {
	return newClient(baseURL, token, base, true)
}

func newClient(baseURL, token string, base http.RoundTripper, insecure bool) ermiteconnect.RunnerServiceClient {
	hc := &http.Client{
		Transport:     bearer{token: token, base: base, allowInsecure: insecure},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return ermiteconnect.NewRunnerServiceClient(hc, strings.TrimRight(baseURL, "/"))
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
	// TimeoutGrace is how long the executor gets after the job timeout to stop
	// its steps (SIGINT, then SIGTERM after 7.5 s, then SIGKILL 2.5 s later) and
	// report, before the sandbox is destroyed.
	TimeoutGrace time.Duration
	// ShutdownGrace is how long jobs get to stop after a shutdown signal before
	// their sandbox is destroyed. The executor needs 10 s for its escalation
	// (SIGINT, SIGTERM after 7.5 s, SIGKILL 2.5 s later) and some for the
	// post-steps and the report to the host.
	ShutdownGrace time.Duration
	// StopDeadline bounds the destruction of a sandbox.
	StopDeadline time.Duration
	// ShutdownReport bounds the last flush of the log and CompleteJob of a job
	// that a shutdown cut short.
	ShutdownReport time.Duration
	// CompleteDeadline bounds the flush of logs and CompleteJob at the end of
	// a job.
	CompleteDeadline time.Duration
	// NoJobPause is the pause after a poll without a job and without
	// retry_after.
	NoJobPause time.Duration
	// ReapRetry is the delay between attempts to reap after a leaked sandbox.
	ReapRetry time.Duration
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
	def(&t.TimeoutGrace, 30*time.Second)
	def(&t.ShutdownGrace, 30*time.Second)
	def(&t.StopDeadline, 30*time.Second)
	def(&t.ShutdownReport, 30*time.Second)
	def(&t.CompleteDeadline, 5*time.Minute)
	def(&t.NoJobPause, 0)
	def(&t.ReapRetry, 5*time.Second)
	return t
}

// ShutdownBudget is the longest a graceful shutdown takes with these timings:
// the grace for the executor, the destruction of the sandbox, and the report.
// The jobs shut down in parallel, so it does not depend on their number. The
// service manager must wait at least this long (contrib/ermite-runner.service
// has TimeoutStopSec=120 for the defaults, 90 s).
func (t Timing) ShutdownBudget() time.Duration {
	t = t.withDefaults()
	return t.ShutdownGrace + t.StopDeadline + t.ShutdownReport
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
