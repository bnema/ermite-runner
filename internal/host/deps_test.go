// SPDX-License-Identifier: MIT
package host

import (
	"bnema.dev/ermite-runner/internal/cachesvc"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestDepsJobReportsFiles(t *testing.T) {
	h := newHarness(t, func(t *testing.T, c *execstream.Conn, r *execpb.Run, _ <-chan *execpb.Cancel) {
		if r.Deps == nil || r.Job != nil || r.Deps.GoVersion != "1.27" || r.Deps.Token != "repo-token" {
			t.Errorf("wrong deps request")
		}
		stepStart(c, 1, "Update dependencies")
		logLineFrame(c, 1, "go: updating dependencies", false)
		send(c, &execpb.Frame{Body: &execpb.Frame_DepsFileChunk{DepsFileChunk: &execpb.DepsFileChunk{Path: "go.mod", Data: []byte("module example.org/test\n"), Last: true}}})
		send(c, &execpb.Frame{Body: &execpb.Frame_DepsResult{DepsResult: &execpb.DepsResult{FileCount: 1}}})
		send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
	})
	j := testJob("deps-one")
	j.Deps = &ermite.DepsJob{GoVersion: "1.27", Updates: []*ermite.DepsJobUpdate{{Ecosystem: "gomod", Name: "example.org/m", Version: "v1.2.0"}}}
	j.GithubToken = "repo-token"
	h.srv.jobs = []*ermite.Job{j}
	stop := h.run(t)
	eventually(t, "deps completion", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	result := h.srv.completes[0]
	if result.Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS || result.DepsReport == nil || len(result.DepsReport.Files) != 1 || result.DepsReport.Files[0].Path != "go.mod" {
		t.Fatalf("result: %v", result)
	}
	if len(h.srv.beats) == 0 || len(h.srv.lines) == 0 {
		t.Fatal("missing heartbeat or logs")
	}
}

func TestDepsJobRejectsGuestPath(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "deps")
		send(c, &execpb.Frame{Body: &execpb.Frame_DepsFileChunk{DepsFileChunk: &execpb.DepsFileChunk{Path: "unexpected", Last: true}}})
	})
	j := testJob("deps-bad")
	j.Deps = &ermite.DepsJob{GoVersion: "1.27", Updates: []*ermite.DepsJobUpdate{{Ecosystem: "gomod", Name: "example.org/m", Version: "v1.2.0"}}}
	h.srv.jobs = []*ermite.Job{j}
	stop := h.run(t)
	eventually(t, "deps failure", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	r := h.srv.completes[0]
	if r.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE || r.GetDepsReport().GetError() == "" || len(r.GetDepsReport().Files) != 0 {
		t.Fatalf("result: %v", r)
	}
}

func depsJob(id string) *ermite.Job {
	j := testJob(id)
	j.Deps = &ermite.DepsJob{GoVersion: "1.27", Updates: []*ermite.DepsJobUpdate{{Ecosystem: "gomod", Name: "example.org/m", Version: "v1.2.0"}}}
	return j
}

func TestDepsJobShutdown(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Update dependencies")
		logLineFrame(c, 1, "working", false)
		<-cancels
		send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 1}}})
	})
	h.srv.jobs = []*ermite.Job{depsJob("deps-shutdown")}
	stop := h.run(t)
	eventually(t, "log", func() bool { return len(h.srv.snapshotLines()) == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.completes) != 1 {
		t.Fatalf("%d completes", len(h.srv.completes))
	}
	if c := h.srv.completes[0]; c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_CANCELLED || !c.RunnerShutdown {
		t.Fatalf("%v", c)
	}
}

func TestDepsJobUsesRunnerTokenAndNoWatch(t *testing.T) {
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Update dependencies")
		send(c, &execpb.Frame{Body: &execpb.Frame_DepsResult{DepsResult: &execpb.DepsResult{}}})
		send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
	})
	j := depsJob("deps-tok")
	j.JobToken = ""
	h.srv.jobs = []*ermite.Job{j}
	stop := h.run(t)
	eventually(t, "deps completion", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	// The fake accepts only the runner token for deps jobs, so reaching
	// CompleteJob and a heartbeat proves the Poll client was used.
	if h.srv.completes[0].Conclusion != ermite.CiConclusion_CI_CONCLUSION_SUCCESS || len(h.srv.beats) == 0 {
		t.Fatalf("result: %v, beats %d", h.srv.completes[0], len(h.srv.beats))
	}
	if h.srv.watches != 0 {
		t.Fatalf("WatchJob called %d times", h.srv.watches)
	}
}

