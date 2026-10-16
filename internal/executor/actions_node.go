// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bnema.dev/ermite-actions/action"
)

const (
	node20Name = "node20"
	node24Name = "node24"
	// Node20DeprecationURL is Constants.Runner.NodeMigration.Node20DeprecationUrl.
	node20DeprecationURL = "https://github.blog/changelog/2025-09-19-deprecation-of-node-20-on-github-actions-runners/"
	forceNode24Var       = "FORCE_JAVASCRIPT_ACTIONS_TO_NODE24"
	allowUnsecureVar     = "ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION"
)

// convertToBool is StringUtil.ConvertToBoolean with a false default.
func convertToBool(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "$true":
		return true
	}
	return false
}

// envFlag is NodeUtil.GetEnvironmentVariableDetails: the value comes from the
// workflow's environment (the env context of the step) when it is set there,
// from the process of the runner otherwise.
type envFlag struct{ on, fromWorkflow, fromSystem bool }

func (j *job) nodeEnvFlag(name string, wf *ordered) envFlag {
	var f envFlag
	v, found := wf.Get(name)
	if found {
		f.fromWorkflow = true
		f.on = convertToBool(v)
	}
	sys := envValue(j.baseEnv, name)
	f.fromSystem = sys != ""
	if !found {
		f.on = convertToBool(sys)
	}
	return f
}

// nodeVersionDefault is NodeUtil.DetermineActionsNodeVersion
// (NodeUtil.cs:50-110) for an action that says node20: the version the action
// runs on and the warning of a conflicting configuration. useNode24ByDefault and
// requireNode24 are the server's flags, ActionsConfig.Node24ByDefault and
// RequireNode24 here.
func (j *job) nodeVersionDefault(wf *ordered) (version, warning string) {
	force := j.nodeEnvFlag(forceNode24Var, wf)
	allow := j.nodeEnvFlag(allowUnsecureVar, wf)
	if j.cfg.Actions.RequireNode24 {
		// Phase 3 (NodeUtil.cs:60-63): node24 whatever the variables say, and
		// no warning about them.
		return node24Name, ""
	}
	byDefault := j.cfg.Actions.Node24ByDefault
	bothWorkflow := force.on && allow.on && force.fromWorkflow && allow.fromWorkflow
	bothSystem := force.on && allow.on && force.fromSystem && allow.fromSystem
	if bothWorkflow || bothSystem {
		source := "system"
		if bothWorkflow {
			source = "workflow"
		}
		def := node20Name
		if byDefault {
			def = node24Name
		}
		return def, fmt.Sprintf("Both %s and %s environment variables are set to true in the %s environment. This is likely a configuration error. Using the default Node version: %s.",
			forceNode24Var, allowUnsecureVar, source, def)
	}
	if byDefault {
		if allow.on {
			return node20Name, ""
		}
		return node24Name, ""
	}
	if force.on {
		return node24Name, ""
	}
	return node20Name, ""
}

// nodeVersionFor is the node part of HandlerFactory.Create
// (HandlerFactory.cs:56-170): node12 and node16 run on node20, node20 on
// whatever DetermineActionsNodeVersion says, node24 on node24. The result is
// kept in the step for the handler.
func (j *job) nodeVersionFor(s *stepRun, def *actionDef) error {
	r := def.runs()
	if r == nil || def.kind != kindNode {
		return nil
	}
	v := r.Using
	if v == "node12" || v == "node16" {
		v = node20Name
	}
	if v == node20Name {
		final, warn := j.nodeVersionDefault(s.env)
		v = final
		if warn != "" {
			j.tagged(s.num, "##[warning]", warn)
		}
		if final == node24Name {
			j.mu.Lock()
			j.upgradedToNode24 = appendUnique(j.upgradedToNode24, actionName(s.n))
			j.mu.Unlock()
			if j.cfg.Actions.Node24ByDefault && !j.cfg.Actions.RequireNode24 {
				j.out(s.num, "Node 20 is being deprecated. This workflow is running with Node 24 by default. "+
					"If you need to temporarily use Node 20, you can set the ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION=true environment variable. "+
					"For more information see: "+node20DeprecationURL)
			}
		}
	}
	s.nodeVersion = v
	return nil
}

