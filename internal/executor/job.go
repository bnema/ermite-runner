// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"github.com/google/uuid"
)

// Names of the implicit steps (JobExtension.cs:70 and :15).
const (
	SetUpJobName    = "Set up job"
	CompleteJobName = "Complete job"
)

// DefaultTimeoutMinutes is the timeout of a job without timeout-minutes.
const DefaultTimeoutMinutes = 360

// MaxOutputBytes is the limit of ermite on the job outputs, names and values
// together (1 MiB).
const MaxOutputBytes = 1 << 20

// maxIssueMessageLength is ExecutionContext._maxIssueMessageLength.
const maxIssueMessageLength = 4096

// Config describes the job to run.
type Config struct {
	// WorkflowPath and WorkflowYAML are the workflow file, which the executor
	// parses itself; JobKey selects the job.
	WorkflowPath string
	WorkflowYAML []byte
	JobKey       string
	// JobName is the display name of the job, for "Complete job name: ...".
	// Empty means the job key.
	JobName string
	// ContextsJSON and MatrixJSON are the fields of the job message.
	ContextsJSON string
	MatrixJSON   string
	// Repo is the repository name used when the github context lacks one.
	Repo string
	// TimeoutMinutes is the job timeout; 0 means DefaultTimeoutMinutes.
	TimeoutMinutes int

	Runner        RunnerInfo
	RunnerVersion string
	// Layout overrides the directory layout (tests); nil means the hosted layout
	// under Home.
	Layout *Layout
	// Home is the home directory of the user of the steps; empty means
	// /home/runner.
	Home string
	// ToolCache is RUNNER_TOOL_CACHE; empty means DefaultToolCache. The official
	// runner reads it from its own environment (HostContext.cs:411).
	ToolCache string
	// BaseEnv is the environment steps inherit; nil means os.Environ().
	BaseEnv []string
	// Debug turns ##[debug] lines on (ACTIONS_STEP_DEBUG).
	Debug bool

	Reporter Reporter

	// Credential, when set, is the user the steps run as.
	Credential *syscall.Credential
	// Cancellation timings and test hooks, see procSpec.
	TermAfter, KillAfter, WaitDelay time.Duration
	Signal                          func(pgid int, sig syscall.Signal)
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

type stepResult int

// Values are ordered like TaskResult so that merge picks the worst
// (TaskResultUtil.MergeTaskResults).
const (
	resSucceeded stepResult = iota
	resFailed
	resCanceled
	resSkipped
)

func (r stepResult) conclusion() Conclusion {
	switch r {
	case resFailed:
		return ConclusionFailure
	case resCanceled:
		return ConclusionCancelled
	case resSkipped:
		return ConclusionSkipped
	}
	return ConclusionSuccess
}

// merge is TaskResultUtil.MergeTaskResults (Runner.Common/Util/TaskResultUtil.cs:40).
func merge(cur *stepResult, coming stepResult) stepResult {
	if cur == nil {
		return coming
	}
	if *cur > resFailed {
		return *cur
	}
	if coming >= *cur {
		return coming
	}
	return *cur
}

type job struct {
	cfg    Config
	cancel *Canceller
	wf     *workflow.Workflow
	jb     *workflow.Job
	fc     *FileCommands
	// tempRoot is RUNNER_TEMP, opened before the first step (see FileCommands).
	tempRoot *os.Root
	trackID  string

	mu          sync.Mutex // serialises the Reporter and guards state and result
	state       *State
	result      *stepResult // nil until a step fails or the job is cancelled (Global result)
	cancelled   bool
	runDefaults struct{ shell, workingDirectory string }
	baseEnv     []string
	lookupPath  string
	before      map[string]bool
}

func (j *job) now() time.Time {
	if j.cfg.Now != nil {
		return j.cfg.Now()
	}
	return time.Now()
}

// Run executes one job and reports to cfg.Reporter. It always ends with a
// JobCompleted call and returns the same Result. cancel may be nil.
func Run(cancel *Canceller, cfg Config) Result {
	if cancel == nil {
		cancel = NewCanceller()
	}
	j := &job{cfg: cfg, cancel: cancel, trackID: "github_" + uuid.NewString()}
	defer func() {
		if j.fc != nil {
			_ = j.fc.Close()
		}
		if j.tempRoot != nil {
			_ = j.tempRoot.Close()
		}
	}()
	res := j.run()
	cfg.Reporter.JobCompleted(res)
	return res
}

func (j *job) run() Result {
	minutes := j.cfg.TimeoutMinutes
	if minutes <= 0 {
		minutes = DefaultTimeoutMinutes
	}
	timer := time.AfterFunc(time.Duration(minutes)*time.Minute, func() { j.cancel.Cancel(CancelTimeout) })
	defer timer.Stop()

	// ---- Set up job (JobExtension.InitializeJob) ----
	rep := j.cfg.Reporter
	rep.StepStarted(1, SetUpJobName, j.now())
	steps, err := j.setUp()
	if err != nil {
		j.errorf(1, "%s", err.Error())
		rep.StepCompleted(1, SetUpJobName, ConclusionFailure, j.now())
		// The job ends here, without "Complete job" (JobRunner.cs:107).
		return Result{Conclusion: ConclusionFailure, Error: err.Error()}
	}
	rep.StepCompleted(1, SetUpJobName, ConclusionSuccess, j.now())

	// ---- the steps (StepsRunner) ----
	for i, st := range steps {
		j.runStep(i+2, st)
	}

	// ---- Complete job (JobExtension.FinalizeJob) ----
	last := len(steps) + 2
	outputs := j.completeJob(last)

	return j.finish(outputs)
}

func (j *job) finish(outputs map[string]string) Result {
	res := Result{Outputs: outputs}
	j.mu.Lock()
	defer j.mu.Unlock()
	r := resSucceeded
	if j.result != nil {
		r = *j.result
	}
	switch {
	case j.cancel.Reason() == CancelTimeout && j.cancelled:
		res.Conclusion = ConclusionTimedOut
		res.Error = fmt.Sprintf("The job has exceeded the maximum execution time of %d minutes.", j.timeoutMinutes())
	case r == resCanceled:
		res.Conclusion = ConclusionCancelled
		res.RunnerShutdown = j.cancel.Reason() == CancelShutdown
	case r == resFailed:
		res.Conclusion = ConclusionFailure
	default:
		res.Conclusion = ConclusionSuccess
	}
	return res
}

func (j *job) timeoutMinutes() int {
	if j.cfg.TimeoutMinutes > 0 {
		return j.cfg.TimeoutMinutes
	}
	return DefaultTimeoutMinutes
}

// ---------------------------------------------------------------- logging

// write sends one log line to the Reporter, in order.
func (j *job) write(num int, line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cfg.Reporter.Log(num, j.now(), line)
}

// out writes a message that may hold several lines (ExecutionContext.Output).
func (j *job) out(num int, text string) {
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		j.write(num, l)
	}
}

