// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/executor"
)

// frameReporter is the executor.Reporter that writes frames on the stream.
type frameReporter struct {
	conn *execstream.Conn
	// outcomes keeps steps.<id>.outcome from StepResults until StepCompleted.
	omu      sync.Mutex
	outcomes map[int]executor.Conclusion
	// err keeps the first send error; the job goes on without a host.
	failed chan struct{}
}

func ts(t time.Time) int64 { return t.UnixNano() }

func conclusionProto(c executor.Conclusion) execpb.StepConclusion {
	switch c {
	case executor.ConclusionSuccess:
		return execpb.StepConclusion_STEP_CONCLUSION_SUCCESS
	case executor.ConclusionFailure:
		return execpb.StepConclusion_STEP_CONCLUSION_FAILURE
	case executor.ConclusionCancelled:
		return execpb.StepConclusion_STEP_CONCLUSION_CANCELLED
	case executor.ConclusionSkipped:
		return execpb.StepConclusion_STEP_CONCLUSION_SKIPPED
	case executor.ConclusionTimedOut:
		return execpb.StepConclusion_STEP_CONCLUSION_TIMED_OUT
	}
	return execpb.StepConclusion_STEP_CONCLUSION_UNSPECIFIED
}

func (r *frameReporter) send(f *execpb.Frame) {
	if err := r.conn.Send(f); err != nil {
		select {
		case <-r.failed:
		default:
			close(r.failed)
		}
	}
}

func (r *frameReporter) StepStarted(n int, name string, at time.Time) {
	r.send(&execpb.Frame{Body: &execpb.Frame_StepStarted{StepStarted: &execpb.StepStarted{Number: int32(n), Name: name, UnixNano: ts(at)}}})
}

func (r *frameReporter) Log(n int, at time.Time, line string) {
	for i, piece := range execstream.SplitLine(line, execstream.MaxLogLine) {
		r.send(&execpb.Frame{Body: &execpb.Frame_Log{Log: &execpb.Log{Step: int32(n), UnixNano: ts(at), Text: piece, Continued: i > 0}}})
	}
}

func (r *frameReporter) StepCompleted(n int, name string, c executor.Conclusion, at time.Time) {
	r.omu.Lock()
	outcome := r.outcomes[n]
	delete(r.outcomes, n)
	r.omu.Unlock()
	r.send(&execpb.Frame{Body: &execpb.Frame_StepCompleted{StepCompleted: &execpb.StepCompleted{
		Number: int32(n), Name: name, Conclusion: conclusionProto(c), Outcome: conclusionProto(outcome), UnixNano: ts(at),
	}}})
}

func levelProto(l executor.AnnotationLevel) execpb.AnnotationLevel {
	switch l {
	case executor.LevelNotice:
		return execpb.AnnotationLevel_ANNOTATION_LEVEL_NOTICE
	case executor.LevelWarning:
		return execpb.AnnotationLevel_ANNOTATION_LEVEL_WARNING
	case executor.LevelFailure:
		return execpb.AnnotationLevel_ANNOTATION_LEVEL_FAILURE
	}
	return execpb.AnnotationLevel_ANNOTATION_LEVEL_UNSPECIFIED
}

// StepResults sends the annotations and the summary of a step, and keeps its
// outcome for the StepCompleted frame that follows.
func (r *frameReporter) StepResults(n int, res executor.StepResults) {
	r.omu.Lock()
	if r.outcomes == nil {
		r.outcomes = map[int]executor.Conclusion{}
	}
	r.outcomes[n] = res.Outcome
	r.omu.Unlock()
	if len(res.Annotations) > 0 {
		sa := &execpb.StepAnnotations{Step: int32(n)}
		for _, a := range res.Annotations {
			sa.Annotations = append(sa.Annotations, &execpb.Annotation{
				Level: levelProto(a.Level), Title: a.Title, Message: a.Message, File: a.File,
				StartLine: a.StartLine, EndLine: a.EndLine, StartColumn: a.StartColumn, EndColumn: a.EndColumn,
			})
		}
		r.send(&execpb.Frame{Body: &execpb.Frame_StepAnnotations{StepAnnotations: sa}})
	}
	if res.HasSummary {
		r.send(&execpb.Frame{Body: &execpb.Frame_StepSummary{StepSummary: &execpb.StepSummary{Step: int32(n), Markdown: res.Summary}}})
	}
}

