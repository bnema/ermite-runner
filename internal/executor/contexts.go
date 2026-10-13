// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"bnema.dev/ermite-actions/expr"
)

// ordered is a string map that keeps the position of a key when it is set
// again, like the Dictionary<string,string> the official runner uses for
// Global.EnvironmentVariables and the step environment (the order shows in the
// "env:" block of the log). Keys are case-sensitive, as on Linux
// (VarUtil.EnvironmentVariableKeyComparer).
type ordered struct {
	keys []string
	vals map[string]string
}

func newOrdered() *ordered { return &ordered{vals: map[string]string{}} }

func (o *ordered) Set(k, v string) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *ordered) Get(k string) (string, bool) { v, ok := o.vals[k]; return v, ok }
func (o *ordered) Len() int                    { return len(o.keys) }
func (o *ordered) Keys() []string              { return slices.Clone(o.keys) }

func (o *ordered) Clone() *ordered {
	c := newOrdered()
	for _, k := range o.keys {
		c.Set(k, o.vals[k])
	}
	return c
}

// Dict is the value of the env context. The context of the official runner is
// case-sensitive on Linux (CaseSensitiveDictionaryContextData); package expr
// has only case-insensitive dictionaries, so two names that differ by case
// collapse to the first in expressions (they stay distinct in the process
// environment). A known deviation.
func (o *ordered) Dict() *expr.Dictionary {
	d := expr.NewDictionary()
	for _, k := range o.keys {
		d.Add(k, expr.String(o.vals[k]))
	}
	return d
}

// setKey returns d with key set to v: the value is replaced in place when the
// key (compared case-insensitively) exists, else appended.
func setKey(d *expr.Dictionary, key string, v expr.Value) *expr.Dictionary {
	out := expr.NewDictionary()
	done := false
	for _, p := range d.Pairs() {
		if strings.EqualFold(p.Key, key) {
			if !done {
				out.Add(p.Key, v)
				done = true
			}
			continue
		}
		out.Add(p.Key, p.Value)
	}
	if !done {
		out.Add(key, v)
	}
	return out
}

// setKeySorted is setKey for the github context: a key that is not there yet
// goes before the first key that sorts after it (bytewise; "token" stays first)
// instead of at the end, so that the keys the runner adds (workspace,
// secret_source, the file commands...) sit where the ermite-conformance
// recordings list them. Note that the recordings are normalised with sorted
// keys, so they show the sorted order, not necessarily the insertion order of
// GitHub's runner (SetGitHubContext appends). Keys that exist are replaced in
// place.
func setKeySorted(d *expr.Dictionary, key string, v expr.Value) *expr.Dictionary {
	for _, p := range d.Pairs() {
		if strings.EqualFold(p.Key, key) {
			return setKey(d, key, v)
		}
	}
	out := expr.NewDictionary()
	done := false
	for _, p := range d.Pairs() {
		if !done && p.Key > key && !strings.EqualFold(p.Key, "token") {
			out.Add(key, v)
			done = true
		}
		out.Add(p.Key, p.Value)
	}
	if !done {
		out.Add(key, v)
	}
	return out
}

func str(d *expr.Dictionary, key string) string {
	if v, ok := d.Get(key); ok {
		if s, ok := v.(expr.String); ok {
			return string(s)
		}
	}
	return ""
}

// Layout is the directory layout of a job, like a hosted runner's
// (/home/runner/work/<repo>/<repo>, RUNNER_TEMP, RUNNER_TOOL_CACHE).
type Layout struct {
	Home string // HOME of the runner user
	// WorkRoot is the _work directory: <Home>/work.
	WorkRoot string
	// Temp is RUNNER_TEMP: <WorkRoot>/_temp.
	Temp string
	// ToolCache is RUNNER_TOOL_CACHE.
	ToolCache string
	// RunnerWorkspace is RUNNER_WORKSPACE: <WorkRoot>/<repo>.
	RunnerWorkspace string
	// Workspace is GITHUB_WORKSPACE: <WorkRoot>/<repo>/<repo>.
	Workspace string
}

// DefaultToolCache is RUNNER_TOOL_CACHE on the ermite image.
const DefaultToolCache = "/opt/hostedtoolcache"

