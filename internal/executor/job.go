// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"github.com/bnema/zerowrap"
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
	// TimeoutMinutes is the job timeout; 0 means DefaultTimeoutMinutes. It is
	// used for the message and, without Deadline, for the timer.
	TimeoutMinutes int
	// Deadline is the time left of the job timeout when Run is called, when the
	// timeout started counting earlier (at the receipt of the job by the host);
	// 0 means TimeoutMinutes from now.
	Deadline time.Duration

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
	// CacheURL and CacheToken, when set, are ACTIONS_CACHE_URL and
	// ACTIONS_RUNTIME_TOKEN of the node and docker actions of the job: the cache
	// service of the runner. The token is masked in the log.
	CacheURL, CacheToken string
	// CacheMode is ACTIONS_CACHE_MODE ("read" or "write") next to the cache URL;
	// empty sets nothing.
	CacheMode string
	// ContainerCacheURL is CacheURL for the steps that run in a container (a
	// step in the job container, a docker action): the same relay on an address
	// of the sandbox their network reaches. Empty: those steps get no runtime
	// variables.
	ContainerCacheURL string
	// Results says that the service behind CacheURL also serves the results
	// service (artifacts) and that CacheToken carries its scope: node and docker
	// actions get ACTIONS_RESULTS_URL, and the node steps of the artifact actions
	// get a GITHUB_SERVER_URL @actions/artifact takes for GitHub (see
	// setArtifactsServerURL).
	Results bool
	// CacheOff says that the service behind CacheURL serves no cache (results
	// only): ACTIONS_CACHE_URL and ACTIONS_CACHE_MODE are not set.
	CacheOff bool
	// Secrets and Vars are Job.secrets and Job.vars. Secrets are registered in
	// the masker before the first step and form the secrets context; they are
	// never logged. Vars are the fallback of the vars context.
	Secrets, Vars map[string]string
	// SecretsWithheld says that the server did not send secrets because the run
	// is not trusted: "Set up job" says so.
	SecretsWithheld bool
	// TokenPermissions are the permissions of GITHUB_TOKEN as the server sent
	// them (Job.permissions: key to "read", "write" or "none"), which "Set up
	// job" prints as they are. Nil means the server sent none: they are then
	// worked out from the workflow. An empty, non-nil map is "none at all".
	TokenPermissions map[string]string
	// Debug turns ##[debug] lines on (ACTIONS_STEP_DEBUG).
	Debug bool
	// Actions configures the actions of the job (resolution, downloads, Node.js,
	// docker).
	Actions ActionsConfig
	// SendAnnotations and SendSummaries say that the server keeps annotations
	// and step summaries (features ci.annotations and ci.summaries): the
	// Reporter then gets them with StepResults.
	SendAnnotations, SendSummaries bool
	// Getenv reads the environment of the executor (the matcher timeout); nil
	// means os.Getenv.
	Getenv func(string) string
	// StepTimeoutSkew is added to every step timeout (tests use a negative
	// value to run a timeout of one minute in milliseconds).
	StepTimeoutSkew time.Duration

	Reporter Reporter
	// Log receives the runner's own diagnostics (not the job log); the zero value
	// discards them.
	Log zerowrap.Logger

	// Credential, when set, is the user the steps run as.
	Credential *syscall.Credential
	// Cancellation timings and test hooks, see procSpec.
	TermAfter, KillAfter, WaitDelay time.Duration
	Signal                          func(pid int, sig syscall.Signal)
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
	// masker hides the secrets in everything reported; Run sets it, with the
	// secrets of the job, before the first report.
	masker atomic.Pointer[masker]
	// actions holds what the pre-job phase loaded (see prepareActions).
	actions *actionState
	// postStack is the post steps registered so far, last registered last.
	postStack []qstep
	// upgradedToNode24 are the actions that asked for node20 and got node24.
	upgradedToNode24 []string
	jobCtx           context.Context
	// cs is the containers of the job (nil container and no services: none).
	cs *containerSet

	// Results of the commands (see issues.go, matchers.go, results.go).
	matchers   *matcherRoot
	issuesMu   sync.Mutex
	collectors map[int]*issueCollector // by step number
	outcomes   map[int]Conclusion
	summaries  map[int]string
	logLines   map[int]int            // lines written to the log of each step, under mu
	states     map[string]*intraState // intra-action state, see saveState
	// ghesSniff is, by script path, whether a node action's script bundles
	// @actions/artifact v2 (see bundlesArtifactClient); under mu.
	ghesSniff map[string]bool
}

func (j *job) mask(s string) string { return j.masker.Load().mask(s) }

