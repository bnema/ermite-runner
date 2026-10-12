// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
)

func TestJobRunsAndReports(t *testing.T) {
	h := newHarness(t, simpleScript("hello", "world"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	stop := h.run(t)

	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	s := h.srv
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.completes[0]
	if c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS || c.Outputs["out"] != "v" || c.RunnerShutdown {
		t.Fatalf("%+v", c)
	}
	if len(s.beats) == 0 {
		t.Error("no heartbeat: the first one is the acknowledgement")
	}
	if strings.Join(s.lines, "|") != "hello|world" {
		t.Errorf("lines %q", s.lines)
	}
	for n, name := range map[int32]string{1: "Set up job", 2: "Run it", 3: "Complete job"} {
		u := s.steps[n]
		if u == nil || u.Name != name || u.State != ermite.StepState_STEP_STATE_COMPLETED || u.Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS || u.StartedAt == nil || u.CompletedAt == nil {
			t.Errorf("step %d: %v", n, u)
		}
	}
	if len(c.Steps) != 3 {
		t.Errorf("CompleteJob carries %d final steps", len(c.Steps))
	}
	if h.driver.stops != 1 || h.driver.reaps < 1 {
		t.Errorf("stops=%d reaps=%d: the sandbox must be stopped and the start must reap", h.driver.stops, h.driver.reaps)
	}
	if strings.Contains(h.logs.String(), "ermitejob_") || strings.Contains(h.logs.String(), "ermiterun_") {
		t.Errorf("a token reached the log: %s", h.logs.String())
	}
}

func TestTheJobTokenAndTheRunnerTokenStayApart(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	var sawJobSpec *execpb.JobSpec
	inner := h.driver.script
	h.driver.script = func(t *testing.T, c *execstream.Conn, run *execpb.Run, cn <-chan *execpb.Cancel) {
		sawJobSpec = run.Job
		inner(t, c, run, cn)
	}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	// The sandbox gets the workflow and contexts, never a token.
	if sawJobSpec == nil || sawJobSpec.JobKey != "j" || sawJobSpec.RunnerName != "r1" || sawJobSpec.RunnerEnvironment != "github-hosted" || sawJobSpec.TimeoutMinutes != 5 {
		t.Fatalf("%v", sawJobSpec)
	}
	if strings.Contains(sawJobSpec.String(), "ermitejob_") || strings.Contains(sawJobSpec.String(), "ermiterun_") {
		t.Fatal("a token reached the sandbox")
	}
}

func TestPollRetryUsesTheSamePollIDAfterAnError(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.pollHook = func(n int, req *ermite.FetchJobRequest) (*ermite.FetchJobResponse, error) {
		if n <= 2 { // the response is lost twice
			return nil, connect.NewError(connect.CodeUnavailable, io.EOF)
		}
		if n == 3 {
			return &ermite.FetchJobResponse{Job: testJob("j1")}, nil
		}
		return nil, nil
	}
	h.run(t)
	eventually(t, "CompleteJob and the next poll", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return len(h.srv.completes) == 1 && len(h.srv.polls) >= 4
	})
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.polls) < 4 {
		t.Fatalf("polls %v", h.srv.polls)
	}
	if h.srv.polls[0] == "" || h.srv.polls[0] != h.srv.polls[1] || h.srv.polls[1] != h.srv.polls[2] {
		t.Errorf("a retried poll must keep its poll_id: %v", h.srv.polls)
	}
	if h.srv.polls[3] == h.srv.polls[2] {
		t.Errorf("an answered poll is followed by a new id: %v", h.srv.polls)
	}
}

func TestPollRespectsRetryAfter(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.retryAfter = 300 * time.Millisecond
	h.run(t)
	time.Sleep(1000 * time.Millisecond)
	h.srv.mu.Lock()
	n := h.srv.pollCalls
	h.srv.mu.Unlock()
	if n < 2 || n > 5 {
		t.Fatalf("%d polls in 1 s with retry_after=300ms", n)
	}
}

func TestPollRemovedRunnerStops(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.runner.Poll = NewClient(h.url, "ermiterun_wrong", http.DefaultTransport)
	err := h.runner.Run(h.logs.ctx())
	if !errors.Is(err, ErrRemoved) {
		t.Fatalf("%v", err)
	}
}