// NewLayout lays out a job of the repository repo (its name, without owner)
// under home. An empty toolCache means DefaultToolCache.
func NewLayout(home, repo, toolCache string) Layout {
	if toolCache == "" {
		toolCache = DefaultToolCache
	}
	work := filepath.Join(home, "work")
	return Layout{
		Home: home, WorkRoot: work, Temp: filepath.Join(work, "_temp"), ToolCache: toolCache,
		RunnerWorkspace: filepath.Join(work, repo), Workspace: filepath.Join(work, repo, repo),
	}
}

// RunnerInfo describes the runner for the runner context.
type RunnerInfo struct {
	Name string
	// Environment is "self-hosted" (the default) or "github-hosted".
	Environment string
	// Arch is "X64" or "ARM64"; empty means the architecture of this process.
	Arch string
}

// githubHosted reports whether the runner.environment is "github-hosted". An
// empty Environment is "self-hosted", the default of the official runner's
// runner context.
func (r RunnerInfo) githubHosted() bool { return r.Environment == "github-hosted" }

func (r RunnerInfo) arch() string {
	if r.Arch != "" {
		return r.Arch
	}
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	case "arm":
		return "ARM"
	}
	return "X86"
}

// repoName returns the repository name of "owner/name".
func repoName(full string) string {
	if i := strings.LastIndexByte(full, '/'); i >= 0 {
		return full[i+1:]
	}
	return full
}

// stepCtx is one entry of the steps context.
type stepCtx struct {
	outputs *ordered
	outcome string // "" until the step completes
	concl   string
}

// State holds the contexts of a job as the official runner does
// (ExecutionContext.ExpressionValues): the contexts the server computed, and
// github, runner, job, env and steps, which the runner owns.
type State struct {
	layout Layout
	github *expr.Dictionary
	// serverGitHub is the github context as the server sent it, before the
	// runner added its keys (token, workspace, the file commands, secret_source,
	// action...): the contexts that the server itself evaluates the job name
	// with (see ServerContexts).
	serverGitHub *expr.Dictionary
	runner       *expr.Dictionary
	job          *expr.Dictionary
	// other holds needs, strategy, matrix, inputs, vars: copied from the server.
	other map[string]expr.Value
	// env is Global.EnvironmentVariables: the job-level environment and what
	// GITHUB_ENV adds. A step's own env is not in it.
	env *ordered
	// steps is the steps context, in order of first mention.
	stepOrder []string
	steps     map[string]*stepCtx
	// prepend is Global.PrependPath in the order of the GITHUB_PATH entries.
	prepend []string
	// status is job.status: "success", "failure" or "cancelled".
	status string
}

// serverContexts are the keys of the contexts JSON that the runner reads.
var serverContexts = []string{"needs", "strategy", "matrix", "inputs", "vars"}

// NewState builds the contexts of a job from the JSON the server sent (the
// "contexts" field of the job) and the matrix JSON, with the context
// initialisation of JobRunner.cs:142-177 and ExecutionContext.InitializeJob
// (ExecutionContext.cs:1001-1056).
func NewState(contextsJSON, matrixJSON, jobKey string, r RunnerInfo, repoFallback string) (*State, Layout, error) {
	root := expr.NewDictionary()
	if strings.TrimSpace(contextsJSON) != "" {
		v, err := expr.FromJSON([]byte(contextsJSON))
		if err != nil {
			return nil, Layout{}, fmt.Errorf("job contexts: %w", err)
		}
		d, ok := v.(*expr.Dictionary)
		if !ok {
			return nil, Layout{}, fmt.Errorf("job contexts: want a JSON object")
		}
		root = d
	}
	s := &State{other: map[string]expr.Value{}, env: newOrdered(), steps: map[string]*stepCtx{}, status: "success"}

	// github: token first, then what the server gave (ExecutionContext.cs:1023-1037).
	// The token is the one the server sent in github.token (GitHub's
	// system.github.token variable), empty when it sent none.
	gh := expr.NewDictionary()
	gh.Add("token", expr.String(""))
	if v, ok := root.Get("github"); ok {
		d, ok := v.(*expr.Dictionary)
		if !ok {
			return nil, Layout{}, fmt.Errorf("job contexts: github must be an object")
		}
		for _, p := range d.Pairs() {
			if strings.EqualFold(p.Key, "token") {
				if t, isStr := p.Value.(expr.String); isStr {
					gh = setKey(gh, "token", t)
				}
				continue
			}
			gh.Add(p.Key, p.Value)
		}
	}
	s.serverGitHub = expr.NewDictionary()
	if v, ok := root.Get("github"); ok {
		s.serverGitHub = v.(*expr.Dictionary)
		// GitHub evaluates names with github.job empty (recorded:
		// ctx-github-names): the job key is only added to the contexts the
		// job runs with.
		if _, has := s.serverGitHub.Get("job"); has {
			s.serverGitHub = setKey(s.serverGitHub, "job", expr.String(""))
		}
	}
	if str(gh, "job") == "" && jobKey != "" {
		gh = setKeySorted(gh, "job", expr.String(jobKey))
	}
	repo := repoName(str(gh, "repository"))
	if repo == "" {
		repo = repoFallback
	}
	if repo == "" {
		return nil, Layout{}, fmt.Errorf("job contexts: github.repository is missing")
	}
	home := "/home/runner"
	s.layout = NewLayout(home, repo, "")
	s.github = setKeySorted(gh, "workspace", expr.String(s.layout.Workspace))
	return s, s.layout, s.initRest(root, matrixJSON, r)
}