// The runner's own diagnostics (Config.Log) bypass the reporter, hence the
// masker: a path, a docker message or an error can hold a secret. Every
// diagnostic of the executor goes through these helpers, which mask the
// rendered error, message and fields (TestDiagnosticsAreMasked checks that no
// other code calls Config.Log).

// diagWarn logs a warning with an optional error and string fields (key,
// value, ...).
func (j *job) diagWarn(err error, msg string, kv ...string) {
	j.diag(false, err, msg, kv)
}

// diagDebug is diagWarn at debug level.
func (j *job) diagDebug(err error, msg string, kv ...string) {
	j.diag(true, err, msg, kv)
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
	j := &job{cfg: cfg, cancel: cancel, trackID: "github_" + uuid.NewString(), matchers: &matcherRoot{}}
	// The masker exists before anything is reported, with the secrets of the job
	// registered (Worker.cs:158-171 does it before the job starts); the token and
	// the cache token join it once the contexts are built. GitHub's SecretMasker
	// has no minimum length (SecretMasker.AddValue ignores only null or empty
	// values), so a one-character secret is masked too.
	j.masker.Store(newMasker())
	registerSecrets(j.masker.Load(), cfg.Secrets)
	j.cfg.Reporter = &maskReporter{j: j, next: cfg.Reporter}
	defer func() {
		if j.fc != nil {
			_ = j.fc.Close()
		}
		if j.tempRoot != nil {
			_ = j.tempRoot.Close()
		}
	}()
	res := j.run()
	res.Error = j.mask(res.Error)
	j.cfg.Reporter.JobCompleted(res)
	return res
}

func (j *job) run() Result {
	// Whatever happens below (a panic, a shutdown that skips the post steps),
	// the containers, the network and the credentials directories are removed.
	defer j.fallbackCleanup()
	limit := j.cfg.Deadline
	if limit <= 0 {
		limit = time.Duration(j.timeoutMinutes()) * time.Minute
	}
	timer := time.AfterFunc(limit, func() { j.cancel.Cancel(CancelTimeout) })
	defer timer.Stop()

	// ---- Set up job (JobExtension.InitializeJob) ----
	rep := j.cfg.Reporter
	rep.StepStarted(1, SetUpJobName, j.now())
	var cancelCtx context.CancelFunc
	j.jobCtx, cancelCtx = context.WithCancel(context.Background())
	defer cancelCtx()
	go func() {
		select {
		case <-j.cancel.Done():
			cancelCtx()
		case <-j.jobCtx.Done():
		}
	}()
	steps, err := j.setUp()
	if err != nil {
		j.errorf(1, "%s", err.Error())
		rep.StepCompleted(1, SetUpJobName, ConclusionFailure, j.now())
		// The job ends here, without "Complete job" (JobRunner.cs:107).
		return Result{Conclusion: ConclusionFailure, Error: err.Error()}
	}
	rep.StepCompleted(1, SetUpJobName, ConclusionSuccess, j.now())

	// ---- the steps (StepsRunner) ----
	// The queue is the pre-job steps then the steps of the job; the post steps
	// registered while they run follow, last registered first
	// (StepsRunner.cs:62-75).
	num := 2
	for _, q := range steps {
		j.runStep(num, q)
		num++
	}
	for {
		j.mu.Lock()
		if len(j.postStack) == 0 {
			j.mu.Unlock()
			break
		}
		q := j.postStack[len(j.postStack)-1]
		j.postStack = j.postStack[:len(j.postStack)-1]
		j.mu.Unlock()
		j.runStep(num, q)
		num++
	}

	// ---- Complete job (JobExtension.FinalizeJob) ----
	outputs := j.completeJob(num)

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

// write sends one log line to the Reporter, in order. It returns the 1-based
// number of the line in the log of the step num, what the runner calls
// logFileLineNumber (_logger.TotalLines + 1 in ExecutionContext.Write). The
// steps inside a composite action write to the log of the step that uses it,
// so they count in the same sequence.
func (j *job) write(num int, line string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.logLines == nil {
		j.logLines = map[int]int{}
	}
	n := j.logLines[num] + 1
	// A line with a line feed in it is several lines of the log (Logging.cs:
	// 90-101); the host splits the text the same way.
	j.logLines[num] += 1 + strings.Count(line, "\n")
	j.cfg.Reporter.Log(num, j.now(), line)
	return n
}

// out writes a message that may hold several lines (ExecutionContext.Output).
func (j *job) out(num int, text string) {
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		j.write(num, l)
	}
}