// spyClient records which client carried the calls of a job.
type spyClient struct {
	ermiteconnect.RunnerServiceClient
	name    string
	mu      *sync.Mutex
	calls   *[]string
	watched chan struct{}
	once    *sync.Once
}

func (c spyClient) record(call string) {
	c.mu.Lock()
	*c.calls = append(*c.calls, c.name+":"+call)
	c.mu.Unlock()
}

func (c spyClient) CompleteJob(ctx context.Context, r *connect.Request[ermite.CompleteJobRequest]) (*connect.Response[ermite.CompleteJobResponse], error) {
	c.record("CompleteJob")
	return c.RunnerServiceClient.CompleteJob(ctx, r)
}

func (c spyClient) WatchJob(ctx context.Context, r *connect.Request[ermite.WatchJobRequest]) (*connect.ServerStreamForClient[ermite.WatchJobResponse], error) {
	c.record("WatchJob")
	c.once.Do(func() { close(c.watched) })
	return c.RunnerServiceClient.WatchJob(ctx, r)
}

// The client of a job follows its job token, not its kind: a deps job with a
// token uses the job client and WatchJob. (Without one it keeps the runner
// token and has no WatchJob: see TestDepsJobUsesRunnerTokenAndNoWatch.)
func TestDepsJobClientFollowsJobToken(t *testing.T) {
	var (
		mu      sync.Mutex
		calls   []string
		watched = make(chan struct{})
		once    sync.Once
	)
	h := newHarness(t, func(t *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "Update dependencies")
		select {
		case <-watched:
		case <-time.After(15 * time.Second):
			t.Error("WatchJob was not called")
		}
		send(c, &execpb.Frame{Body: &execpb.Frame_DepsResult{DepsResult: &execpb.DepsResult{}}})
		send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
	})
	spy := func(name string, inner ermiteconnect.RunnerServiceClient) ermiteconnect.RunnerServiceClient {
		return spyClient{RunnerServiceClient: inner, name: name, mu: &mu, calls: &calls, watched: watched, once: &once}
	}
	h.runner.Poll = spy("poll", h.runner.Poll)
	jobClient := h.runner.Job
	h.runner.Job = func(tok string) ermiteconnect.RunnerServiceClient { return spy("job", jobClient(tok)) }
	j := depsJob("deps-spy")
	j.JobToken = "tok"
	h.srv.jobs = []*ermite.Job{j}
	stop := h.run(t)
	eventually(t, "deps completion", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	hasWatch, hasComplete := false, false
	for _, c := range calls {
		hasWatch = hasWatch || c == "job:WatchJob"
		hasComplete = hasComplete || c == "job:CompleteJob"
	}
	if !hasComplete || !hasWatch {
		t.Fatalf("calls %v, want WatchJob and CompleteJob on the job client", calls)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if got := h.srv.completes[0].Conclusion; got != ermite.CiConclusion_CI_CONCLUSION_SUCCESS {
		t.Fatalf("conclusion %v", got)
	}
}

// A WatchJob cancel reaches the guest of a deps job that has a job token.
func TestDepsJobWatchCancel(t *testing.T) {
	got := make(chan *execpb.Cancel, 1)
	h := newHarness(t, func(t *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Update dependencies")
		logLineFrame(c, 1, "working", false)
		select {
		case cn := <-cancels:
			got <- cn
		case <-time.After(5 * time.Second):
			t.Error("no cancel reached the guest")
		}
		send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 1}}})
	})
	j := depsJob("deps-cancel")
	j.JobToken = "tok"
	h.srv.jobs = []*ermite.Job{j}
	h.run(t)
	eventually(t, "the job to run", func() bool { return len(h.srv.snapshotLines()) == 1 })
	select {
	case h.srv.watchMsgs <- &ermite.WatchJobResponse{Msg: &ermite.WatchJobResponse_Cancel{Cancel: &ermite.CancelRequest{Reason: ermite.CancelReason_CANCEL_REASON_USER, Message: "Canceled by the owner"}}}:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel was not taken by WatchJob")
	}
	select {
	case cn := <-got:
		if cn.Kind != execpb.CancelKind_CANCEL_KIND_USER {
			t.Fatalf("%v", cn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel did not reach the guest")
	}
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if c := h.srv.completes[0]; c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_CANCELLED || c.RunnerShutdown {
		t.Fatalf("%v", c)
	}
}

// A deps job honours timeout_minutes: the host cancels it at the deadline.
func TestDepsJobHonoursTimeout(t *testing.T) {
	h := newHarness(t, func(t *testing.T, c *execstream.Conn, _ *execpb.Run, cancels <-chan *execpb.Cancel) {
		stepStart(c, 1, "Update dependencies")
		if cn := <-cancels; cn.Kind != execpb.CancelKind_CANCEL_KIND_TIMEOUT {
			t.Errorf("kind %v", cn.Kind)
		}
		send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 1}}})
	})
	h.runner.now = func() time.Time { return time.Now().Add(-59*time.Second - 700*time.Millisecond) } // received 59.7 s ago
	j := depsJob("deps-timeout")
	j.TimeoutMinutes = 1
	h.srv.jobs = []*ermite.Job{j}
	h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if c := h.srv.completes[0]; c.Conclusion != ermite.CiConclusion_CI_CONCLUSION_TIMED_OUT {
		t.Fatalf("%v", c.Conclusion)
	}
}

