// SPDX-License-Identifier: MIT

// Package hosttest is an in-memory RunnerService for tests that run the real
// host loop against a real sandbox. It enforces the rules of runner.proto that
// matter to a client: tokens per kind and per job, the offsets of AppendLog,
// the limits of a request, and the end of a job after CompleteJob.
package hosttest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// RunnerToken is the runner token the server accepts.
const RunnerToken = "ermiterun_test"

// Line is one stored log line.
type Line struct {
	Step      int32
	Text      string
	Continued bool
}

// Result is what a job reported.
type Result struct {
	// Annotations are those stored, by step (replace-per-step rule), and
	// Summaries the Markdown stored by step.
	Annotations map[int32][]*ermite.Annotation
	Summaries   map[int32]string
	Complete    *ermite.CompleteJobRequest
	Lines       []Line
	Steps       map[int32]*ermite.StepUpdate
}

// Server is the fake. Create it with New; URL is where to point the runner.
type Server struct {
	ermiteconnect.UnimplementedRunnerServiceHandler

	URL string
	srv *httptest.Server

	mu       sync.Mutex
	queue    []*ermite.Job
	jobs     map[string]*jobState
	assigned map[string]*ermite.Job // by job token
	// Errors lists contract violations seen (a gap in AppendLog, a request over
	// the limits, a call after the end): a test fails when it is not empty.
	Errors []string
	done   chan string
}

type jobState struct {
	annots   map[int32][]*ermite.Annotation
	sums     map[int32]string
	log      *LogStore
	steps    map[int32]*ermite.StepUpdate
	complete *ermite.CompleteJobRequest
}

// Mount serves a RunnerService handler where ermite does: under /-/api, so
// that a client that forgets the prefix fails here as it would against ermite.
func Mount(path string, h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/-/api"+path, http.StripPrefix("/-/api", h))
	return mux
}

// New starts the server; Close stops it.
func New() *Server {
	s := &Server{jobs: map[string]*jobState{}, assigned: map[string]*ermite.Job{}, done: make(chan string, 256)}
	s.srv = httptest.NewServer(Mount(ermiteconnect.NewRunnerServiceHandler(s)))
	s.URL = s.srv.URL
	return s
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Queue adds jobs to hand out, in order.
func (s *Server) Queue(jobs ...*ermite.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, jobs...)
}

// Violations returns the contract violations seen so far.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Errors...)
}

// Done receives the id of each job when its CompleteJob has been applied.
func (s *Server) Done() <-chan string { return s.done }

// Result returns what a job reported so far.
func (s *Server) Result(jobID string) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.jobs[jobID]
	if st == nil {
		return Result{}
	}
	steps := map[int32]*ermite.StepUpdate{}
	for k, v := range st.steps {
		steps[k] = v
	}
	anns := map[int32][]*ermite.Annotation{}
	for k, v := range st.annots {
		anns[k] = v
	}
	sums := map[int32]string{}
	for k, v := range st.sums {
		sums[k] = v
	}
	return Result{Complete: st.complete, Lines: st.log.Lines(), Steps: steps, Annotations: anns, Summaries: sums}
}

func (s *Server) violation(msg string) { s.Errors = append(s.Errors, msg) } // called with s.mu held

func unauth() error { return connect.NewError(connect.CodeUnauthenticated, io.EOF) }

func (s *Server) job(h http.Header, id string) (*jobState, error) {
	j := s.assigned[strings.TrimPrefix(h.Get("Authorization"), "Bearer ")]
	if j == nil {
		return nil, unauth()
	}
	if j.JobId != id {
		return nil, connect.NewError(connect.CodePermissionDenied, io.EOF)
	}
	return s.jobs[id], nil
}

func ended(c ermite.CiConclusion) error {
	e := connect.NewError(connect.CodeFailedPrecondition, io.EOF)
	if d, err := connect.NewErrorDetail(&ermite.JobEnded{Conclusion: c}); err == nil {
		e.AddDetail(d)
	}
	return e
}