// tagged writes tag+message; only the first line carries the tag. The tags of
// issues (##[error], ##[warning], ##[notice]) go through AddIssue: they are
// logged and become annotations, like the runner's IExecutionContext.Error
// and Warning.
func (j *job) tagged(num int, tag, msg string) {
	if level, ok := levelOfTag(tag); ok {
		j.addIssue(num, nil, false, issue{Level: level, Message: msg})
		return
	}
	j.logTagged(num, tag, j.mask(msg))
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
		Funcs:    map[string]expr.Func{"hashFiles": j.hashFilesFunc()},
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
func (j *job) setUp() ([]qstep, error) {
	cfg := j.cfg
	j.out(1, fmt.Sprintf("Current runner version: '%s'", cfg.RunnerVersion))
	// Only a self-hosted runner prints its name and machine (JobExtension.cs:
	// 88-98: the runner is identified by its OAuth client id); the recordings
	// of github-hosted runners have neither line. The runner group name
	// (system.runnerGroupName) is not known to ermite.
	if !cfg.Runner.githubHosted() {
		if cfg.Runner.Name != "" {
			j.out(1, fmt.Sprintf("Runner name: '%s'", cfg.Runner.Name))
		}
		if host, err := os.Hostname(); err == nil {
			j.out(1, fmt.Sprintf("Machine name: '%s'", host))
		}
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
	state.WithJobData(cfg.Secrets, cfg.Vars)
	j.state = state
	for _, v := range state.Secrets() {
		j.masker.Load().add(v)
	}
	j.masker.Load().add(cfg.CacheToken)

	// Features that arrive in later phases fail the job with a clear message.
	switch {
	case jb.IsReusable():
		return nil, errors.New("not supported yet: reusable workflows (jobs.<id>.uses)")
	}

	j.printTokenInfo()

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

	// JobExtension.cs:250-275: the job container and the services are
	// evaluated after the job environment, before the actions are prepared.
	j.debug(1, "Evaluating job container")
	j.debug(1, "Evaluating job service containers")
	jc, svcs, err := j.evalContainers()
	if err != nil {
		return nil, err
	}
	if jc != nil || len(svcs) > 0 {
		j.cs = &containerSet{job: jc, services: svcs}
	}

	j.out(1, "Prepare all required actions")
	queue, err := j.prepareJob()
	if err != nil {
		return nil, err
	}
	// JobExtension.cs:418-422: the display name of the job, which the server
	// computes there (message.JobDisplayName). ermite's Job message has no such
	// field, so the executor evaluates it (see jobDisplayName).
	name := cfg.JobName
	if name == "" {
		name = j.jobDisplayName()
	}
	if strings.TrimSpace(name) != "" {
		j.out(1, "Complete job name: "+name)
	}

	// Process tracking for the orphan cleanup of "Complete job".
	j.before = map[string]bool{}
	for _, p := range snapshotProcesses() {
		j.before[p.key()] = true
	}
	return queue, nil
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
			// An output that holds a secret is dropped (JobExtension.cs:819-824).
			if j.masker.Load().has(p.Value) {
				j.tagged(num, "##[warning]", fmt.Sprintf("Skip output '%s' since it may contain secret.", p.Key))
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

	// The actions that asked for node20 and got node24 (JobExtension.cs:1043-1049).
	j.mu.Lock()
	up := slices.SortedFunc(slices.Values(j.upgradedToNode24), func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	j.mu.Unlock()
	if len(up) > 0 {
		j.tagged(num, "##[warning]", "Node.js 20 is deprecated. The following actions target Node.js 20 but are being forced to run on Node.js 24: "+
			strings.Join(up, ", ")+". For more information see: "+node20DeprecationURL)
	}

	rep.StepCompleted(num, CompleteJobName, result.conclusion(), j.now())
	return outputs
}

// Defaults of the lines "Set up job" prints between the runner version and
// "Prepare workflow directory" (JobExtension.cs:150-182). The server gives
// GitHub's runner these as the variables system.github.token.permissions and
// actions_cache_mode and the github context key secret_source; ermite's Job
// message carries none of them, so the values recorded on github.com for a
// push to a public repository are used. Every one of the 221 recorded jobs
// has exactly these lines.
const (
	defaultSecretSource = "Actions"
	defaultCacheMode    = "write"
)

// SecretsWithheldLine is the line of "Set up job" for a run whose secrets the
// server withheld (an ermite text: GitHub has none for fork pull requests).
const SecretsWithheldLine = "Secrets are not available to this job: the run was started by a limited token."

// printTokenInfo prints the GITHUB_TOKEN permissions group, the secret source
// and the cache mode.
//
// GitHub's runner prints what the server sends as
// system.github.token.permissions (JobExtension.cs:154-166). ermite's server
// sends them in Job.permissions, already merged and lowered by its trust rule:
// they are printed as they are (serverPermissionLines). From a server that
// sends none the group is worked out from the workflow with the rules recorded
// on github.com (tokenPermissionLines). It is always printed: Metadata is listed
// even for "permissions: {}".
func (j *job) printTokenInfo() {
	j.out(1, "##[group]GITHUB_TOKEN Permissions")
	var lines []permissionLine
	if j.cfg.TokenPermissions != nil {
		lines = serverPermissionLines(j.cfg.TokenPermissions)
	} else {
		lines = tokenPermissionLines(j.wf, j.jb)
	}
	for _, p := range lines {
		j.out(1, p.name+": "+p.level)
	}
	j.out(1, "##[endgroup]")
	// secret_source is a github context key (JobExtension.cs:172); the context
	// of a job from ermite lacks it, so it is added with the recorded value.
	src := j.state.GitHub("secret_source")
	if src == "" {
		src = defaultSecretSource
		j.state.SetGitHub("secret_source", src)
	}
	j.out(1, "Secret source: "+src)
	if j.cfg.SecretsWithheld {
		j.out(1, SecretsWithheldLine)
	}
	j.out(1, "Cache mode: "+defaultCacheMode)
}

// jobDisplayName is the name of the job as GitHub shows it and the runner
// prints it after "Complete job name:": the "name" of the job (the job key when
// it has none) evaluated with github, needs, strategy, matrix, vars and inputs
// as the server has them (State.ServerContexts: github without the keys the
// runner adds), followed by the matrix values in parentheses unless the name
// reads the matrix (recorded: matrix-job-name, see workflow.Job.DisplayName).
// When the name cannot be worked out the job key is used and a warning is
// logged: the name is not worth failing the job for.
func (j *job) jobDisplayName() string {
	ctx := context.Background()
	fallback := func(why string, err error) string {
		j.diagWarn(err, "job display name: "+why+", using the job key", "job_key", j.cfg.JobKey)
		return j.cfg.JobKey
	}
	j.mu.Lock()
	all := j.state.ServerContexts()
	j.mu.Unlock()
	env := j.evalEnv(newOrdered())
	env.Contexts = all
	// The strategy itself sees neither the strategy nor the matrix context.
	senv := env
	senv.Contexts = map[string]expr.Value{}
	for _, k := range []string{"github", "needs", "vars", "inputs"} {
		senv.Contexts[k] = all[k]
	}
	st, err := j.jb.EvaluateStrategy(ctx, senv)
	if err != nil {
		return fallback("the strategy does not evaluate", err)
	}
	idx, ok := comboIndex(st, all["strategy"], all["matrix"])
	if !ok {
		return fallback("the matrix of the job is not one of the combinations of its strategy", nil)
	}
	name, err := j.jb.DisplayName(ctx, env, st, idx)
	if err != nil {
		return fallback("the name does not evaluate", err)
	}
	// GitHub shows "Display name of", not "Display name of " (recorded:
	// matrix-include-exclude, an empty last matrix value). The recordings only
	// show trailing spaces; both ends are trimmed, as ermite's server does.
	return strings.TrimSpace(name)
}

// comboIndex finds the combination of the strategy that is this job. Without a
// matrix it is the only one. The index of the strategy context is trusted only
// when that combination has the matrix of the job; otherwise the first
// combination with equal values is taken. ok is false when there is none.
func comboIndex(st *workflow.Strategy, strategyCtx, matrix expr.Value) (idx int, ok bool) {
	if !st.HasMatrix {
		return 0, true
	}
	m, isDict := matrix.(*expr.Dictionary)
	if !isDict {
		return 0, false
	}
	if d, isDict := strategyCtx.(*expr.Dictionary); isDict {
		if n, found := d.Get("job-index"); found {
			if f, isNum := n.(expr.Number); isNum && f >= 0 && int(f) < len(st.Combos) && sameMatrix(st.Combos[int(f)].Values, m) {
				return int(f), true
			}
		}
	}
	for i, c := range st.Combos {
		if sameMatrix(c.Values, m) {
			return i, true
		}
	}
	return 0, false
}

// sameMatrix compares two matrix values: the same keys (ignoring case and
// order) with equal values.
func sameMatrix(a, b *expr.Dictionary) bool {
	if a.Len() != b.Len() {
		return false
	}
	for _, p := range a.Pairs() {
		v, found := b.Get(p.Key)
		if !found || expr.ToJSON(v) != expr.ToJSON(p.Value) {
			return false
		}
	}
	return true
}

func (j *job) diag(debug bool, err error, msg string, kv []string) {
	ev := j.cfg.Log.Warn()
	if debug {
		ev = j.cfg.Log.Debug()
	}
	if err != nil {
		ev = ev.Str("error", j.mask(err.Error()))
	}
	for i := 0; i+1 < len(kv); i += 2 {
		ev = ev.Str(kv[i], j.mask(kv[i+1]))
	}
	ev.Msg(j.mask(msg))
}