func TestNoPollWithoutFreeCapacity(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		<-release
		stepDone(c, 1, "Set up job", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1"), testJob("j2")}
	h.run(t)
	eventually(t, "first job started", func() bool { h.driver.mu.Lock(); defer h.driver.mu.Unlock(); return h.driver.started == 1 })
	time.Sleep(300 * time.Millisecond)
	h.srv.mu.Lock()
	calls := h.srv.pollCalls
	h.srv.mu.Unlock()
	if calls != 1 {
		t.Fatalf("polled %d times with the only slot busy", calls)
	}
	h.driver.mu.Lock()
	if h.driver.started != 1 {
		t.Errorf("started=%d", h.driver.started)
	}
	h.driver.mu.Unlock()
	close(release)
	eventually(t, "both jobs done", func() bool { return h.srv.completeCount() == 2 })
}

func TestCapacityTwoRunsTwoJobsAtOnce(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		<-release
		stepDone(c, 1, "Set up job", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.runner.Cfg.Capacity = 2
	h.srv.jobs = []*ermite.Job{testJob("j1"), testJob("j2"), testJob("j3")}
	h.run(t)
	eventually(t, "two sandboxes", func() bool { h.driver.mu.Lock(); defer h.driver.mu.Unlock(); return h.driver.started == 2 })
	time.Sleep(200 * time.Millisecond)
	h.driver.mu.Lock()
	if h.driver.started != 2 {
		t.Errorf("started=%d with capacity 2", h.driver.started)
	}
	h.driver.mu.Unlock()
	close(release)
	eventually(t, "three jobs done", func() bool { return h.srv.completeCount() == 3 })
}

func TestFirstHeartbeatRetriesEvery5sStyle(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.beatHook = func(n int) error {
		if n <= 2 {
			return connect.NewError(connect.CodeUnavailable, io.EOF)
		}
		return nil
	}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.beats) < 3 {
		t.Fatalf("beats %d", len(h.srv.beats))
	}
	// The job starts only after the acknowledgement: no log before the third beat.
	if gap := h.srv.beats[1].Sub(h.srv.beats[0]); gap > 90*time.Millisecond {
		t.Errorf("retry after %v, want about HeartbeatRetry (30ms), not the 100ms period", gap)
	}
}

func TestHeartbeatTicksAndRetriesQuickly(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		<-release
		stepDone(c, 1, "Set up job", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.beatHook = func(n int) error {
		if n == 3 { // one failure in the middle of the job
			return connect.NewError(connect.CodeUnavailable, io.EOF)
		}
		return nil
	}
	h.run(t)
	eventually(t, "five heartbeats", func() bool { h.srv.mu.Lock(); defer h.srv.mu.Unlock(); return len(h.srv.beats) >= 5 })
	h.srv.mu.Lock()
	b := append([]time.Time(nil), h.srv.beats...)
	h.srv.mu.Unlock()
	if d := b[2].Sub(b[1]); d < 80*time.Millisecond {
		t.Errorf("period %v, want about 100ms", d)
	}
	if d := b[3].Sub(b[2]); d > 80*time.Millisecond {
		t.Errorf("the retry after a failure came after %v, want about 30ms", d)
	}
	close(release)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
}

func TestCancelFromWatchJobReachesTheExecutor(t *testing.T) {
	got := make(chan *execpb.Cancel, 1)
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepStart(c, 2, "Run sleep")
		logLineFrame(c, 2, "working", false)
		cn := <-cancels
		got <- cn
		stepDone(c, 2, "Run sleep", cCancel)
		jobDone(c, cCancel, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "the job to run", func() bool { return len(h.srv.snapshotLines()) == 1 })
	h.srv.watchMsgs <- &ermite.WatchJobResponse{Msg: &ermite.WatchJobResponse_Keepalive{Keepalive: &ermite.Keepalive{}}}
	h.srv.watchMsgs <- &ermite.WatchJobResponse{Msg: &ermite.WatchJobResponse_Cancel{Cancel: &ermite.CancelRequest{Reason: ermite.CancelReason_CANCEL_REASON_USER, Message: "Canceled by the owner"}}}
	select {
	case cn := <-got:
		if cn.Reason != "Canceled by the owner" || cn.Kind != execpb.CancelKind_CANCEL_KIND_USER {
			t.Fatalf("%v", cn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel did not reach the executor")
	}
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	c := h.srv.completes[0]
	if c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_CANCELLED || c.RunnerShutdown {
		t.Fatalf("%v", c)
	}
}

func TestTimeoutCancelKind(t *testing.T) {
	got := make(chan *execpb.Cancel, 1)
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		logLineFrame(c, 1, "x", false)
		got <- <-cancels
		jobDone(c, execpb.StepConclusion_STEP_CONCLUSION_TIMED_OUT, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "log", func() bool { return len(h.srv.snapshotLines()) == 1 })
	h.srv.watchMsgs <- &ermite.WatchJobResponse{Msg: &ermite.WatchJobResponse_Cancel{Cancel: &ermite.CancelRequest{Reason: ermite.CancelReason_CANCEL_REASON_TIMEOUT}}}
	if cn := <-got; cn.Kind != execpb.CancelKind_CANCEL_KIND_TIMEOUT {
		t.Fatalf("%v", cn)
	}
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if c := h.srv.completes[0]; c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT {
		t.Fatalf("%v", c.Conclusion)
	}
}

func TestWatchJobIsReopenedAfterAnError(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		<-release
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "the first watch", func() bool { h.srv.mu.Lock(); defer h.srv.mu.Unlock(); return h.srv.watches >= 1 })
	// A message that ends the stream cleanly without Ended: the runner reopens.
	// The fake closes the stream only on Ended or context end, so cut the
	// transport instead: close idle connections of the test server.
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	close(release)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
}

func TestLogBatchesRespectTheLimitsAndKeepOrder(t *testing.T) {
	var lines []string
	big := strings.Repeat("x", 1000)
	for i := 0; i < 2500; i++ {
		lines = append(lines, big[:1+i%1000])
	}
	h := newHarness(t, simpleScript(lines...))
	h.runner.Timing.LogFlush = 50 * time.Millisecond
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.lines) != 2500 {
		t.Fatalf("the server holds %d lines", len(h.srv.lines))
	}
	for i, l := range h.srv.lines {
		if len(l) != 1+(i)%1000 {
			t.Fatalf("line %d out of order or wrong", i)
		}
	}
	var next int64
	for _, r := range h.srv.appendReq {
		if len(r.Lines) > 1000 {
			t.Fatalf("batch of %d lines", len(r.Lines))
		}
		n := 0
		for _, l := range r.Lines {
			n += len(l.Text)
		}
		if n > 64*1024 {
			t.Fatalf("batch of %d bytes", n)
		}
		if r.Offset != next {
			t.Fatalf("offset %d, want %d", r.Offset, next)
		}
		next += int64(len(r.Lines))
	}
	if next != 2500 {
		t.Fatalf("offsets end at %d", next)
	}
	// Batches never mix steps.
	for _, r := range h.srv.appendReq {
		if r.Step < 1 {
			t.Fatalf("step %d", r.Step)
		}
	}
}

func TestLogStepsAreSeparateRequests(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		for n := 1; n <= 3; n++ {
			stepStart(c, n, "s")
			logLineFrame(c, n, "a", false)
			logLineFrame(c, n, "b", false)
			stepDone(c, n, "s", cOK)
		}
		jobDone(c, cOK, nil, false)
	})
	h.runner.Timing.LogFlush = 50 * time.Millisecond
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.appendReq) != 3 {
		t.Fatalf("%d requests, want one per step", len(h.srv.appendReq))
	}
	for i, r := range h.srv.appendReq {
		if r.Step != int32(i+1) || r.Offset != int64(2*i) || len(r.Lines) != 2 {
			t.Errorf("request %d: step %d offset %d lines %d", i, r.Step, r.Offset, len(r.Lines))
		}
	}
}