func (r *frameReporter) JobCompleted(res executor.Result) {
	r.send(&execpb.Frame{Body: &execpb.Frame_JobResult{JobResult: &execpb.JobResult{
		Conclusion: conclusionProto(res.Conclusion), Outputs: res.Outputs, Error: res.Error, RunnerShutdown: res.RunnerShutdown,
	}}})
}

// StepUser is the account the steps run as: uid, gid and supplementary groups,
// like the hosted runners' "runner" user. Nil credential means the steps run as
// the executor itself.
type StepUser struct {
	Credential *syscall.Credential
	Name, Home string
}

// LookupStepUser finds the user the steps run as. When the executor is not
// root it cannot switch user: the steps run as the current user (a development
// or test setup, never the sandbox of a runner, whose init is root). When it is
// root and the user does not exist, the lookup fails: running a job's code as
// root of the sandbox, with the docker daemon and the whole image at hand,
// because an image lacks the account would hide a broken image. A test image
// opts in with allowRoot.
func LookupStepUser(name string, allowRoot bool) (StepUser, error) {
	return lookupStepUser(os.Geteuid(), user.Lookup, name, allowRoot)
}

func lookupStepUser(euid int, lookup func(string) (*user.User, error), name string, allowRoot bool) (StepUser, error) {
	if euid != 0 || name == "" {
		u := StepUser{Home: os.Getenv("HOME")}
		if cu, err := user.Current(); err == nil {
			u.Name = cu.Username
			if u.Home == "" {
				u.Home = cu.HomeDir
			}
		}
		return u, nil
	}
	u, err := lookup(name)
	if err != nil {
		if allowRoot {
			return StepUser{Name: "root", Home: "/root"}, nil
		}
		return StepUser{}, fmt.Errorf("the image has no user %q to run the steps as (%v): refusing to run them as root", name, err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				cred.Groups = append(cred.Groups, uint32(n))
			}
		}
	}
	return StepUser{Credential: cred, Name: u.Username, Home: u.HomeDir}, nil
}

// stepEnv is the environment the steps inherit: the executor's, without the
// variables of the stream, with HOME, USER and LOGNAME of the step user.
func stepEnv(environ []string, u StepUser) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "ERMITE_"), k == "RUNNER_TOOL_CACHE", k == "HOME", k == "USER", k == "LOGNAME":
			continue
		}
		out = append(out, kv)
	}
	if u.Home != "" {
		out = append(out, "HOME="+u.Home)
	}
	if u.Name != "" {
		out = append(out, "USER="+u.Name, "LOGNAME="+u.Name)
	}
	return out
}

func cancelKind(k execpb.CancelKind) executor.CancelReason {
	switch k {
	case execpb.CancelKind_CANCEL_KIND_TIMEOUT:
		return executor.CancelTimeout
	case execpb.CancelKind_CANCEL_KIND_SHUTDOWN:
		return executor.CancelShutdown
	}
	return executor.CancelUser
}

// runJob runs the job of a Run frame with the executor and reports on conn.
// It returns the exit code to report.
func runJob(ctx context.Context, conn *execstream.Conn, cfg ExecConfig, spec *execpb.JobSpec, vals jobValues) int {
	su, err := LookupStepUser(cfg.StepUser, cfg.AllowRootSteps)
	if err != nil {
		// Reported like any failure of "Set up job": the job fails, with the reason
		// in its log, and nothing of the workflow runs.
		return failSetup(conn, err.Error())
	}
	canceller := executor.NewCanceller()
	rep := &frameReporter{conn: conn, failed: make(chan struct{})}
	res := newResolver(conn)
	cacheEnv := startCacheRelay(ctx, cfg.Target, spec)
	defer cacheEnv.close()

	// Frames from the host while the job runs: a Cancel stops the job; a lost
	// host does too, like a runner shutdown. The reader ends with the stream: the
	// caller closes it after runJob, and the goroutine that reports a failed
	// send ends with the job (done).
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			f, err := conn.Recv()
			if err != nil {
				canceller.Cancel(executor.CancelShutdown)
				return
			}
			if c := f.GetCancel(); c != nil {
				canceller.Cancel(cancelKind(c.GetKind()))
			}
			if r := f.GetResolveResponse(); r != nil {
				res.deliver(r)
			}
		}
	}()
	// The executor's own context (SIGTERM forwarded by init: the sandbox is
	// being stopped) is a shutdown.
	stop := context.AfterFunc(ctx, func() { canceller.Cancel(executor.CancelShutdown) })
	defer stop()
	go func() {
		select {
		case <-rep.failed:
			canceller.Cancel(executor.CancelShutdown)
		case <-canceller.Done():
		case <-done:
		}
	}()

	env := spec.GetRunnerEnvironment()
	result := executor.Run(canceller, executor.Config{
		WorkflowPath: spec.GetWorkflowPath(), WorkflowYAML: []byte(spec.GetWorkflowYaml()), JobKey: spec.GetJobKey(),
		ContextsJSON: spec.GetContexts(), MatrixJSON: spec.GetMatrix(), Repo: spec.GetRepo(),
		TimeoutMinutes: int(spec.GetTimeoutMinutes()), Deadline: time.Duration(spec.GetDeadlineMs()) * time.Millisecond,
		Runner:        executor.RunnerInfo{Name: spec.GetRunnerName(), Environment: env},
		RunnerVersion: spec.GetRunnerVersion(),
		Home:          su.Home, ToolCache: os.Getenv("RUNNER_TOOL_CACHE"), BaseEnv: stepEnv(os.Environ(), su), Credential: su.Credential,
		CacheURL: cacheEnv.url, CacheToken: cacheEnv.token, CacheMode: cacheEnv.mode,
		Debug: spec.GetDebug(), Reporter: rep, Actions: executor.DefaultActionsConfig(res.resolve), Log: zerowrap.FromCtx(ctx),
		Secrets: vals.secrets, Vars: vals.vars, SecretsWithheld: spec.GetSecretsWithheld(), TokenPermissions: tokenPermissions(spec),
		SendAnnotations: spec.GetSendAnnotations(), SendSummaries: spec.GetSendSummaries(),
		TermAfter: cfg.TermAfter, KillAfter: cfg.KillAfter, WaitDelay: cfg.WaitDelay,
	})
	if result.Conclusion == executor.ConclusionSuccess {
		return 0
	}
	return 1
}

