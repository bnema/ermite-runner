// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/executor"
)

// MaxLogLine is the largest piece of a log line that fits an AppendLog line
// (64 KiB); longer lines are cut by SplitLine.
const MaxLogLine = 64 * 1024

// SplitLine cuts text into pieces of at most max bytes at UTF-8 character
// boundaries. The result has one piece for a short line, and always at least
// one (an empty line is one empty piece). Invalid UTF-8 in text is replaced
// with U+FFFD first, because the protocol only carries valid text.
func SplitLine(text string, max int) []string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\ufffd")
	}
	if len(text) <= max {
		return []string{text}
	}
	var out []string
	for len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 { // cannot happen with max >= utf8.UTFMax
			cut = max
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}

// frameReporter is the executor.Reporter that writes frames on the stream.
type frameReporter struct {
	conn *execstream.Conn
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
	for i, piece := range SplitLine(line, MaxLogLine) {
		r.send(&execpb.Frame{Body: &execpb.Frame_Log{Log: &execpb.Log{Step: int32(n), UnixNano: ts(at), Text: piece, Continued: i > 0}}})
	}
}

func (r *frameReporter) StepCompleted(n int, name string, c executor.Conclusion, at time.Time) {
	r.send(&execpb.Frame{Body: &execpb.Frame_StepCompleted{StepCompleted: &execpb.StepCompleted{Number: int32(n), Name: name, Conclusion: conclusionProto(c), UnixNano: ts(at)}}})
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
// root it cannot switch user: the steps run as the current user. When it is
// root and name does not exist, the steps run as root (an image without the
// user is a test image).
func LookupStepUser(name string) StepUser {
	if os.Geteuid() != 0 || name == "" {
		u := StepUser{Home: os.Getenv("HOME")}
		if cu, err := user.Current(); err == nil {
			u.Name = cu.Username
			if u.Home == "" {
				u.Home = cu.HomeDir
			}
		}
		return u
	}
	u, err := user.Lookup(name)
	if err != nil {
		return StepUser{Name: "root", Home: "/root"}
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
	return StepUser{Credential: cred, Name: u.Username, Home: u.HomeDir}
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
func runJob(ctx context.Context, conn *execstream.Conn, cfg ExecConfig, spec *execpb.JobSpec) int {
	su := LookupStepUser(cfg.StepUser)
	canceller := executor.NewCanceller()
	rep := &frameReporter{conn: conn, failed: make(chan struct{})}

	// Frames from the host while the job runs: a Cancel stops the job; a lost
	// host does too, like a runner shutdown.
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
		}
	}()

	env := spec.GetRunnerEnvironment()
	res := executor.Run(canceller, executor.Config{
		WorkflowPath: spec.GetWorkflowPath(), WorkflowYAML: []byte(spec.GetWorkflowYaml()), JobKey: spec.GetJobKey(),
		ContextsJSON: spec.GetContexts(), MatrixJSON: spec.GetMatrix(), Repo: spec.GetRepo(),
		TimeoutMinutes: int(spec.GetTimeoutMinutes()),
		Runner:         executor.RunnerInfo{Name: spec.GetRunnerName(), Environment: env},
		RunnerVersion:  spec.GetRunnerVersion(),
		Home:           su.Home, ToolCache: os.Getenv("RUNNER_TOOL_CACHE"), BaseEnv: stepEnv(os.Environ(), su), Credential: su.Credential,
		Debug: spec.GetDebug(), Reporter: rep,
		TermAfter: cfg.TermAfter, KillAfter: cfg.KillAfter, WaitDelay: cfg.WaitDelay, Signal: cfg.signal,
	})
	if res.Conclusion == executor.ConclusionSuccess {
		return 0
	}
	return 1
}