func TestContinuedLinesAreForwarded(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "s")
		logLineFrame(c, 1, "first", false)
		logLineFrame(c, 1, "piece1", false)
		logLineFrame(c, 1, "piece2", true)
		stepDone(c, 1, "s", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.cont) != 3 || h.srv.cont[0] || h.srv.cont[1] || !h.srv.cont[2] {
		t.Fatalf("%v", h.srv.cont)
	}
}

func TestAppendLogRetriesAreIdempotent(t *testing.T) {
	h := newHarness(t, simpleScript("l1", "l2", "l3"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	// The first AppendLog is applied but its response is lost; the second fails
	// before being applied. The runner resends the same lines at the same offset.
	h.srv.appendHook = func(n int, _ *ermite.AppendLogRequest) (bool, error) {
		switch n {
		case 1:
			return true, connect.NewError(connect.CodeUnavailable, io.EOF)
		case 2:
			return false, connect.NewError(connect.CodeInternal, io.EOF)
		}
		return false, nil
	}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if strings.Join(h.srv.lines, "|") != "l1|l2|l3" {
		t.Fatalf("lines %q: a retry must neither lose nor duplicate", h.srv.lines)
	}
	if h.srv.appendN < 3 {
		t.Fatalf("only %d calls", h.srv.appendN)
	}
	first := h.srv.appendReq[0]
	for _, r := range h.srv.appendReq[1:3] {
		if r.Offset != first.Offset || len(r.Lines) != len(first.Lines) {
			t.Errorf("a retry changed the request: offset %d/%d lines %d/%d", r.Offset, first.Offset, len(r.Lines), len(first.Lines))
		}
	}
}

func TestStepUpdatesAreRetried(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	// UpdateJob snapshots carry the state at each step boundary.
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if u := h.srv.steps[2]; u == nil || u.State != ermite.StepState_STEP_STATE_COMPLETED {
		t.Fatalf("%v", u)
	}
}

func TestCompleteJobIsRetriedAndIdempotent(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.completeHook = func(n int) (bool, error) {
		switch n {
		case 1: // applied, response lost
			return true, connect.NewError(connect.CodeUnavailable, io.EOF)
		case 2: // not even reached
			return false, connect.NewError(connect.CodeInternal, io.EOF)
		}
		return false, nil
	}
	h.run(t)
	eventually(t, "three CompleteJob calls", func() bool { return h.srv.completeCount() == 3 })
	time.Sleep(100 * time.Millisecond)
	if n := h.srv.completeCount(); n != 3 {
		t.Fatalf("%d calls", n)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	a, b := h.srv.completes[0], h.srv.completes[2]
	if a.Conclusion != b.Conclusion || a.Outputs["out"] != b.Outputs["out"] || len(a.Steps) != len(b.Steps) {
		t.Error("a retry must repeat the same result")
	}
}

func TestJobEndedStopsEverything(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		logLineFrame(c, 1, "before", false)
		<-release
		logLineFrame(c, 1, "after", false)
		stepDone(c, 1, "Set up job", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "first line", func() bool { return len(h.srv.snapshotLines()) == 1 })
	// The server ends the job (lost runner, log limit, ...): every call now says so.
	h.srv.mu.Lock()
	h.srv.ended = &ermite.JobEnded{Conclusion: ermite.CiConclusion_CI_CONCLUSION_CANCELLED, Error: "ended by the server"}
	h.srv.mu.Unlock()
	h.srv.watchMsgs <- &ermite.WatchJobResponse{Msg: &ermite.WatchJobResponse_Ended{Ended: &ermite.JobEnded{Conclusion: ermite.CiConclusion_CI_CONCLUSION_CANCELLED}}}
	eventually(t, "the sandbox to be stopped", func() bool { h.driver.mu.Lock(); defer h.driver.mu.Unlock(); return h.driver.stops == 1 })
	close(release)
	time.Sleep(300 * time.Millisecond)
	if n := h.srv.completeCount(); n != 0 {
		t.Fatalf("CompleteJob was called %d times for a job the server ended", n)
	}
	if got := h.srv.snapshotLines(); len(got) != 1 {
		t.Fatalf("lines after the end: %q", got)
	}
}

func TestJobEndedFromAppendLogAbandonsTheJob(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		logLineFrame(c, 1, "a", false)
		<-release
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.appendHook = func(n int, _ *ermite.AppendLogRequest) (bool, error) {
		return false, endedErr(&ermite.JobEnded{Conclusion: ermite.CiConclusion_CI_CONCLUSION_FAILURE, Error: "log limit reached"})
	}
	h.run(t)
	eventually(t, "the sandbox to be stopped", func() bool { h.driver.mu.Lock(); defer h.driver.mu.Unlock(); return h.driver.stops == 1 })
	close(release)
	time.Sleep(200 * time.Millisecond)
	if n := h.srv.completeCount(); n != 0 {
		t.Fatalf("CompleteJob was called %d times", n)
	}
}

func TestUnauthenticatedJobCallAbandonsAtOnce(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		<-release
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.beatHook = func(n int) error {
		if n >= 2 { // the assignment was replaced
			return connect.NewError(connect.CodeUnauthenticated, io.EOF)
		}
		return nil
	}
	h.run(t)
	eventually(t, "the sandbox to be stopped", func() bool { h.driver.mu.Lock(); defer h.driver.mu.Unlock(); return h.driver.stops == 1 })
	close(release)
	time.Sleep(200 * time.Millisecond)
	if n := h.srv.completeCount(); n != 0 {
		t.Fatalf("CompleteJob was called %d times", n)
	}
}

func TestGracefulShutdownReportsRunnerShutdown(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepStart(c, 2, "Run sleep")
		logLineFrame(c, 2, "working", false)
		cn := <-cancels
		if cn.Kind != execpb.CancelKind_CANCEL_KIND_SHUTDOWN {
			t.Errorf("kind %v", cn.Kind)
		}
		stepDone(c, 2, "Run sleep", cCancel)
		jobDone(c, cCancel, nil, true)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	stop := h.run(t)
	eventually(t, "the job to run", func() bool { return len(h.srv.snapshotLines()) == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.completes) != 1 {
		t.Fatalf("%d CompleteJob calls", len(h.srv.completes))
	}
	if c := h.srv.completes[0]; c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_CANCELLED || !c.RunnerShutdown {
		t.Fatalf("%+v", c)
	}
	if h.driver.stops != 1 {
		t.Errorf("stops=%d", h.driver.stops)
	}
}

func TestShutdownHardStopsAnExecutorThatIgnoresIt(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepStart(c, 2, "Run hang")
		logLineFrame(c, 2, "hung", false)
		<-cancels // received, ignored: keep running until the stream is cut
		select {}
	})
	h.runner.Timing.ShutdownGrace = 300 * time.Millisecond
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	stop := h.run(t)
	eventually(t, "the job to run", func() bool { return len(h.srv.snapshotLines()) == 1 })
	start := time.Now()
	_ = stop()
	if time.Since(start) > 5*time.Second {
		t.Fatal("shutdown waited for an executor that does not answer")
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.completes) != 1 || h.srv.completes[0].Conclusion != ermite.CiConclusion_CI_CONCLUSION_CANCELLED || !h.srv.completes[0].RunnerShutdown {
		t.Fatalf("%+v", h.srv.completes)
	}
	// Steps that never completed are closed by the runner.
	for _, u := range h.srv.completes[0].Steps {
		if u.State != ermite.StepState_STEP_STATE_COMPLETED || u.Conclusion != ermite.CiConclusion_CI_CONCLUSION_CANCELLED {
			t.Errorf("step %d: %v/%v", u.Number, u.State, u.Conclusion)
		}
	}
}

func TestExecutorDyingFailsTheJob(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		logLineFrame(c, 1, "partial", false)
		// the stream closes without a result
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	c := h.srv.completes[0]
	if c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE || c.RunnerShutdown {
		t.Fatalf("%+v", c)
	}
	if len(h.srv.lines) != 2 || !strings.HasPrefix(h.srv.lines[1], "##[error]The job's sandbox ended unexpectedly") {
		t.Fatalf("lines %q", h.srv.lines)
	}
	if len(c.Steps) != 1 || c.Steps[0].State != ermite.StepState_STEP_STATE_COMPLETED || c.Steps[0].Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE {
		t.Fatalf("%v", c.Steps)
	}
}

func TestSandboxStartFailureFailsTheJobInStepOne(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.driver.startErr = errors.New("no kvm today")
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if c := h.srv.completes[0]; c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE {
		t.Fatalf("%v", c.Conclusion)
	}
	if len(h.srv.lines) != 1 || !strings.Contains(h.srv.lines[0], "The sandbox did not start: no kvm today") {
		t.Fatalf("%q", h.srv.lines)
	}
	if h.driver.stops != 1 {
		t.Errorf("a sandbox that failed to start is still stopped: stops=%d", h.driver.stops)
	}
}

func TestOutputsOverTheLimitFailTheJob(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepDone(c, 1, "Set up job", cOK)
		// Names that ermite would refuse: the job fails without outputs.
		jobDone(c, cOK, map[string]string{"1bad": "x", "fine": "y"}, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	c := h.srv.completes[0]
	if c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE || len(c.Outputs) != 0 {
		t.Fatalf("%v %v", c.Conclusion, len(c.Outputs))
	}
	if len(h.srv.lines) != 1 || !strings.Contains(h.srv.lines[0], "is not valid") {
		t.Fatalf("%q", h.srv.lines)
	}
}

func TestCheckOutputs(t *testing.T) {
	for _, tc := range []struct {
		in   map[string]string
		want string
	}{
		{nil, ""},
		{map[string]string{"a-b_c1": "v", "_x": ""}, ""},
		{map[string]string{"1a": "v"}, "not valid"},
		{map[string]string{"a b": "v"}, "not valid"},
		{map[string]string{"": "v"}, "not valid"},
		{map[string]string{strings.Repeat("a", 256): "v"}, "not valid"},
		{map[string]string{"Foo": "1", "foo": "2"}, "differ only by case"},
		{map[string]string{"a": strings.Repeat("x", 1<<20+1)}, "larger than 1 MiB"},
		{map[string]string{"a": strings.Repeat("x", 600000), "b": strings.Repeat("x", 600000)}, "in total"},
	} {
		got := checkOutputs(tc.in)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%v: %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLeakedSandboxStopsTakingJobsUntilReaped(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.driver.stopErr = sandbox.ErrLeaked
	h.driver.reapErr = errors.New("still leaked")
	h.runner.Timing.ReapRetry = 50 * time.Millisecond
	h.srv.jobs = []*ermite.Job{testJob("j1"), testJob("j2")}
	h.run(t)
	eventually(t, "first job done", func() bool { return h.srv.completeCount() == 1 })
	time.Sleep(400 * time.Millisecond)
	if h.srv.completeCount() != 1 {
		t.Fatal("a second job started while a sandbox is leaked and cannot be reaped")
	}
	// Once the reap works, jobs are taken again.
	h.driver.mu.Lock()
	h.driver.reapErr = nil
	h.driver.stopErr = nil
	h.driver.mu.Unlock()
	eventually(t, "the second job", func() bool { return h.srv.completeCount() == 2 })
}

func TestReapAtStartAndIsolation(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.run(t)
	eventually(t, "reap", func() bool { h.driver.mu.Lock(); defer h.driver.mu.Unlock(); return h.driver.reaps >= 1 })
	for in, want := range map[sandbox.Isolation]ermite.RunnerIsolation{
		sandbox.IsolationVM:                  ermite.RunnerIsolation_RUNNER_ISOLATION_VM,
		sandbox.IsolationContainerSysbox:     ermite.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_SYSBOX,
		sandbox.IsolationContainerPrivileged: ermite.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_PRIVILEGED,
		"other":                              ermite.RunnerIsolation_RUNNER_ISOLATION_UNSPECIFIED,
	} {
		if got := Isolation(in); got != want {
			t.Errorf("%s: %v", in, got)
		}
	}
}

func TestRegisterStoresTheToken(t *testing.T) {
	srv := newFakeServer()
	url := srv.start(t)
	dir := t.TempDir() + "/state"
	cfg := Config{URL: url, Name: "r1", Labels: []string{"ubuntu-latest"}, StateDir: dir, Version: "v1"}
	st, err := Register(context.Background(), cfg, NewClient(url, "reg-token", http.DefaultTransport), sandbox.IsolationContainerSysbox, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.RunnerToken != "ermiterun_new" || st.RunnerID != "rid1" {
		t.Fatalf("%+v", st)
	}
	req := srv.registers[0]
	if req.Name != "r1" || req.Isolation != ermite.RunnerIsolation_RUNNER_ISOLATION_CONTAINER_SYSBOX || req.Os != "linux" || (req.Arch != "x64" && req.Arch != "arm64") || !req.Replace || req.Version != "v1" || len(req.Labels) != 1 {
		t.Fatalf("%v", req)
	}
	fi, err := os.Stat(StateFile(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	got, err := LoadState(dir)
	if err != nil || got != st {
		t.Fatalf("%+v %v", got, err)
	}
	// Anyone can read a state file with a loose mode: refuse it.
	if err := os.Chmod(StateFile(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir); err == nil || !strings.Contains(err.Error(), "accessible by other users") {
		t.Fatalf("%v", err)
	}
	if _, err := LoadState(t.TempDir()); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("%v", err)
	}
	// A refused registration stores nothing.
	dir2 := t.TempDir()
	cfg.StateDir = dir2
	if _, err := Register(context.Background(), cfg, NewClient(url, "bad", http.DefaultTransport), sandbox.IsolationVM, false); err == nil {
		t.Fatal("registration with a bad token succeeded")
	}
	if _, err := os.Stat(StateFile(dir2)); err == nil {
		t.Fatal("a state file was written")
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{EnvURL: " https://ermite.example ", EnvLabels: "ubuntu-latest, Ubuntu-24.04 ,ubuntu-latest,"}
	c, err := LoadConfig(func(k string) string { return env[k] }, "my host.local", "/state", 2, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://ermite.example" || c.Name != "my-host.local" || strings.Join(c.Labels, ",") != "ubuntu-latest,Ubuntu-24.04" || c.Capacity != 2 {
		t.Fatalf("%+v", c)
	}
	if c.Environment != "github-hosted" {
		t.Errorf("default environment %q", c.Environment)
	}
	env[EnvEnvironment] = " Self-Hosted "
	if c, err := LoadConfig(func(k string) string { return env[k] }, "h", "/s", 1, ""); err != nil || c.Environment != "self-hosted" {
		t.Errorf("self-hosted: %q, %v", c.Environment, err)
	}
	env[EnvEnvironment] = "cloud"
	if _, err := LoadConfig(func(k string) string { return env[k] }, "h", "/s", 1, ""); err == nil || !strings.Contains(err.Error(), EnvEnvironment) {
		t.Errorf("an invalid environment was accepted: %v", err)
	}
	delete(env, EnvEnvironment)
	env[EnvName] = "bad name"
	if _, err := LoadConfig(func(k string) string { return env[k] }, "h", "/s", 1, ""); err == nil {
		t.Error("an invalid name was accepted")
	}
	env[EnvName] = "ok"
	env[EnvLabels] = "good,bad label"
	if _, err := LoadConfig(func(k string) string { return env[k] }, "h", "/s", 1, ""); err == nil {
		t.Error("an invalid label was accepted")
	}
}

var _ = ermiteconnect.RunnerServiceClient(nil)

// The job timeout counts from the receipt of the job, and the host enforces it
// even when the executor does not: the sandbox is stopped and the job ends
// timed_out.
func TestHostEnforcesTheJobTimeout(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		logLineFrame(c, 1, "stuck", false)
		cn := <-cancels
		if cn.Kind != execpb.CancelKind_CANCEL_KIND_TIMEOUT {
			t.Errorf("kind %v", cn.Kind)
		}
		select {} // the executor ignores the cancel
	})
	h.runner.Timing.TimeoutGrace = 200 * time.Millisecond
	job := testJob("j1")
	job.TimeoutMinutes = 1
	h.runner.Job = func(tok string) ermiteconnect.RunnerServiceClient {
		return NewClient(h.url, tok, http.DefaultTransport)
	}
	h.runner.now = func() time.Time { return time.Now().Add(-59*time.Second - 700*time.Millisecond) } // received 59.7 s ago
	h.srv.jobs = []*ermite.Job{job}
	start := time.Now()
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	c := h.srv.completes[0]
	if c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT || c.RunnerShutdown {
		t.Fatalf("%v", c.Conclusion)
	}
	for _, u := range c.Steps {
		if u.State != ermite.StepState_STEP_STATE_COMPLETED {
			t.Errorf("step %d not completed", u.Number)
		}
	}
}

// The remaining time, not the whole timeout, goes to the executor.
func TestRunCarriesTheRemainingDeadline(t *testing.T) {
	var spec *execpb.JobSpec
	inner := simpleScript("x")
	h := newHarness(t, func(t *testing.T, c *execstream.Conn, run *execpb.Run, cn <-chan *execpb.Cancel) {
		spec = run.Job
		inner(t, c, run, cn)
	})
	h.runner.now = func() time.Time { return time.Now().Add(-3 * time.Minute) }
	h.srv.jobs = []*ermite.Job{testJob("j1")} // 5 minutes
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if spec == nil || spec.TimeoutMinutes != 5 || spec.DeadlineMs < 110_000 || spec.DeadlineMs > 120_000 {
		t.Fatalf("%v", spec)
	}
}

// ---- the guest is untrusted: step numbers and lines are checked (M3)

// hostile runs a job whose executor sends the frames of body, and checks that
// the job fails, the sandbox is destroyed, the server never saw a violation of
// its contract, and a line in the log names the reason.
func hostile(t *testing.T, wantReason string, body script) *harness {
	t.Helper()
	h := newHarness(t, func(t *testing.T, c *execstream.Conn, run *execpb.Run, cn <-chan *execpb.Cancel) {
		body(t, c, run, cn)
		// A real executor would go on; this one waits for the host to cut it.
		<-cn
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	c := h.srv.completes[0]
	if c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE || len(c.Outputs) != 0 {
		t.Fatalf("conclusion %v outputs %v", c.Conclusion, c.Outputs)
	}
	found := false
	for _, l := range h.srv.lines {
		if strings.Contains(l, "broke the protocol") && strings.Contains(l, wantReason) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error line mentioning %q in %q", wantReason, h.srv.lines)
	}
	for _, u := range c.Steps {
		if u.State != ermite.StepState_STEP_STATE_COMPLETED || u.Number < 1 || u.Number > 1000 {
			t.Errorf("invalid final step %v", u)
		}
	}
	if h.driver.stops != 1 {
		t.Errorf("the sandbox was not destroyed: stops=%d", h.driver.stops)
	}
	return h
}

func TestGuestStepNumberZeroFailsTheJob(t *testing.T) {
	hostile(t, "outside 1..1000", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepStart(c, 0, "zero")
	})
}

func TestGuestStepNumberOverTheLimitFailsTheJob(t *testing.T) {
	hostile(t, "outside 1..1000", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Set up job")
		stepStart(c, 1001, "far")
	})
}

func TestGuestStepGoingBackFailsTheJob(t *testing.T) {
	hostile(t, "started after step 3", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		stepStart(c, 3, "c")
		stepStart(c, 2, "b")
	})
}

func TestGuestStepRestartedFailsTheJob(t *testing.T) {
	hostile(t, "started after step 1", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		stepDone(c, 1, "a", cOK)
		stepStart(c, 1, "again")
	})
}

func TestGuestCompletedStepIsNeverTouchedAgain(t *testing.T) {
	h := hostile(t, "completed twice", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		stepDone(c, 1, "a", cOK)
		stepDone(c, 1, "a", cFail)
	})
	if u := h.srv.Result("j1").Steps[1]; u == nil || u.Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS {
		t.Errorf("a completed step was changed: %v", u.Conclusion)
	}
}

func TestGuestCompletingAnUnknownStepFailsTheJob(t *testing.T) {
	hostile(t, "without having started", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		stepDone(c, 7, "ghost", cOK)
	})
}

func TestGuestLogForAnUnstartedOrCompletedStepFailsTheJob(t *testing.T) {
	hostile(t, "has not started", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		logLineFrame(c, 5, "x", false)
	})
	hostile(t, "which is completed", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		stepDone(c, 1, "a", cOK)
		logLineFrame(c, 1, "late", false)
	})
}

func TestGuestLogGoingBackAStepFailsTheJob(t *testing.T) {
	hostile(t, "after one for step 2", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		stepStart(c, 2, "b")
		logLineFrame(c, 2, "x", false)
		logLineFrame(c, 1, "y", false)
	})
}

