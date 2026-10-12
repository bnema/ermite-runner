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

	"github.com/google/uuid"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
)

// Names of the implicit steps (JobExtension.cs:70 and :15).
const (
	SetUpJobName    = "Set up job"
	CompleteJobName = "Complete job"
)

// DefaultTimeoutMinutes is the timeout of a job without timeout-minutes.
const DefaultTimeoutMinutes = 360

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
	// Layout overrides the directory layout (tests); nil means the hosted layout.
	Layout *Layout
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
	cfg     Config
	cancel  *Canceller
	wf      *workflow.Workflow
	jb      *workflow.Job
	fc      *FileCommands
	trackID string

	mu          sync.Mutex // serialises the Reporter and guards state and result
	state       *State
	result      *stepResult // nil until a step fails or the job is cancelled (Global result)
	cancelled   bool
	runDefaults struct{ shell, workingDirectory string }
	baseEnv     []string
	lookupPath  string
	before      map[string]bool
	outputsSet  bool
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
			return nil, fmt.Errorf("Unexpected type '%s' encountered while reading 'environment value'. The type 'StringToken' was expected.", k)
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
		return nil, fmt.Errorf("The workflow is not valid. %s", strings.Join(msgs, "\n"))
	}
	jb := wf.Job(cfg.JobKey)
	if jb == nil {
		return nil, fmt.Errorf("The workflow has no job '%s'.", cfg.JobKey)
	}
	j.wf, j.jb = wf, jb

	state, layout, err := NewState(cfg.ContextsJSON, cfg.MatrixJSON, cfg.JobKey, cfg.Runner, cfg.Repo)
	if err != nil {
		return nil, err
	}
	if cfg.Layout != nil {
		state.WithLayout(*cfg.Layout)
		layout = *cfg.Layout
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
	j.fc = fc

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

// ---------------------------------------------------------------- steps

// stepRun is the state of one step being run; it implements Sink and
// commandHost.
type stepRun struct {
	j               *job
	num             int
	st              *workflow.Step
	env             *ordered // the step's environment: job env + step env
	result          *stepResult
	cmdResult       *stepResult
	outcome         *stepResult
	cmds            *commandManager
	secretEnvLogged bool
}

func (s *stepRun) Out(text string)        { s.j.out(s.num, text) }
func (s *stepRun) Tagged(tag, msg string) { s.j.tagged(s.num, tag, msg) }
func (s *stepRun) Debug(msg string)       { s.j.debug(s.num, msg) }
func (s *stepRun) Error(msg string)       { s.j.errorf(s.num, "%s", msg) }
func (s *stepRun) CommandFailed()         { f := resFailed; s.cmdResult = &f }
func (s *stepRun) SetOutput(name, value string) {
	s.j.mu.Lock()
	s.j.state.SetOutput(s.st.ID, name, value)
	s.j.mu.Unlock()
}
func (s *stepRun) SaveState(name, value string) {}
func (s *stepRun) SetEnv(name, value string) {
	s.j.mu.Lock()
	s.j.state.SetEnv(name, value)
	s.j.mu.Unlock()
}
func (s *stepRun) AddPath(p string) {
	s.j.mu.Lock()
	s.j.state.AddPath(p)
	s.j.mu.Unlock()
}

// EnvFlag reads a boolean variable of the env context, then of the process
// (ActionCommandManager.cs:236-246).
func (s *stepRun) EnvFlag(name string) bool {
	v := strings.ToLower(os.Getenv(name))
	if v != "true" {
		if e, ok := s.env.Get(name); ok {
			v = strings.ToLower(e)
		}
	}
	return v == "true"
}

func firstLine(contents string) string {
	l := strings.TrimLeft(contents, " \t\r\n")
	if i := strings.IndexAny(l, "\r\n"); i >= 0 {
		l = l[:i]
	}
	return l
}

// displayName is the name shown for the step (ActionRunner.GenerateDisplayName,
// ActionRunner.cs:363): the evaluated name, or "Run " and the first line of
// the script or the action reference.
func (j *job) displayName(st *workflow.Step, env *ordered) string {
	tok := st.Name
	prefix := ""
	switch {
	case tok != nil:
	case st.Run != nil:
		tok, prefix = st.Run, "Run "
	case st.Uses != nil:
		tok, prefix = st.Uses, "Run "
	default:
		return ""
	}
	name, err := j.evalString(tok, env)
	if err != nil {
		name = tokenSource(tok)
	}
	return formatStepName(prefix, name)
}

func tokenSource(t template.Token) string {
	if be, ok := t.(*template.BasicExpression); ok && be.Source != "" {
		return be.Source
	}
	return t.String()
}

// formatStepName is ActionRunner.FormatStepName (ActionRunner.cs:447).
func formatStepName(prefix, name string) string {
	if name == "" {
		return ""
	}
	return prefix + firstLine(name)
}

func (j *job) statusFuncs() workflow.StatusFuncs {
	st := j.state.Status()
	return workflow.StatusFuncs{Success: st == "success", Failure: st == "failure", Cancelled: st == "cancelled"}
}

// markCancelled records the cancellation of the job: job result Canceled and
// job.status cancelled (StepsRunner.cs:152-155 and :190-196).
func (j *job) markCancelledLocked() {
	if j.cancelled {
		return
	}
	j.cancelled = true
	c := resCanceled
	j.result = &c
	j.state.SetStatus("cancelled")
}

func (j *job) runStep(num int, st *workflow.Step) {
	rep := j.cfg.Reporter
	s := &stepRun{j: j, num: num, st: st}
	s.cmds = newCommandManager(s, j.cfg.Debug)

	// Environment of the step (StepsRunner.cs:108-139): job env, then the step's
	// own, evaluated against the job env.
	j.mu.Lock()
	jobEnv := j.state.JobEnv()
	j.mu.Unlock()
	s.env = jobEnv.Clone()
	pairs, envErr := j.evalMap(st.Env, jobEnv)
	if envErr == nil {
		for _, p := range pairs {
			s.env.Set(p.Key, p.Value)
		}
	}
	name := j.displayName(st, s.env)
	rep.StepStarted(num, name, j.now())

	complete := func(r stepResult) {
		res := r
		j.mu.Lock()
		// Update the steps context (ExecutionContext.UpdateGlobalStepsContext).
		outcome := res
		if s.outcome != nil {
			outcome = *s.outcome
		}
		j.state.SetStepResult(st.ID, string(outcome.conclusion()), string(res.conclusion()))
		if res == resFailed {
			m := merge(j.result, resFailed)
			j.result = &m
			j.state.SetStatus(statusName(m))
		}
		j.mu.Unlock()
		rep.StepCompleted(num, name, res.conclusion(), j.now())
	}

	if envErr != nil {
		j.errorf(num, "%s", envErr.Error())
		complete(resFailed)
		return
	}

	// Cancellation seen before the step starts (StepsRunner.cs:188-197).
	cancelledAtStart := false
	select {
	case <-j.cancel.Done():
		cancelledAtStart = true
		j.mu.Lock()
		j.markCancelledLocked()
		j.mu.Unlock()
	default:
	}

	// Condition (StepsRunner.cs:200-261). On runner shutdown conditions are not
	// evaluated and the step is skipped.
	cond := func() (bool, error) {
		j.mu.Lock()
		defer j.mu.Unlock()
		return st.EvaluateIf(context.Background(), j.evalEnv(s.env), j.statusFuncs())
	}
	if cancelledAtStart && j.cancel.Reason() == CancelShutdown {
		complete(resSkipped)
		return
	}
	ok, err := cond()
	switch {
	case err != nil:
		j.errorf(num, "%s", err.Error())
		complete(resFailed)
		return
	case !ok:
		complete(resSkipped)
		return
	}

	// Cancellation while the step runs (StepsRunner.cs:149-186): the job becomes
	// cancelled and the step is stopped unless its condition still holds
	// (always()).
	stepCancel := make(chan struct{})
	stopWatch := make(chan struct{})
	var watch sync.WaitGroup
	if !cancelledAtStart {
		watch.Add(1)
		go func() {
			defer watch.Done()
			select {
			case <-j.cancel.Done():
			case <-stopWatch:
				return
			}
			j.mu.Lock()
			j.markCancelledLocked()
			j.mu.Unlock()
			keep := false
			if j.cancel.Reason() != CancelShutdown {
				keep, _ = cond()
			}
			if !keep {
				close(stepCancel)
			}
		}()
	}

	r := j.execute(s, stepCancel)
	close(stopWatch)
	watch.Wait()

	// Merge the result of the commands (StepsRunner.cs:347-351).
	if s.cmdResult != nil {
		m := merge(&r, *s.cmdResult)
		r = m
	}
	// continue-on-error (ExecutionContext.ApplyContinueOnError, :1422).
	if r == resFailed && st.ContinueOnError != nil {
		coe, cerr := template.EvaluateBool(context.Background(), st.ContinueOnError, j.evalEnv(s.env))
		if cerr != nil {
			j.errorf(num, "The step failed and an error occurred when attempting to determine whether to continue on error.")
			j.errorf(num, "%s", cerr.Error())
		}
		if coe && cerr == nil {
			o := resFailed
			s.outcome = &o
			r = resSucceeded
		}
	}
	complete(r)
}

// statusName is the job.status for a result (TaskResult.ToActionResult).
func statusName(r stepResult) string {
	switch r {
	case resFailed:
		return "failure"
	case resCanceled:
		return "cancelled"
	}
	return "success"
}

// notSupported is the failure of a step that uses a feature of a later phase.
func notSupported(st *workflow.Step) string {
	switch {
	case st.Uses != nil:
		return fmt.Sprintf("not supported yet: uses (%s)", st.Uses.Value)
	case st.TimeoutMinutes != nil:
		return "not supported yet: timeout-minutes"
	}
	return ""
}

// execute is ActionRunner.RunAsync plus ScriptHandler for one run step.
func (j *job) execute(s *stepRun, stepCancel <-chan struct{}) (result stepResult) {
	num, st := s.num, s.st
	if msg := notSupported(st); msg != "" {
		if st.Uses != nil {
			j.out(num, "##[group]Run "+st.Uses.Value)
			j.out(num, "##[endgroup]")
		}
		j.errorf(num, "%s", msg)
		return resFailed
	}
	fail := func(err error) stepResult { j.errorf(num, "%s", err.Error()); return resFailed }

	// File commands for this step: the github context carries the paths
	// (FileCommandManager.InitializeFiles), event.json is written
	// (ExecutionContext.WriteWebhookPayload).
	j.mu.Lock()
	files, ferr := j.fc.Init()
	if ferr == nil {
		for _, c := range fileCommands {
			j.state.SetGitHub(c.ContextName, files[envVarName(c.ContextName)])
		}
		j.state.SetGitHub("action", st.ID)
		j.state.SetGitHub("action_repository", "")
		j.state.SetGitHub("action_ref", "")
		if ev := j.state.EventJSON(); ev != "" {
			dir := filepath.Join(j.state.Layout().Temp, "_github_workflow")
			if err := os.MkdirAll(dir, 0o755); err == nil {
				p := filepath.Join(dir, "event.json")
				if err := os.WriteFile(p, []byte(ev), 0o644); err == nil {
					j.state.SetGitHub("event_path", p)
				}
			}
		}
	}
	j.mu.Unlock()
	if ferr != nil {
		return fail(ferr)
	}
	defer func() {
		// ProcessFiles runs after the handler, even when it failed (finally).
		j.fc.Process(s)
	}()

	// Inputs (ActionRunner.cs:174-185).
	script, err := j.evalString(st.Run, s.env)
	if err != nil {
		return fail(err)
	}
	shellInput, err := j.evalString(st.Shell, s.env)
	if err != nil {
		return fail(err)
	}
	var wdInput string
	var hasWD bool
	if st.WorkingDirectory != nil {
		hasWD = true
		if wdInput, err = j.evalString(st.WorkingDirectory, s.env); err != nil {
			return fail(err)
		}
	}

	// Header (ScriptHandler.PrintActionDetails, ScriptHandler.cs:27-137).
	j.out(num, "##[group]Run "+firstLine(script))
	for _, l := range strings.Split(strings.TrimRight(strings.ReplaceAll(script, "\r\n", "\n"), "\n"), "\n") {
		j.write(num, "\x1b[36;1m"+l+"\x1b[0m") // bright cyan
	}
	shell := shellInput
	if shell == "" {
		shell = j.runDefaults.shell
	}
	j.mu.Lock()
	lookup := Lookup{Path: j.lookupPath, Prepend: j.state.Prepend()}
	j.mu.Unlock()
	plan, err := PlanShell(shell, lookup)
	if err != nil {
		return fail(err) // the group stays open, like the original
	}
	j.out(num, "shell: "+plan.ShellLine())
	if s.env.Len() > 0 {
		j.write(num, "env:")
		for _, k := range s.env.Keys() {
			v, _ := s.env.Get(k)
			j.out(num, fmt.Sprintf("  %s: %s", k, v))
		}
	}
	j.write(num, "##[endgroup]")

	// Run (ScriptHandler.RunAsync, :140-358).
	wd := wdInput
	if !hasWD {
		wd = j.runDefaults.workingDirectory
	}
	j.mu.Lock()
	workspace := j.state.GitHub("workspace")
	temp := j.state.Layout().Temp
	j.mu.Unlock()
	dir := ResolveWorkingDirectory(workspace, wd)
	sc, err := WriteScript(temp, plan, script)
	if err != nil {
		return fail(err)
	}

	// Process environment: the inherited one, the step's environment, PATH with
	// GITHUB_PATH in front, then the runtime variables of the contexts.
	j.mu.Lock()
	runtime := j.state.RuntimeEnv()
	prepend := j.state.Prepend()
	j.mu.Unlock()
	penv := newOrdered()
	for _, kv := range j.baseEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			penv.Set(k, v)
		}
	}
	for _, k := range s.env.Keys() {
		v, _ := s.env.Get(k)
		penv.Set(k, v)
	}
	if len(prepend) > 0 {
		orig, ok := s.env.Get("PATH")
		if !ok {
			orig = j.lookupPath
		}
		penv.Set("PATH", PrependedPath(prepend, orig))
	}
	for _, k := range runtime.Keys() {
		v, _ := runtime.Get(k)
		penv.Set(k, v)
	}
	penv.Set("GITHUB_ACTIONS", "true") // ProcessInvoker.cs:283
	if _, ok := penv.Get("CI"); !ok {
		penv.Set("CI", "true")
	}
	penv.Set(ProcessTrackingID, j.trackID)
	var envList []string
	for _, k := range penv.Keys() {
		v, _ := penv.Get(k)
		envList = append(envList, k+"="+v)
	}

	res, err := runProcess(procSpec{
		Argv: sc.Argv, Dir: dir, Env: envList, Credential: j.cfg.Credential,
		OnLine: func(line string) {
			if strings.Contains(line, "##[start-action") || strings.Contains(line, "##[end-action") {
				line = strings.ReplaceAll(strings.ReplaceAll(line, "##[start-action", `##[\start-action`), "##[end-action", `##[\end-action`)
			}
			if (strings.Contains(line, "##[") || strings.Contains(line, "::")) && s.cmds.TryProcess(line) {
				return
			}
			j.write(num, line)
		},
		Cancel: stepCancel, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter,
		WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
	})
	if err != nil {
		return fail(errors.New(startError(plan.CommandPath, dir, err)))
	}
	if res.Cancelled {
		// The process was stopped on cancellation: ExecuteAsync throws
		// (ProcessInvoker.cs:369) and StepsRunner logs the exception
		// (StepsRunner.cs:333-337).
		j.errorf(num, "The operation was canceled.")
		return resCanceled
	}
	if res.ExitCode != 0 {
		j.errorf(num, "Process completed with exit code %d.", res.ExitCode)
		return resFailed
	}
	return resSucceeded
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
		for _, p := range pairs {
			if p.Value == "" {
				j.debug(num, fmt.Sprintf("Skip output '%s' since it's empty", p.Key))
				continue
			}
			outputs[p.Key] = p.Value
		}
	}

	j.out(num, "Cleaning up orphan processes")
	killOrphans(j.trackID, j.before, func(l string) { j.out(num, l) })

	rep.StepCompleted(num, CompleteJobName, result.conclusion(), j.now())
	return outputs
}