// WithLayout replaces the directory layout (tests and non-default homes). It
// must be called before the first step.
func (s *State) WithLayout(l Layout) {
	s.layout = l
	s.github = setKeySorted(s.github, "workspace", expr.String(l.Workspace))
	s.runner = setKey(s.runner, "temp", expr.String(l.Temp))
	s.runner = setKey(s.runner, "tool_cache", expr.String(l.ToolCache))
	s.runner = setKey(s.runner, "workspace", expr.String(l.RunnerWorkspace))
}

func (s *State) initRest(root *expr.Dictionary, matrixJSON string, r RunnerInfo) error {
	for _, k := range serverContexts {
		if v, ok := root.Get(k); ok {
			s.other[k] = v
		}
	}
	if matrixJSON != "" {
		if v, err := expr.FromJSON([]byte(matrixJSON)); err == nil {
			if d, ok := v.(*expr.Dictionary); ok && d.Len() > 0 {
				s.other["matrix"] = d
			}
		} else {
			return fmt.Errorf("job matrix: %w", err)
		}
	}
	// A job without a matrix has the matrix context null on github.com, not an
	// empty object (recordings: "matrix": null in the 176 of 221 jobs that have
	// none, and toJSON(matrix) prints null). The server sends "{}" for such a
	// job (runner.proto: Job.matrix), so an empty matrix becomes null here.
	// matrix.x still works: a property of null is null, as for a missing key.
	if d, ok := s.other["matrix"].(*expr.Dictionary); !ok || d.Len() == 0 {
		s.other["matrix"] = expr.Null{}
	}
	// A context the job does not have is an empty object, like the server's
	// ("needs": {}, "inputs": {}, "vars": {}); matrix and strategy are null
	// when absent (recordings: "matrix": null).
	for _, k := range []string{"needs", "inputs", "vars"} {
		if _, ok := s.other[k]; !ok {
			s.other[k] = expr.NewDictionary()
		}
	}

	// runner: os, arch, name, environment, tool_cache, temp (JobRunner.cs:165-176,
	// TempDirectoryManager.cs:31) and workspace (JobExtension.cs:215).
	env := r.Environment
	if env == "" {
		env = "self-hosted"
	}
	rc := expr.NewDictionary()
	rc.Add("os", expr.String("Linux"))
	rc.Add("arch", expr.String(r.arch()))
	rc.Add("name", expr.String(r.Name))
	rc.Add("environment", expr.String(env))
	rc.Add("tool_cache", expr.String(s.layout.ToolCache))
	rc.Add("temp", expr.String(s.layout.Temp))
	rc.Add("workspace", expr.String(s.layout.RunnerWorkspace))
	s.runner = rc

	// job: status first (StepsRunner.cs:53), then what the server gave.
	jc := expr.NewDictionary()
	jc.Add("status", expr.String("success"))
	if v, ok := root.Get("job"); ok {
		if d, ok := v.(*expr.Dictionary); ok {
			for _, p := range d.Pairs() {
				if !strings.EqualFold(p.Key, "status") {
					jc.Add(p.Key, p.Value)
				}
			}
		}
	}
	s.job = jc
	return nil
}

