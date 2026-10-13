// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/host/hosttest"
	"bnema.dev/ermite-runner/internal/sandbox"
)

// fakeServer is an in-memory RunnerService with the contract's rules for
// AppendLog (offsets), tokens and the end of a job. Hooks let a test inject
// failures.
type fakeServer struct {
	ermiteconnect.UnimplementedRunnerServiceHandler

	mu         sync.Mutex
	runnerTok  string
	jobs       []*ermite.Job // queued, handed out in order
	assigned   map[string]*ermite.Job
	polls      []string // poll ids seen
	pollHook   func(n int, req *ermite.FetchJobRequest) (*ermite.FetchJobResponse, error)
	pollCalls  int
	retryAfter time.Duration

	beats     []time.Time
	beatHook  func(n int) error
	registers []*ermite.RegisterRunnerRequest

	store      *hosttest.LogStore
	violations []string
	lines      []string // log text, one per line (mirror of store)
	cont       []bool
	steps      map[int32]*ermite.StepUpdate
	annots     map[int32][]*ermite.Annotation // by step, replaced by each call that names the step
	summaries  map[int32]string
	updates    []*ermite.UpdateJobRequest
	updateHook func(n int, req *ermite.UpdateJobRequest) error
	appendReq  []*ermite.AppendLogRequest
	appendHook func(n int, req *ermite.AppendLogRequest) (applyThenFail bool, err error)
	appendN    int

	completes    []*ermite.CompleteJobRequest
	completeHook func(n int) (applyThenFail bool, err error)
	completeN    int
	ended        *ermite.JobEnded // when set, job calls answer JobEnded
	completed    map[string]bool

	watchMsgs chan *ermite.WatchJobResponse
	watches   int
}

func newFakeServer() *fakeServer {
	s := &fakeServer{
		runnerTok: "ermiterun_tok", assigned: map[string]*ermite.Job{}, completed: map[string]bool{}, steps: map[int32]*ermite.StepUpdate{},
		watchMsgs: make(chan *ermite.WatchJobResponse, 16),
	}
	s.store = &hosttest.LogStore{OnViolation: func(m string) { s.violations = append(s.violations, m) }}
	return s
}

func endedErr(je *ermite.JobEnded) error {
	e := connect.NewError(connect.CodeFailedPrecondition, io.EOF)
	if d, err := connect.NewErrorDetail(je); err == nil {
		e.AddDetail(d)
	}
	return e
}

func (s *fakeServer) checkRunner(h http.Header) error {
	if h.Get("Authorization") != "Bearer "+s.runnerTok {
		return connect.NewError(connect.CodeUnauthenticated, io.EOF)
	}
	return nil
}

func (s *fakeServer) checkJob(h http.Header, id string) (*ermite.Job, error) {
	tok := strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
	for _, j := range s.assigned {
		if j.JobToken == tok {
			if j.JobId != id {
				return nil, connect.NewError(connect.CodePermissionDenied, io.EOF)
			}
			return j, nil
		}
	}
	return nil, connect.NewError(connect.CodeUnauthenticated, io.EOF)
}