// tagged writes tag+message; only the first line carries the tag.
func (j *job) tagged(num int, tag, msg string) {
	if len(msg) > maxIssueMessageLength && tag != "" {
		msg = msg[:maxIssueMessageLength]
	}
	lines := strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n")
	j.write(num, tag+lines[0])
	for _, l := range lines[1:] {
		j.write(num, l)
	}
}

func (j *job) errorf(num int, format string, a ...any) {
	j.tagged(num, "##[error]", fmt.Sprintf(format, a...))
}

func (j *job) debug(num int, msg string) {
	if j.cfg.Debug {
		for _, l := range strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n") {
			j.write(num, "##[debug]"+l)
		}
	}
}

// ---------------------------------------------------------------- set up

// evalEnv is the environment for expressions of the job and step tokens.
func (j *job) evalEnv(env *ordered) template.Env {
	return template.Env{
		Contexts: j.state.Values(env),
		Funcs: map[string]expr.Func{
			// hashFiles is a CI-2 feature: the schema allows it in steps, so it
			// parses, and fails when evaluated.
			"hashFiles": func(context.Context, []expr.Value) (expr.Value, error) {
				return nil, errors.New("not supported yet: hashFiles")
			},
		},
	}
}

func (j *job) evalString(tok template.Token, env *ordered) (string, error) {
	if tok == nil {
		return "", nil
	}
	return template.EvaluateString(context.Background(), tok, j.evalEnv(env))
}