// Layout returns the directory layout.
func (s *State) Layout() Layout { return s.layout }

// SetGitHub sets a key of the github context (SetGitHubContext).
func (s *State) SetGitHub(k, v string) { s.github = setKeySorted(s.github, k, expr.String(v)) }

// GitHub returns a string key of the github context ("" when absent).
func (s *State) GitHub(k string) string { return str(s.github, k) }

// Event returns github.event, or nil.
func (s *State) Event() expr.Value { v, _ := s.github.Get("event"); return v }

// SetEnv sets a variable of the job environment (Global.EnvironmentVariables
// and the env context).
func (s *State) SetEnv(k, v string) { s.env.Set(k, v) }

// JobEnv returns a copy of the job environment.
func (s *State) JobEnv() *ordered { return s.env.Clone() }

// AddPath records a GITHUB_PATH entry: a repeated entry moves to the end
// (AddPathFileCommand, FileCommandManager.cs:152).
func (s *State) AddPath(p string) {
	s.prepend = slices.DeleteFunc(s.prepend, func(x string) bool { return x == p })
	s.prepend = append(s.prepend, p)
}

// Prepend returns the GITHUB_PATH entries in the order added.
func (s *State) Prepend() []string { return slices.Clone(s.prepend) }

// SetStatus sets job.status ("success", "failure" or "cancelled").
func (s *State) SetStatus(status string) {
	s.status = status
	s.job = setKey(s.job, "status", expr.String(status))
}

// Status returns job.status.
func (s *State) Status() string { return s.status }

// stepIDVisible reports whether a step with this id appears in the steps
// context: generated ids start with "__" and do not (ExecutionContext.cs:644).
func stepIDVisible(id string) bool { return id != "" && !strings.HasPrefix(id, "__") }

func (s *State) step(id string) *stepCtx {
	c, ok := s.steps[id]
	if !ok {
		c = &stepCtx{outputs: newOrdered()}
		s.steps[id] = c
		s.stepOrder = append(s.stepOrder, id)
	}
	return c
}

// SetOutput sets an output of step id (StepsContext.SetOutput). Steps with a
// generated id keep no outputs.
func (s *State) SetOutput(id, name, value string) {
	if !stepIDVisible(id) {
		return
	}
	s.step(id).outputs.Set(name, value)
}

// SetStepResult records outcome and conclusion of a completed step
// (ExecutionContext.UpdateGlobalStepsContext).
func (s *State) SetStepResult(id, outcome, conclusion string) {
	if !stepIDVisible(id) {
		return
	}
	c := s.step(id)
	c.outcome, c.concl = outcome, conclusion
}

// Output returns an output of a step ("" when absent).
func (s *State) Output(id, name string) string {
	if c, ok := s.steps[id]; ok {
		v, _ := c.outputs.Get(name)
		return v
	}
	return ""
}

func (s *State) stepsValue() *expr.Dictionary {
	d := expr.NewDictionary()
	for _, id := range s.stepOrder {
		c := s.steps[id]
		sd := expr.NewDictionary()
		sd.Add("outputs", c.outputs.Dict())
		if c.outcome != "" {
			sd.Add("outcome", expr.String(c.outcome))
			sd.Add("conclusion", expr.String(c.concl))
		}
		d.Add(id, sd)
	}
	return d
}

// Values returns the contexts for expression evaluation. stepEnv, when not
// nil, is the env context of a step (job environment with the step's own
// variables); otherwise the env context is the job environment.
func (s *State) Values(env *ordered) map[string]expr.Value {
	if env == nil {
		env = s.env
	}
	m := map[string]expr.Value{
		"github":  s.github,
		"runner":  s.runner,
		"job":     s.job,
		"env":     env.Dict(),
		"steps":   s.stepsValue(),
		"secrets": s.secrets(),
	}
	for k, v := range s.other {
		m[k] = v
	}
	return m
}