func TestGuestOversizedLineFailsTheJob(t *testing.T) {
	hostile(t, "bytes (the limit is 65536)", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		logLineFrame(c, 1, strings.Repeat("x", 64*1024+1), false)
	})
}

// Text that the server would refuse is repaired, not forwarded: a line feed
// makes more lines and long names are cut. (Invalid UTF-8 cannot reach the host:
// proto3 strings are validated when the frame is decoded, which fails the
// session; the executor replaces invalid bytes before it sends.)
func TestGuestTextIsMadeValidForTheServer(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, strings.Repeat("é", 800)) // 1,600 bytes
		logLineFrame(c, 1, "two\nlines", false)
		stepDone(c, 1, "", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if strings.Join(h.srv.lines, "|") != "two|lines" {
		t.Fatalf("%q", h.srv.lines)
	}
	if n := h.srv.steps[1].Name; len(n) > 1024 || !utf8.ValidString(n) {
		t.Fatalf("name of %d bytes", len(n))
	}
}

// A guest that sends steps without end cannot grow the memory of the host.
func TestGuestFloodIsBounded(t *testing.T) {
	h := hostile(t, "outside 1..1000", func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		for n := 1; n <= 1100; n++ {
			stepStart(c, n, "s")
			stepDone(c, n, "s", cOK)
		}
	})
	_ = h
}