// evalMap evaluates a mapping of strings (EvaluateStepEnvironment): values
// are converted like GitHub converts them (null is the empty string).
func (j *job) evalMap(m *template.Mapping, env *ordered) ([]Pair, error) {
	if m == nil {
		return nil, nil
	}
	v, err := template.Evaluate(context.Background(), m, j.evalEnv(env))
	if err != nil {
		return nil, err
	}
	d, ok := v.(*expr.Dictionary)
	if !ok {
		return nil, nil
	}
	var out []Pair
	for _, p := range d.Pairs() {
		if k := p.Value.Kind(); k == expr.KindArray || k == expr.KindDictionary {
			return nil, ghErrorf("Unexpected type '%s' encountered while reading 'environment value'. The type 'StringToken' was expected.", k)
		}
		out = append(out, Pair{p.Key, expr.CoerceString(p.Value)})
	}
	return out, nil
}

// setUp parses the workflow and prepares the job: JobExtension.InitializeJob
// (JobExtension.cs:63-182) and the job-start code of JobRunner.cs:142-177. Its
// log lines belong to step 1.
func (j *job) setUp() ([]*workflow.Step, error) {
	cfg := j.cfg
	j.out(1, fmt.Sprintf("Current runner version: '%s'", cfg.RunnerVersion))
	if cfg.Runner.Name != "" {
		j.out(1, fmt.Sprintf("Runner name: '%s'", cfg.Runner.Name))
	}
	if host, err := os.Hostname(); err == nil {
		j.out(1, fmt.Sprintf("Machine name: '%s'", host))
	}

	wf, errs := workflow.Parse(cfg.WorkflowPath, cfg.WorkflowYAML)
	if len(errs) > 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return nil, ghErrorf("The workflow is not valid. %s", strings.Join(msgs, "\n"))
	}
	jb := wf.Job(cfg.JobKey)
	if jb == nil {
		return nil, ghErrorf("The workflow has no job '%s'.", cfg.JobKey)
	}
	j.wf, j.jb = wf, jb

	state, layout, err := NewState(cfg.ContextsJSON, cfg.MatrixJSON, cfg.JobKey, cfg.Runner, cfg.Repo)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Layout != nil:
		state.WithLayout(*cfg.Layout)
		layout = *cfg.Layout
	case (cfg.Home != "" && cfg.Home != layout.Home) || cfg.ToolCache != "":
		home := cfg.Home
		if home == "" {
			home = layout.Home
		}
		layout = NewLayout(home, filepath.Base(layout.Workspace), cfg.ToolCache)
		state.WithLayout(layout)
	}
	j.state = state

	// Features that arrive in later phases fail the job with a clear message.
	switch {
	case jb.IsReusable():
		return nil, errors.New("not supported yet: reusable workflows (jobs.<id>.uses)")
	case jb.Container != nil:
		return nil, errors.New("not supported yet: jobs.<id>.container")
	case jb.Services != nil:
		return nil, errors.New("not supported yet: jobs.<id>.services")
	}

	// Directories (TempDirectoryManager, PipelineDirectoryManager).
	j.out(1, "Prepare workflow directory")
	for _, d := range []string{layout.Temp, layout.Workspace, layout.ToolCache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	clearDir(layout.Temp)
	if err := os.MkdirAll(layout.Temp, 0o755); err != nil {
		return nil, err
	}
	fc, err := NewFileCommands(filepath.Join(layout.Temp, "_runner_file_commands"))
	if err != nil {
		return nil, err
	}
	if c := cfg.Credential; c != nil {
		fc.SetOwner(int(c.Uid), int(c.Gid))
	}
	j.fc = fc
	// Everything the executor writes below the temp directory later goes through
	// this root (see FileCommands): the user of the steps owns the directory.
	if j.tempRoot, err = os.OpenRoot(layout.Temp); err != nil {
		return nil, err
	}
	for _, d := range []string{layout.WorkRoot, layout.RunnerWorkspace, layout.Workspace, layout.Temp, fc.dir} {
		j.own(d)
	}

	j.baseEnv = cfg.BaseEnv
	if j.baseEnv == nil {
		j.baseEnv = os.Environ()
	}
	if !hasEnv(j.baseEnv, "HOME") {
		j.baseEnv = append(append([]string(nil), j.baseEnv...), "HOME="+layout.Home)
	}
	j.lookupPath = envValue(j.baseEnv, "PATH")

	// Job-level environment: workflow env, then job env, each evaluated with an
	// empty env context (JobExtension.cs:38-49, ExecutionContext.cs:1048).
	for _, m := range []*template.Mapping{wf.Env, jb.Env} {
		pairs, err := j.evalMap(m, newOrdered())
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			j.state.SetEnv(p.Key, p.Value)
		}
	}

	// defaults.run: a job-level "run" replaces the workflow's whole (JobExtension.cs:78-96).
	for _, d := range []*workflow.Defaults{wf.Defaults, jb.Defaults} {
		if d == nil || d.Run == nil {
			continue
		}
		shell, err := j.evalString(d.Run.Shell, newOrdered())
		if err != nil {
			return nil, err
		}
		wd, err := j.evalString(d.Run.WorkingDirectory, newOrdered())
		if err != nil {
			return nil, err
		}
		j.runDefaults.shell, j.runDefaults.workingDirectory = shell, wd
	}

	j.out(1, "Prepare all required actions")
	name := cfg.JobName
	if name == "" {
		name = cfg.JobKey
	}
	if strings.TrimSpace(name) != "" {
		j.out(1, "Complete job name: "+name)
	}

	// Process tracking for the orphan cleanup of "Complete job".
	j.before = map[string]bool{}
	for _, p := range snapshotProcesses() {
		j.before[p.key()] = true
	}
	return jb.Steps, nil
}