// cacheURL is ACTIONS_CACHE_URL: the cache service of the runner on the
// loopback of the sandbox, where the relay listens. The trailing slash matters:
// the toolkit appends "_apis/…" to it (cacheHttpClient.ts:47). Empty when the
// job has no cache service.
func cacheURL(port uint32) string {
	if port == 0 || port > 65535 {
		return ""
	}
	return "http://" + net.JoinHostPort(CacheListenHost, strconv.FormatUint(uint64(port), 10)) + "/"
}

// cacheRelay is the cache of a job in the sandbox: the relay, the URL of the
// loopback it listens on and the token of the job. All are empty without a cache
// service.
type cacheRelay struct {
	relay            *Relay
	url, token, mode string
}

func (c cacheRelay) close() {
	if c.relay != nil {
		_ = c.relay.Close()
	}
}

// startCacheRelay starts the relay of the cache for a job that has a cache
// service: it listens on the loopback port the host says and pipes every
// connection to a new stream on the cache channel of the host (a sibling of
// the executor's target). A relay that cannot start leaves the job without a
// cache, like a scope the host refuses: the job runs.
func startCacheRelay(ctx context.Context, target Target, spec *execpb.JobSpec) cacheRelay {
	url := cacheURL(spec.GetCachePort())
	if url == "" || spec.GetCacheToken() == "" {
		return cacheRelay{}
	}
	addr := net.JoinHostPort(CacheListenHost, strconv.FormatUint(uint64(spec.GetCachePort()), 10))
	r, err := StartRelay(ctx, addr, target.CacheTarget().DialStream, DefaultRelayMax)
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("job runs without the cache")
		return cacheRelay{}
	}
	return cacheRelay{relay: r, url: url, token: spec.GetCacheToken(), mode: spec.GetCacheMode()}
}

// failSetup ends a job that cannot start, like a failed "Set up job": step 1
// starts, logs the reason as an error, and fails; the job result is a failure.
func failSetup(conn *execstream.Conn, msg string) int {
	rep := &frameReporter{conn: conn, failed: make(chan struct{})}
	now := time.Now()
	rep.StepStarted(1, executor.SetUpJobName, now)
	rep.Log(1, now, "##[error]"+msg)
	rep.StepCompleted(1, executor.SetUpJobName, executor.ConclusionFailure, time.Now())
	rep.JobCompleted(executor.Result{Conclusion: executor.ConclusionFailure, Error: msg})
	return 1
}

// tokenPermissions is the permissions of GITHUB_TOKEN the server sent, nil when
// it sent none (the executor then works them out from the workflow).
func tokenPermissions(spec *execpb.JobSpec) map[string]string {
	p := spec.GetTokenPermissions()
	if p == nil {
		return nil
	}
	if p.GetLevels() == nil {
		return map[string]string{}
	}
	return p.GetLevels()
}