func (s *fakeServer) RegisterRunner(_ context.Context, r *connect.Request[ermite.RegisterRunnerRequest]) (*connect.Response[ermite.RegisterRunnerResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registers = append(s.registers, r.Msg)
	if r.Header().Get("Authorization") != "Bearer reg-token" {
		return nil, connect.NewError(connect.CodePermissionDenied, io.EOF)
	}
	return connect.NewResponse(&ermite.RegisterRunnerResponse{RunnerId: "rid1", RunnerToken: "ermiterun_new"}), nil
}

func (s *fakeServer) FetchJob(_ context.Context, r *connect.Request[ermite.FetchJobRequest]) (*connect.Response[ermite.FetchJobResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkRunner(r.Header()); err != nil {
		return nil, err
	}
	s.pollCalls++
	s.polls = append(s.polls, r.Msg.PollId)
	if s.pollHook != nil {
		if resp, err := s.pollHook(s.pollCalls, r.Msg); resp != nil || err != nil {
			if resp != nil && resp.Job != nil {
				s.assigned[resp.Job.JobId] = resp.Job
			}
			return connect.NewResponse(resp), err
		}
	}
	if len(s.jobs) == 0 {
		resp := &ermite.FetchJobResponse{}
		if s.retryAfter > 0 {
			resp.RetryAfter = durationpbOf(s.retryAfter)
		}
		return connect.NewResponse(resp), nil
	}
	j := s.jobs[0]
	s.jobs = s.jobs[1:]
	s.assigned[j.JobId] = j
	return connect.NewResponse(&ermite.FetchJobResponse{Job: j}), nil
}

func (s *fakeServer) Heartbeat(_ context.Context, r *connect.Request[ermite.HeartbeatRequest]) (*connect.Response[ermite.HeartbeatResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.checkJob(r.Header(), r.Msg.JobId); err != nil {
		return nil, err
	}
	if s.ended != nil {
		return nil, endedErr(s.ended)
	}
	s.beats = append(s.beats, time.Now())
	if s.beatHook != nil {
		if err := s.beatHook(len(s.beats)); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&ermite.HeartbeatResponse{}), nil
}

func (s *fakeServer) UpdateJob(_ context.Context, r *connect.Request[ermite.UpdateJobRequest]) (*connect.Response[ermite.UpdateJobResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.checkJob(r.Header(), r.Msg.JobId); err != nil {
		return nil, err
	}
	if s.ended != nil || s.completed[r.Msg.JobId] {
		je := s.ended
		if je == nil {
			je = &ermite.JobEnded{Conclusion: ermite.CiConclusion_CI_CONCLUSION_SUCCESS}
		}
		return nil, endedErr(je)
	}
	s.updates = append(s.updates, r.Msg)
	if s.updateHook != nil {
		if err := s.updateHook(len(s.updates), r.Msg); err != nil {
			return nil, err
		}
	}
	for _, u := range r.Msg.Steps {
		s.steps[u.Number] = u
	}
	s.replaceAnnotations(r.Msg.Annotations)
	return connect.NewResponse(&ermite.UpdateJobResponse{}), nil
}

// replaceAnnotations applies the replace-per-step rule of runner.proto.
func (s *fakeServer) replaceAnnotations(anns []*ermite.Annotation) {
	if s.annots == nil {
		s.annots = map[int32][]*ermite.Annotation{}
	}
	named := map[int32]bool{}
	for _, a := range anns {
		if !named[a.StepNumber] {
			named[a.StepNumber] = true
			s.annots[a.StepNumber] = nil
		}
		s.annots[a.StepNumber] = append(s.annots[a.StepNumber], a)
	}
}

func (s *fakeServer) UploadSummary(_ context.Context, r *connect.Request[ermite.UploadSummaryRequest]) (*connect.Response[ermite.UploadSummaryResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.checkJob(r.Header(), r.Msg.JobId); err != nil {
		return nil, err
	}
	if s.summaries == nil {
		s.summaries = map[int32]string{}
	}
	s.summaries[r.Msg.StepNumber] = r.Msg.Markdown
	return connect.NewResponse(&ermite.UploadSummaryResponse{}), nil
}

func (s *fakeServer) AppendLog(_ context.Context, r *connect.Request[ermite.AppendLogRequest]) (*connect.Response[ermite.AppendLogResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.checkJob(r.Header(), r.Msg.JobId); err != nil {
		return nil, err
	}
	if s.ended != nil || s.completed[r.Msg.JobId] {
		je := s.ended
		if je == nil {
			je = &ermite.JobEnded{Conclusion: ermite.CiConclusion_CI_CONCLUSION_SUCCESS}
		}
		return nil, endedErr(je)
	}
	s.appendN++
	s.appendReq = append(s.appendReq, r.Msg)
	// The rules of runner.proto are written once, in hosttest.LogStore.
	var failAfter error
	if s.appendHook != nil {
		thenFail, err := s.appendHook(s.appendN, r.Msg)
		if err != nil && !thenFail {
			return nil, err
		}
		failAfter = err
	}
	next, err := s.store.Apply(r.Msg)
	if err != nil {
		return nil, err
	}
	s.lines, s.cont = s.lines[:0], s.cont[:0]
	for _, l := range s.store.Lines() {
		s.lines = append(s.lines, l.Text)
		s.cont = append(s.cont, l.Continued)
	}
	if failAfter != nil {
		return nil, failAfter
	}
	return connect.NewResponse(&ermite.AppendLogResponse{NextOffset: next}), nil
}

func (s *fakeServer) CompleteJob(_ context.Context, r *connect.Request[ermite.CompleteJobRequest]) (*connect.Response[ermite.CompleteJobResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.checkJob(r.Header(), r.Msg.JobId); err != nil {
		return nil, err
	}
	s.completeN++
	s.completes = append(s.completes, r.Msg)
	s.replaceAnnotations(r.Msg.Annotations)
	if s.completeHook != nil {
		thenFail, err := s.completeHook(s.completeN)
		if err != nil {
			if thenFail {
				s.completed[r.Msg.JobId] = true
			}
			return nil, err
		}
	}
	if s.completed[r.Msg.JobId] || s.ended != nil {
		c := ermite.CiConclusion_CI_CONCLUSION_FAILURE
		if s.ended != nil {
			c = s.ended.Conclusion
		}
		return connect.NewResponse(&ermite.CompleteJobResponse{Conclusion: c, Applied: false}), nil
	}
	s.completed[r.Msg.JobId] = true
	return connect.NewResponse(&ermite.CompleteJobResponse{Conclusion: r.Msg.Conclusion, Applied: true}), nil
}

func (s *fakeServer) WatchJob(ctx context.Context, r *connect.Request[ermite.WatchJobRequest], st *connect.ServerStream[ermite.WatchJobResponse]) error {
	s.mu.Lock()
	_, err := s.checkJob(r.Header(), r.Msg.JobId)
	s.watches++
	ended := s.ended
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if ended != nil {
		return endedErr(ended)
	}
	for {
		select {
		case m := <-s.watchMsgs:
			if err := st.Send(m); err != nil {
				return err
			}
			if m.GetEnded() != nil {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// start serves the handler over HTTP/1.1 on a local port (Connect streams server messages over it) and returns its URL.
func (s *fakeServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(hosttest.Mount(ermiteconnect.NewRunnerServiceHandler(s)))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *fakeServer) snapshotLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// ---- fake sandbox: the "executor" is a function on the other end of a pipe

type script func(t *testing.T, c *execstream.Conn, run *execpb.Run, cancels <-chan *execpb.Cancel)

type fakeDriver struct {
	mu         sync.Mutex
	script     script
	t          *testing.T
	news       int
	stops      int
	started    int
	reaps      int
	reapErr    error
	startErr   error
	stopErr    error
	capacity   int
	live       int
	blockStart chan struct{}     // when set, Start waits on it
	specs      []sandbox.JobSpec // what each Start was asked for
	iso        sandbox.Isolation
}

func (d *fakeDriver) Mode() sandbox.Mode           { return sandbox.Container }
func (d *fakeDriver) Isolation() sandbox.Isolation { return d.iso }
func (d *fakeDriver) Close() error                 { return nil }
func (d *fakeDriver) Reap(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reaps++
	return d.reapErr
}
func (d *fakeDriver) New() (sandbox.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.capacity > 0 && d.live >= d.capacity {
		return nil, sandbox.ErrCapacity
	}
	d.news++
	d.live++
	return &fakeSandbox{d: d}, nil
}

type fakeSandbox struct {
	d      *fakeDriver
	mu     sync.Mutex
	conn   net.Conn
	stoped bool
}

func (s *fakeSandbox) ID() string { return "er00000000" }
func (s *fakeSandbox) Start(ctx context.Context, spec sandbox.JobSpec) (sandbox.Conn, error) {
	d := s.d
	d.mu.Lock()
	d.specs = append(d.specs, spec)
	d.mu.Unlock()
	if d.blockStart != nil {
		select {
		case <-d.blockStart:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	if d.startErr != nil {
		err := d.startErr
		d.mu.Unlock()
		return nil, err
	}
	d.started++
	sc := d.script
	d.mu.Unlock()
	host, guest := net.Pipe()
	s.mu.Lock()
	s.conn = guest
	s.mu.Unlock()
	go func() {
		defer func() { _ = guest.Close() }()
		c := execstream.NewConn(guest)
		_ = c.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{Version: "test", Sandbox: "container"}}})
		f, err := c.Recv()
		if err != nil || f.GetRun() == nil {
			return
		}
		cancels := make(chan *execpb.Cancel, 4)
		go func() {
			for {
				f, err := c.Recv()
				if err != nil {
					close(cancels)
					return
				}
				if cn := f.GetCancel(); cn != nil {
					cancels <- cn
				}
			}
		}()
		sc(d.t, c, f.GetRun(), cancels)
	}()
	return host, nil
}

func (s *fakeSandbox) Stop(context.Context) error {
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	first := !s.stoped
	s.stoped = true
	s.mu.Unlock()
	if first {
		s.d.mu.Lock()
		s.d.stops++
		s.d.live--
		err := s.d.stopErr
		s.d.mu.Unlock()
		return err
	}
	return nil
}

// ---- script helpers

func send(c *execstream.Conn, b *execpb.Frame) { _ = c.Send(b) }

func stepStart(c *execstream.Conn, n int, name string) {
	send(c, &execpb.Frame{Body: &execpb.Frame_StepStarted{StepStarted: &execpb.StepStarted{Number: int32(n), Name: name, UnixNano: time.Now().UnixNano()}}})
}
func stepDone(c *execstream.Conn, n int, name string, concl execpb.StepConclusion) {
	send(c, &execpb.Frame{Body: &execpb.Frame_StepCompleted{StepCompleted: &execpb.StepCompleted{Number: int32(n), Name: name, Conclusion: concl, UnixNano: time.Now().UnixNano()}}})
}
func logLineFrame(c *execstream.Conn, n int, text string, continued bool) {
	send(c, &execpb.Frame{Body: &execpb.Frame_Log{Log: &execpb.Log{Step: int32(n), UnixNano: time.Now().UnixNano(), Text: text, Continued: continued}}})
}
func jobDone(c *execstream.Conn, concl execpb.StepConclusion, outputs map[string]string, shutdown bool) {
	send(c, &execpb.Frame{Body: &execpb.Frame_JobResult{JobResult: &execpb.JobResult{Conclusion: concl, Outputs: outputs, RunnerShutdown: shutdown}}})
	send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
}

const (
	cOK     = execpb.StepConclusion_STEP_CONCLUSION_SUCCESS
	cFail   = execpb.StepConclusion_STEP_CONCLUSION_FAILURE
	cCancel = execpb.StepConclusion_STEP_CONCLUSION_CANCELLED
)

// simpleScript runs a one-step job that logs the given lines.
func simpleScript(lines ...string) script {
	return func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepDone(c, 1, "Set up job", cOK)
		stepStart(c, 2, "Run it")
		for _, l := range lines {
			logLineFrame(c, 2, l, false)
		}
		stepDone(c, 2, "Run it", cOK)
		stepStart(c, 3, "Complete job")
		stepDone(c, 3, "Complete job", cOK)
		jobDone(c, cOK, map[string]string{"out": "v"}, false)
	}
}

// ---- harness

func testJob(id string) *ermite.Job {
	return &ermite.Job{
		JobId: id, RunId: 7, Attempt: 1, JobToken: "ermitejob_" + id, WorkflowPath: ".github/workflows/ci.yml",
		WorkflowYaml: "on: push\njobs:\n  j:\n    runs-on: x\n    steps: [{run: echo}]\n", JobKey: "j", Matrix: "{}",
		Contexts: `{"github":{"repository":"o/r"}}`, Repo: "r", TimeoutMinutes: 5,
	}
}

func fastTiming() Timing {
	return Timing{
		PollTimeout: 5 * time.Second, HeartbeatEvery: 100 * time.Millisecond, HeartbeatRetry: 30 * time.Millisecond,
		CallTimeout: 5 * time.Second, BackoffMin: 10 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
		LogFlush: 5 * time.Millisecond, WatchRetry: 20 * time.Millisecond, ShutdownGrace: 2 * time.Second,
		CompleteDeadline: 10 * time.Second, NoJobPause: 10 * time.Millisecond,
	}
}

type harness struct {
	srv    *fakeServer
	url    string
	driver *fakeDriver
	runner *Runner
	logs   *logBuf
}

// logBuf collects what the runner logs (zerowrap, console format without
// colors), so that tests can look at it.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// ctx returns a context with a logger that writes to the buffer (JSON, one
// event per line, debug level).
func (l *logBuf) ctx() context.Context {
	return zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: l}))
}

func newHarness(t *testing.T, sc script) *harness {
	t.Helper()
	srv := newFakeServer()
	url := srv.start(t)
	drv := &fakeDriver{script: sc, t: t, iso: sandbox.IsolationVM}
	lb := &logBuf{}
	h := &harness{srv: srv, url: url, driver: drv, logs: lb}
	t.Cleanup(func() {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if len(srv.violations) > 0 {
			t.Errorf("the host broke the contract of AppendLog: %q", srv.violations)
		}
	})
	h.runner = &Runner{
		Cfg:      Config{URL: url, Name: "r1", Capacity: 1, Version: "test"},
		Driver:   drv,
		Poll:     NewClient(url, "ermiterun_tok", http.DefaultTransport),
		Features: func(context.Context) ([]string, error) { return []string{FeatureAnnotations, FeatureSummaries}, nil },
		Job:      func(tok string) ermiteconnect.RunnerServiceClient { return NewClient(url, tok, http.DefaultTransport) },
		Timing:   fastTiming(),
	}
	return h
}

// run starts the loop; stop cancels it and waits for Run to return.
func (h *harness) run(t *testing.T) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(h.logs.ctx())
	done := make(chan error, 1)
	go func() { done <- h.runner.Run(ctx) }()
	var once sync.Once
	var res error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case res = <-done:
			case <-time.After(20 * time.Second):
				t.Error("Run did not return after shutdown")
			}
		})
		return res
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *fakeServer) completeCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.completes) }

// Result returns what a job reported, like hosttest.Server.Result.
func (s *fakeServer) Result(jobID string) hosttest.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hosttest.Result{Steps: s.steps}
}