// secrets is the secrets context. Today it holds one entry, GITHUB_TOKEN, as
// on GitHub (Variables.ToSecretsContext lists the secret variables but the
// access token; GitHub's message carries the token as the secret GITHUB_TOKEN,
// recorded: ctx-inputs-vars, secrets.GITHUB_TOKEN == github.token).
//
// Where the value comes from: the "token" key of the github context that the
// server sends in Job.contexts, the same value as github.token. It must be a
// token of its own, scoped to the job (the permissions "Set up job" prints) and
// short lived. It must never be the "ermitejob_" job token: that one
// authenticates the runner's calls to ermite, stays in the runner host process
// and must not reach the sandbox (runner.proto, "Secrets and trust"). ermite
// does not send a token yet (planned for CI-3, with the secrets of the
// repository); until it does, github.token is empty, so is secrets, and
// a test of secrets.GITHUB_TOKEN against the empty string is false. Without a
// token nothing is masked.
func (s *State) secrets() *expr.Dictionary {
	d := expr.NewDictionary()
	if t := s.GitHub("token"); t != "" {
		d.Add("GITHUB_TOKEN", expr.String(t))
	}
	return d
}

// Secrets returns the values that must never leave the job: the token and the
// base64 of "x-access-token:<token>" (ExecutionContext.cs:1026-1028); newMasker
// adds their other encodings.
func (s *State) Secrets() []string {
	t := s.GitHub("token")
	if t == "" {
		return nil
	}
	return []string{t, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + t))}
}

// ServerContexts returns the contexts that the server has when it evaluates
// the name of a job: github as received (without the keys the runner adds),
// needs, strategy, matrix, vars and inputs.
func (s *State) ServerContexts() map[string]expr.Value {
	m := map[string]expr.Value{"github": s.serverGitHub}
	for _, k := range []string{"needs", "strategy", "matrix", "vars", "inputs"} {
		if v, ok := s.other[k]; ok {
			m[k] = v
		}
	}
	return m
}

// githubEnvAllowlist is GitHubContext._contextEnvAllowlist (GitHubContext.cs:9).
var githubEnvAllowlist = map[string]bool{
	"action_path": true, "action_ref": true, "action_repository": true, "action": true, "actor": true,
	"actor_id": true, "api_url": true, "artifacts": true, "artifacts_list": true, "base_ref": true,
	"env": true, "event_name": true, "event_path": true, "graphql_url": true, "head_ref": true,
	"job": true, "output": true, "path": true, "ref_name": true, "ref_protected": true, "ref_type": true,
	"ref": true, "repository": true, "repository_id": true, "repository_owner": true,
	"repository_owner_id": true, "retention_days": true, "run_attempt": true, "run_id": true,
	"run_number": true, "server_url": true, "sha": true, "state": true, "step_summary": true,
	"triggering_actor": true, "workflow": true, "workflow_ref": true, "workflow_sha": true, "workspace": true,
}

// RuntimeEnv is the environment that the contexts add to every step: the
// allow-listed keys of github as GITHUB_<KEY> (strings and booleans only, as in
// GitHubContext.GetRuntimeEnvironmentVariables), and every key of runner as
// RUNNER_<KEY> (RunnerContext.cs:11). Numbers and objects are skipped: the
// server must send GitHub's string forms ("run_id": "123").
func (s *State) RuntimeEnv() *ordered {
	out := newOrdered()
	for _, p := range s.github.Pairs() {
		if !githubEnvAllowlist[strings.ToLower(p.Key)] {
			continue
		}
		switch v := p.Value.(type) {
		case expr.String:
			out.Set("GITHUB_"+strings.ToUpper(p.Key), string(v))
		case expr.Boolean:
			out.Set("GITHUB_"+strings.ToUpper(p.Key), map[bool]string{true: "true", false: "false"}[bool(v)])
		}
	}
	for _, p := range s.runner.Pairs() {
		if v, ok := p.Value.(expr.String); ok {
			out.Set("RUNNER_"+strings.ToUpper(p.Key), string(v))
		}
	}
	return out
}

// EventJSON returns github.event as JSON for event.json, or "" without event.
func (s *State) EventJSON() string {
	v := s.Event()
	if v == nil {
		return ""
	}
	if _, isNull := v.(expr.Null); isNull {
		return ""
	}
	return expr.ToJSON(v)
}