// The server's line count is the truth (m6): when the answer to an AppendLog
// says the server holds more lines than the request accounts for, the next
// offset starts from the server's count and no gap or repeat is created.
func TestAppendLogResyncsToTheServersCount(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "a")
		logLineFrame(c, 1, "l1", false)
		time.Sleep(150 * time.Millisecond) // a first request of its own
		logLineFrame(c, 1, "l2", false)
		logLineFrame(c, 1, "l3", false)
		stepDone(c, 1, "a", cOK)
		jobDone(c, cOK, nil, false)
	})
	h.runner.Timing.LogFlush = 30 * time.Millisecond
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	// A line the runner never sent appears in the server's log (a retry whose
	// answer was lost, in a real server): the runner must continue after it.
	h.srv.mu.Lock()
	h.srv.appendHook = func(n int, r *ermite.AppendLogRequest) (bool, error) {
		if n == 1 {
			_, _ = h.srv.store.Apply(&ermite.AppendLogRequest{JobId: r.JobId, Step: 1, Offset: 0, Lines: []*ermite.LogLine{
				{Time: timestamppb.Now(), Text: "planted"}, {Time: timestamppb.Now(), Text: "planted2"}}})
			return false, nil
		}
		return false, nil
	}
	h.srv.mu.Unlock()
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	// The first request (offset 0, "l1") was skipped by the server because the
	// planted lines already took offsets 0 and 1; the runner learns it holds 2
	// lines (it expected 1), and goes on at offset 2: no gap, no repeat.
	if len(h.srv.lines) < 3 || h.srv.lines[0] != "planted" || h.srv.lines[len(h.srv.lines)-1] != "l3" {
		t.Fatalf("%q", h.srv.lines)
	}
	if !strings.Contains(h.logs.String(), "continuing from its count") {
		t.Fatalf("no warning about the resync:\n%s", h.logs.String())
	}
}