// A multi-line cause survives the guest protocol and the API client unchanged.
func TestDepsJobReportsMultilineError(t *testing.T) {
	const cause = "dependency command failed (exit 1)\ngo: module requires go >= 1.26.0\nlast real line"
	h := newHarness(t, func(_ *testing.T, c *execstream.Conn, _ *execpb.Run, _ <-chan *execpb.Cancel) {
		stepStart(c, 1, "deps")
		send(c, &execpb.Frame{Body: &execpb.Frame_DepsResult{DepsResult: &execpb.DepsResult{Error: cause}}})
	})
	h.srv.jobs = []*ermite.Job{depsJob("deps-error-tail")}
	stop := h.run(t)
	eventually(t, "deps failure", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	result := h.srv.completes[0]
	if result.Conclusion != ermite.CiConclusion_CI_CONCLUSION_FAILURE || result.GetDepsReport().GetError() != cause {
		t.Fatalf("result: %v", result)
	}
}

func TestDepsCacheRequest(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			svc, err := cachesvc.New(cachesvc.Config{Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = svc.CloseAll() }()
			h := newHarness(t, func(t *testing.T, c *execstream.Conn, r *execpb.Run, _ <-chan *execpb.Cancel) {
				if explicit {
					if r.Deps.CacheToken == "" || r.Deps.CacheMode != "write" || r.Deps.RegistryPort != cachesvc.DefaultPort {
						t.Error("missing cache credentials")
					}
					if job, _, _, err := svc.RuntimeToken(r.Deps.CacheToken); err != nil || job != "deps-cache" {
						t.Errorf("invalid token: %s %v", job, err)
					}
				} else if r.Deps.CacheToken != "" || r.Deps.CacheMode != "" || r.Deps.RegistryPort != 0 {
					t.Error("old server enabled cache")
				}
				send(c, &execpb.Frame{Body: &execpb.Frame_DepsResult{DepsResult: &execpb.DepsResult{}}})
				send(c, &execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
			})
			j := depsJob("deps-cache")
			j.Contexts = scopeCtx
			j.Ref = "refs/heads/main"
			if explicit {
				j.Cache = &ermite.JobCache{Repository: "o/r", WriteRef: "refs/ermite/deps", ReadRefs: []string{"refs/ermite/deps"}}
			}
			h.runner.Cache = svc
			h.srv.jobs = []*ermite.Job{j}
			stop := h.run(t)
			eventually(t, "deps cache completion", func() bool { return h.srv.completeCount() == 1 })
			if err := stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