// FetchJob hands out the queued jobs.
func (s *Server) FetchJob(_ context.Context, r *connect.Request[ermite.FetchJobRequest]) (*connect.Response[ermite.FetchJobResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header().Get("Authorization") != "Bearer "+RunnerToken {
		return nil, unauth()
	}
	if len(s.queue) == 0 {
		return connect.NewResponse(&ermite.FetchJobResponse{}), nil
	}
	j := s.queue[0]
	s.queue = s.queue[1:]
	s.assigned[j.JobToken] = j
	s.jobs[j.JobId] = &jobState{log: &LogStore{OnViolation: s.violation}, steps: map[int32]*ermite.StepUpdate{}, annots: map[int32][]*ermite.Annotation{}, sums: map[int32]string{}}
	return connect.NewResponse(&ermite.FetchJobResponse{Job: j}), nil
}

// Heartbeat accepts heartbeats of running jobs.
func (s *Server) Heartbeat(_ context.Context, r *connect.Request[ermite.HeartbeatRequest]) (*connect.Response[ermite.HeartbeatResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.job(r.Header(), r.Msg.JobId)
	if err != nil {
		return nil, err
	}
	if st.complete != nil {
		return nil, ended(st.complete.Conclusion)
	}
	return connect.NewResponse(&ermite.HeartbeatResponse{}), nil
}

// UpdateJob stores step snapshots.
func (s *Server) UpdateJob(_ context.Context, r *connect.Request[ermite.UpdateJobRequest]) (*connect.Response[ermite.UpdateJobResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.job(r.Header(), r.Msg.JobId)
	if err != nil {
		return nil, err
	}
	if st.complete != nil {
		return nil, ended(st.complete.Conclusion)
	}
	for _, u := range r.Msg.Steps {
		if old := st.steps[u.Number]; old != nil && old.State == ermite.StepState_STEP_STATE_COMPLETED {
			s.violation("UpdateJob: a completed step changed")
		}
		st.steps[u.Number] = u
	}
	replace(st.annots, r.Msg.Annotations)
	return connect.NewResponse(&ermite.UpdateJobResponse{}), nil
}

// AppendLog applies the rules of runner.proto through a LogStore.
func (s *Server) AppendLog(_ context.Context, r *connect.Request[ermite.AppendLogRequest]) (*connect.Response[ermite.AppendLogResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.job(r.Header(), r.Msg.JobId)
	if err != nil {
		return nil, err
	}
	if st.complete != nil {
		s.violation("AppendLog after CompleteJob")
		return nil, ended(st.complete.Conclusion)
	}
	next, err := st.log.Apply(r.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ermite.AppendLogResponse{NextOffset: next}), nil
}

// CompleteJob ends a job, idempotently.
func (s *Server) CompleteJob(_ context.Context, r *connect.Request[ermite.CompleteJobRequest]) (*connect.Response[ermite.CompleteJobResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.job(r.Header(), r.Msg.JobId)
	if err != nil {
		return nil, err
	}
	if st.complete != nil {
		return connect.NewResponse(&ermite.CompleteJobResponse{Conclusion: st.complete.Conclusion}), nil
	}
	st.complete = r.Msg
	for _, u := range r.Msg.Steps {
		st.steps[u.Number] = u
	}
	replace(st.annots, r.Msg.Annotations)
	s.done <- r.Msg.JobId
	return connect.NewResponse(&ermite.CompleteJobResponse{Conclusion: r.Msg.Conclusion, Applied: true}), nil
}

// WatchJob keeps the stream open until the job ends or the client leaves.
func (s *Server) WatchJob(ctx context.Context, r *connect.Request[ermite.WatchJobRequest], _ *connect.ServerStream[ermite.WatchJobResponse]) error {
	s.mu.Lock()
	_, err := s.job(r.Header(), r.Msg.JobId)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// replace applies the rule of runner.proto: the annotations of each step the
// call names replace those stored.
func replace(stored map[int32][]*ermite.Annotation, anns []*ermite.Annotation) {
	named := map[int32]bool{}
	for _, a := range anns {
		if !named[a.StepNumber] {
			named[a.StepNumber] = true
			stored[a.StepNumber] = nil
		}
		stored[a.StepNumber] = append(stored[a.StepNumber], a)
	}
}

// UploadSummary stores the summary of a step.
func (s *Server) UploadSummary(_ context.Context, r *connect.Request[ermite.UploadSummaryRequest]) (*connect.Response[ermite.UploadSummaryResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.job(r.Header(), r.Msg.JobId)
	if err != nil {
		return nil, err
	}
	if st.complete != nil {
		return nil, ended(st.complete.Conclusion)
	}
	st.sums[r.Msg.StepNumber] = r.Msg.Markdown
	return connect.NewResponse(&ermite.UploadSummaryResponse{}), nil
}