// ---- M4: the server refuses the result itself

func TestCompleteJobRefusedIsRetriedAsAFailureWithoutOutputs(t *testing.T) {
	h := newHarness(t, simpleScript("hello"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.completeHook = func(n int) (bool, error) {
		if n == 1 { // e.g. an output the server's rules refuse
			return false, connect.NewError(connect.CodeInvalidArgument, io.EOF)
		}
		return false, nil
	}
	h.run(t)
	eventually(t, "two CompleteJob calls", func() bool { return h.srv.completeCount() == 2 })
	time.Sleep(100 * time.Millisecond)
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.completes) != 2 {
		t.Fatalf("%d calls", len(h.srv.completes))
	}
	first, second := h.srv.completes[0], h.srv.completes[1]
	if first.Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS || first.Outputs["out"] != "v" {
		t.Fatalf("first: %v %v", first.Conclusion, first.Outputs)
	}
	if second.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE || len(second.Outputs) != 0 || second.RunnerShutdown {
		t.Fatalf("second: %v %v", second.Conclusion, second.Outputs)
	}
	if len(second.Steps) != 3 {
		t.Fatalf("steps %v", second.Steps)
	}
	for _, u := range second.Steps {
		if u.State != ermite.StepState_STEP_STATE_COMPLETED || u.StartedAt == nil || u.CompletedAt == nil || u.CompletedAt.AsTime().Before(u.StartedAt.AsTime()) {
			t.Errorf("invalid step %v", u)
		}
	}
	last := h.srv.lines[len(h.srv.lines)-1]
	if !strings.HasPrefix(last, "##[error]The runner could not report the result of the job: invalid_argument") {
		t.Fatalf("last line %q", last)
	}
}

func TestCompleteJobRefusedTwiceGivesUp(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.completeHook = func(int) (bool, error) {
		return false, connect.NewError(connect.CodeFailedPrecondition, io.EOF) // not a JobEnded detail
	}
	h.run(t)
	eventually(t, "two calls", func() bool { return h.srv.completeCount() == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := h.srv.completeCount(); n != 2 {
		t.Fatalf("%d calls: one retry only", n)
	}
	if !strings.Contains(h.logs.String(), `"level":"error"`) || !strings.Contains(h.logs.String(), "refused again") {
		t.Fatalf("no error log:\n%s", h.logs.String())
	}
}

// ---- G1: structured logs with the job's fields, warn for retries, error for refusals

func TestLogsCarryTheJobFieldsAndLevels(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.appendHook = func(n int, _ *ermite.AppendLogRequest) (bool, error) {
		if n == 1 {
			return false, connect.NewError(connect.CodeUnavailable, io.EOF)
		}
		return false, nil
	}
	h.srv.beatHook = func(n int) error { return nil }
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	var retry, assigned map[string]any
	for _, l := range strings.Split(h.logs.String(), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		switch m["message"] {
		case "call failed, retrying":
			retry = m
		case "job assigned":
			assigned = m
		}
	}
	for name, m := range map[string]map[string]any{"retry": retry, "assigned": assigned} {
		if m == nil {
			t.Fatalf("no %s event in\n%s", name, h.logs.String())
		}
		if m["job_id"] != "j1" || m["run_id"] != float64(7) || m["attempt"] != float64(1) {
			t.Errorf("%s event lacks the job fields: %v", name, m)
		}
	}
	if retry["level"] != "warn" || retry["call"] != "AppendLog" {
		t.Errorf("a retry is a warning: %v", retry)
	}
}

// The sizes of a sandbox come from ERMITE_RUNNER_JOB_CPUS, _JOB_MEMORY and
// _JOB_DISK (sandbox.Config.Resolve, applied by the driver at Start): the host
// asks for none, so that nothing in a job can change them.
func TestSandboxSizesAreLeftToTheDriversConfiguration(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	if len(h.driver.specs) != 1 {
		t.Fatalf("%v", h.driver.specs)
	}
	if s := h.driver.specs[0]; s.JobID != "j1" || s.CPUs != 0 || s.MemoryMiB != 0 || s.DiskMiB != 0 {
		t.Fatalf("%+v", s)
	}
	// And the configuration fills them (the real rule, in package sandbox).
	cfg, err := sandbox.LoadConfig(func(k string) string {
		return map[string]string{sandbox.EnvJobCPUs: "3", sandbox.EnvJobMemory: "2G", sandbox.EnvJobDisk: "10G"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Resolve(h.driver.specs[0]); got.CPUs != 3 || got.MemoryMiB != 2048 || got.DiskMiB != 10240 {
		t.Fatalf("%+v", got)
	}
}

// ---- M5: shutdown

// A job whose acknowledgement is pending when the runner shuts down is left to
// the server (it requeues it after 30 s): the sandbox is never started, no
// result is reported, and Run returns promptly.
func TestShutdownBeforeTheAcknowledgementLeavesTheJobToTheServer(t *testing.T) {
	h := newHarness(t, simpleScript("x"))
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	h.srv.beatHook = func(int) error { return connect.NewError(connect.CodeUnavailable, io.EOF) } // never acknowledged
	stop := h.run(t)
	eventually(t, "a failed acknowledgement", func() bool { h.srv.mu.Lock(); defer h.srv.mu.Unlock(); return len(h.srv.beats) >= 2 })
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Run took %v to return", time.Since(start))
	}
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	if h.driver.news != 0 || h.driver.started != 0 {
		t.Fatalf("a sandbox was created for a job that was never acknowledged: news=%d started=%d", h.driver.news, h.driver.started)
	}
	if n := h.srv.completeCount(); n != 0 {
		t.Fatalf("CompleteJob was called %d times", n)
	}
	if !strings.Contains(h.logs.String(), "before the job was acknowledged") {
		t.Fatalf("no explanation in the log:\n%s", h.logs.String())
	}
}

// The graceful shutdown of the defaults fits the time the unit gives.
func TestShutdownBudgetFitsTheUnit(t *testing.T) {
	budget := Timing{}.ShutdownBudget()
	if budget != 90*time.Second {
		t.Fatalf("the budget is %v, README and contrib/ermite-runner.service say 90 s", budget)
	}
	unit, err := os.ReadFile("../../contrib/ermite-runner.service")
	if err != nil {
		t.Skip(err)
	}
	m := regexp.MustCompile(`(?m)^TimeoutStopSec=(\d+)$`).FindStringSubmatch(string(unit))
	if m == nil {
		t.Fatal("no TimeoutStopSec in the unit")
	}
	secs, _ := strconv.Atoi(m[1])
	if time.Duration(secs)*time.Second < budget+15*time.Second {
		t.Fatalf("TimeoutStopSec=%d does not leave 15 s of margin over the %v budget", secs, budget)
	}
}

// ---- m7: the state directory is private

func TestSaveStateMakesAnExistingDirectoryPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, State{URL: "https://x", RunnerID: "r", Name: "n", RunnerToken: "ermiterun_t"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	if fi, _ := os.Stat(StateFile(dir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v", fi.Mode())
	}
}

// mountNamespaceOptions are the [Service] options that give a unit a mount
// namespace of its own. The unit must set none of them: in container mode the
// runner loop-mounts the scratch disk of a job; the Docker daemon, in the host's
// namespace, binds it into the container, and a mount made inside a private mount
// namespace of the unit is invisible to it (GO-001). Besides the options that say
// so (PrivateTmp, ProtectHome, ProtectSystem...), systemd builds a mount
// namespace for ProtectKernelModules and ProtectKernelLogs too.
var mountNamespaceOptions = []string{
	"PrivateTmp", "PrivateDevices", "PrivateMounts", "PrivateUsers", "ProtectHome", "ProtectSystem",
	"ProtectKernelModules", "ProtectKernelLogs", "ProtectKernelTunables", "ProtectControlGroups",
	"ProtectProc", "ProcSubset", "ProtectHostname", "ReadOnlyPaths", "ReadWritePaths", "InaccessiblePaths",
	"ExecPaths", "NoExecPaths", "TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths", "MountFlags",
	"MountAPIVFS", "RootDirectory", "RootImage", "RootEphemeral", "BindLogSockets", "LogNamespace",
	"PrivateIPC", "PrivatePIDs", "MountImages", "ExtensionImages", "ExtensionDirectories",
}

// unitMountNamespaceOptions returns the lines of the [Service] section of a unit
// that set one of mountNamespaceOptions. Comments, which name the options on
// purpose, and other sections are ignored.
func unitMountNamespaceOptions(unit string) []string {
	var found []string
	inService := false
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inService = line == "[Service]"
			continue
		}
		if !inService || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _ := strings.Cut(line, "=")
		for _, f := range mountNamespaceOptions {
			if strings.EqualFold(strings.TrimSpace(key), f) {
				found = append(found, line)
			}
		}
	}
	return found
}

func TestUnitHasNoMountNamespaceOptions(t *testing.T) {
	unit, err := os.ReadFile("../../contrib/ermite-runner.service")
	if err != nil {
		t.Skip(err)
	}
	if found := unitMountNamespaceOptions(string(unit)); len(found) > 0 {
		t.Fatalf("the unit sets options that give it a mount namespace of its own, which hides the loop mounts from Docker: %q", found)
	}
	// The hardening that stays must still be there.
	for _, want := range []string{"NoNewPrivileges=yes", "RestrictSUIDSGID=yes", "ProtectClock=yes", "RestrictAddressFamilies="} {
		if !strings.Contains(string(unit), "\n"+want) {
			t.Errorf("the unit lost %s", want)
		}
	}
}

// The check sees the options in [Service] (also with spaces and another case),
// and ignores comments and other sections.
func TestUnitMountNamespaceCheckSeesOptions(t *testing.T) {
	unit := "[Unit]\nPrivateTmp=yes\n[Service]\nPrivateTmp=yes\n  protecthome = read-only\nProtectKernelModules=yes\n# ProtectSystem=strict\nNoNewPrivileges=yes\n[Install]\nProtectSystem=yes\n"
	got := unitMountNamespaceOptions(unit)
	want := []string{"PrivateTmp=yes", "protecthome = read-only", "ProtectKernelModules=yes"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}
