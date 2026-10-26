// SPDX-License-Identifier: MIT
package host

import (
	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-runner/internal/deps"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"context"
	"errors"
	"strings"
	"time"
)

func (j *jobRun) executeDeps(ctx context.Context, rt *Runner, stopHard <-chan struct{}) outcome {
	fail := func(err error) outcome {
		j.depsReport = &ermite.DepsReport{Error: err.Error()}
		// AppendLog accepts single lines; the report keeps the full error.
		for _, line := range strings.Split(err.Error(), "\n") {
			j.sandboxError(line)
		}
		return failure()
	}
	r := &execpb.DepsRequest{Repo: j.job.Repo, Sha: j.job.Sha, GoVersion: j.job.Deps.GoVersion, Token: j.job.GithubToken, CloneUrl: strings.TrimRight(rt.Cfg.URL, "/") + "/" + j.job.Repo + ".git", AllowInsecureHttp: rt.Cfg.InsecureHTTP}
	j.contexts = j.job.Contexts
	r.RegistryPort, r.CacheToken, r.CacheMode, _ = j.runtimeFor(rt)
	for _, u := range j.job.Deps.Updates {
		r.Updates = append(r.Updates, &execpb.DepsUpdate{Ecosystem: u.GetEcosystem(), Directory: u.GetDirectory(), Name: u.GetName(), Version: u.GetVersion()})
	}
	allowed, err := deps.Allowed(r)
	if err != nil {
		return fail(err)
	}
	sb, err := rt.Driver.New()
	if err != nil {
		return fail(err)
	}
	defer j.stopSandbox(ctx, rt, sb)
	conn, err := j.startSandbox(ctx, rt, sb, stopHard)
	if err != nil {
		if ctx.Err() != nil || j.cancelRequested() {
			return j.stoppedOutcome()
		}
		return fail(err)
	}
	defer func() { _ = conn.Close() }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stopHard:
			_ = conn.Close()
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	c := &deps.Collector{Allowed: allowed}
	var invalid error
	reject := func(err error) {
		if err != nil && invalid == nil {
			invalid = err
			_ = conn.Close()
		}
	}
	_, exit, err := execstream.DriveJob(conn, &execpb.Run{Deps: r}, j.cancelCh, execstream.JobHandler{
		StepStarted: j.stepStarted, Log: j.logLine,
		DepsFileChunk: func(f *execpb.DepsFileChunk) { reject(c.Add(f)) },
		DepsResult:    func(r *execpb.DepsResult) { reject(c.Finish(r)) },
	})
	ok := invalid == nil && err == nil && c.Done() && exit != nil && exit.Code == 0
	if !ok && (ctx.Err() != nil || j.cancelRequested()) {
		return j.stoppedOutcome()
	}
	if invalid != nil {
		return fail(invalid)
	}
	if err != nil {
		return fail(errors.New("dependency sandbox session failed"))
	}
	if !c.Done() || exit == nil || exit.Code != 0 {
		return fail(errors.New("dependency sandbox ended without a successful result"))
	}
	j.depsReport = &ermite.DepsReport{}
	for p, b := range c.Files {
		j.depsReport.Files = append(j.depsReport.Files, &ermite.DepsFile{Path: p, Content: b})
	}
	j.stepCompleted(&execpb.StepCompleted{Number: 1, Conclusion: execpb.StepConclusion_STEP_CONCLUSION_SUCCESS, UnixNano: time.Now().UnixNano()})
	return outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_SUCCESS}
}
