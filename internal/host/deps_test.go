// SPDX-License-Identifier: MIT
package host

import (
	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"testing"
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