// own gives a file or directory created by the executor to the user the steps
// run as, so that scripts can write to RUNNER_TEMP, the workspace and the file
// command files. Best effort: without Credential nothing is done.
func (j *job) own(path string) {
	if c := j.cfg.Credential; c != nil {
		_ = os.Lchown(path, int(c.Uid), int(c.Gid))
	}
}

func clearDir(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// ---------------------------------------------------------------- complete

// completeJob is JobExtension.FinalizeJob (JobExtension.cs:9-190): job outputs
// and the cleanup of orphan processes.
func (j *job) completeJob(num int) map[string]string {
	rep := j.cfg.Reporter
	rep.StepStarted(num, CompleteJobName, j.now())
	result := resSucceeded
	outputs := map[string]string{}

	if j.jb.Outputs != nil {
		// The runner of the official service prints nothing here: the recordings
		// of github.com show only "Cleaning up orphan processes" in "Complete
		// job", although the jobs have outputs (the log lines of
		// JobExtension.cs:48,68 belong to the legacy message that carries
		// JobOutputs).
		j.mu.Lock()
		env := j.state.JobEnv()
		j.mu.Unlock()
		pairs, err := j.evalMap(j.jb.Outputs, env)
		if err != nil {
			result = resFailed
			j.errorf(num, "Fail to evaluate job outputs")
			j.errorf(num, "%s", err.Error())
			j.mu.Lock()
			m := merge(j.result, resFailed)
			j.result = &m
			j.mu.Unlock()
		}
		total := 0
		for _, p := range pairs {
			if p.Value == "" {
				j.debug(num, fmt.Sprintf("Skip output '%s' since it's empty", p.Key))
				continue
			}
			// ermite refuses outputs above 1 MiB in total (runner.proto,
			// CompleteJobRequest.outputs), and they would not fit a frame of the
			// stream either: the job fails without outputs.
			if total += len(p.Key) + len(p.Value); total > MaxOutputBytes {
				result = resFailed
				j.errorf(num, "Fail to evaluate job outputs")
				j.errorf(num, "The job outputs are larger than 1 MiB in total.")
				j.mu.Lock()
				m := merge(j.result, resFailed)
				j.result = &m
				j.mu.Unlock()
				outputs = map[string]string{}
				break
			}
			outputs[p.Key] = p.Value
		}
	}

	j.out(num, "Cleaning up orphan processes")
	killOrphans(j.trackID, j.before, func(l string) { j.out(num, l) })

	rep.StepCompleted(num, CompleteJobName, result.conclusion(), j.now())
	return outputs
}