func appendUnique(l []string, v string) []string {
	if v == "" {
		return l
	}
	for _, x := range l {
		if x == v {
			return l
		}
	}
	return append(l, v)
}

// actionName is HandlerFactory.GetActionName: "Name[/Path]@Ref".
func actionName(n *node) string {
	if n.uses.Kind == action.UsesLocal {
		return n.uses.Path // no name: HandlerFactory.cs:200-208 uses the path alone
	}
	return registeredRef(n.uses)
}

// externalsDir is where the node versions live.
func (j *job) externalsDir() string {
	if d := j.cfg.Actions.ExternalsDir; d != "" {
		return d
	}
	return defaultExternalsDir
}

// addRuntimeEnv is the "Add Actions Runtime server info" part of the handlers
// (NodeScriptActionHandler.cs:55-72, ContainerActionHandler.cs:219-236): the
// variables @actions/cache reads. ermite has no Actions runtime service, only
// the cache of the runner, so only ACTIONS_CACHE_URL (the base of
// "_apis/artifactcache/…": the toolkit appends to it, cacheHttpClient.ts:47, so
// it ends with a slash), ACTIONS_RUNTIME_URL (same value, for clients that
// require the variable) and ACTIONS_RUNTIME_TOKEN are set. ACTIONS_CACHE_SERVICE_V2
// is never set: the toolkit would then ignore ACTIONS_CACHE_URL (config.ts:42-58)
// and, on a server URL that is not github.com, uses v1 anyway (config.ts:22-27).
// Without a cache service in the job nothing is set.
func (j *job) addRuntimeEnv(env *ordered) {
	if j.cfg.CacheURL == "" || j.cfg.CacheToken == "" {
		return
	}
	env.Set("ACTIONS_RUNTIME_URL", j.cfg.CacheURL)
	env.Set("ACTIONS_RUNTIME_TOKEN", j.cfg.CacheToken)
	env.Set("ACTIONS_CACHE_URL", j.cfg.CacheURL)
	// ACTIONS_CACHE_MODE: NodeScriptActionHandler.cs:71-84, ContainerActionHandler.cs:
	// 237-245; the toolkit skips uploads in "read" mode (cache config.ts:27-40).
	if j.cfg.CacheMode != "" {
		env.Set("ACTIONS_CACHE_MODE", j.cfg.CacheMode)
	}
}

// runNode is NodeScriptActionHandler.RunAsync (NodeScriptActionHandler.cs:
// 27-196) on the host of the steps (a job container is phase H): INPUT_
// variables and the PATH of the previous steps in the environment, the
// script of the stage, node from the externals, run in the workspace.
func (j *job) runNode(s *stepRun, def *actionDef, in *inputSet, env *ordered, cancel <-chan struct{}) stepResult {
	num := s.num
	for _, k := range in.keys {
		v, _ := in.Get(k)
		env.Set(inputEnvName(k), v)
	}
	j.addRuntimeEnv(env)
	// The PATH of the previous steps is added by procEnv.
	env.Remove("NODE_ICU_DATA") // https://github.com/actions/runner/issues/795
	r := def.runs()
	var target string
	switch s.stage {
	case stageMain:
		target = r.Main
	case stagePre:
		target = r.Pre
	case stagePost:
		target = r.Post
	}
	fail := func(err error) stepResult { j.errorf(num, "%s", err.Error()); return resFailed }
	if target == "" {
		return fail(ghErrorf("Value cannot be null or empty. (Parameter 'target')"))
	}
	script := pathCombine(def.dir, filepath.FromSlash(target))
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		return fail(ghErrorf("File not found: '%s'", script))
	}
	if c := j.stepContainer(); c != nil {
		return j.runNodeInContainer(s, c, script, env, cancel)
	}
	j.mu.Lock()
	wd := j.state.GitHub("workspace")
	j.mu.Unlock()
	node := filepath.Join(j.externalsDir(), s.nodeVersion, "bin", "node")
	s.actEnv = env
	return j.runProc(s, node, wd, []string{node, script}, "Node Action run completed with exit code %d", cancel)
}
