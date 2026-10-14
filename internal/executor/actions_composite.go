// SPDX-License-Identifier: MIT

package executor

import (
	"context"

	"bnema.dev/ermite-actions/expr"
)

// compositeGitHubKeys are the keys of the github context that a composite
// action's steps change: the runner gives them a copy of the context
// (CompositeActionHandler.cs:152-161), the executor restores these afterwards.
func compositeGitHubKeys() []string {
	keys := []string{"action_path", "action_status", "action_repository", "action_ref", "event_path"}
	for _, c := range fileCommands {
		keys = append(keys, c.ContextName)
	}
	return keys
}

// maxLocalDepth is the nesting of composite actions the executor allows. It is
// ermite's guard against a local composite action that uses itself, not a limit
// of GitHub's, whose runner has none for local actions.
const maxLocalDepth = 100

// prepareLocalComposite is the first part of ActionRunner.RunAsync for a local
// composite action (ActionRunner.cs:77-96): its steps are prepared when the
// step runs, in the log of the step, and the pulls and builds they need run
// first as embedded steps.
func (j *job) prepareLocalComposite(s *stepRun, def *actionDef, cancel <-chan struct{}) error {
	n := s.n
	if n.children != nil {
		return nil // pre and post stages reuse the nodes of the main stage
	}
	// Local composite actions have no depth limit on GitHub: the recording of
	// ermite-conformance corpus/act-composite-depth has composites of 9, 10 and 12
	// levels, all of which succeed. The runner prepares the steps of a local
	// composite when its step runs, with the depth restarting at 1
	// (ActionRunner.cs:77-96), which is why. This port does the same, and the
	// resolutions it needs are cached and budgeted by the host (host/resolve.go).
	//
	// The one guard that is ermite's own, not GitHub's: a local composite that
	// uses itself (directly or in a cycle) would recurse until the executor
	// crashes, so the nesting of composite actions is capped at maxLocalDepth.
	if d := s.depth(); d >= maxLocalDepth {
		return ghErrorf("Composite action depth exceeded max depth %d", maxLocalDepth)
	}
	kids, err := newNodes(def.act.Runs.Steps, n, n.scope)
	if err != nil {
		return err
	}
	for _, k := range kids {
		k.runtimePrepared = true
	}
	n.children = kids
	n.preSteps, n.postSteps = nil, nil
	j.out(s.num, "Prepare all required actions")
	if j.actions == nil {
		j.actions = newActionState()
	}
	saved := *j.actions
	j.actions.pullOrder, j.actions.buildOrder = nil, nil
	j.actions.pullUsers, j.actions.buildUsers = map[string][]*node{}, map[string][]*node{}
	// The preparation stops with the step, and only with it: its context ends when
	// the cancel channel of the step closes (job cancellation for a step that does
	// not run on always(), timeout-minutes). A step that keeps running after the
	// job was cancelled keeps the means to prepare its composite.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if cancel != nil {
		go func() {
			select {
			case <-cancel:
				stop()
			case <-ctx.Done():
			}
		}()
	}
	if err := j.prepare(ctx, s.num, kids, 1, n); err != nil {
		return err
	}
	setups := j.setupSteps()
	j.actions.pullOrder, j.actions.buildOrder = saved.pullOrder, saved.buildOrder
	j.actions.pullUsers, j.actions.buildUsers = saved.pullUsers, saved.buildUsers
	for _, q := range setups {
		if r := q.setup(s, cancel); r != resSucceeded {
			if r == resCanceled {
				return errSetupCancelled
			}
			return errSetupFailed
		}
	}
	return nil
}

// errSetupFailed ends a composite whose image setup failed; the cause is in the
// log already.
// errSetupCancelled: the step was cancelled while its images were prepared; the
// cancellation is logged by the docker command already.
var errSetupCancelled = ghErrorf("The operation was canceled.")

var errSetupFailed = ghErrorf("A container setup step of the composite action failed.")

// runComposite is CompositeActionHandler.RunAsync (CompositeActionHandler.cs:
// 30-207) and RunStepsAsync: the steps of the stage run in their own steps
// context with the inputs of the action, and the outputs are mapped from them.
func (j *job) runComposite(s *stepRun, def *actionDef, in *inputSet, env *ordered) stepResult {
	n := s.n
	var kids []*node
	switch s.stage {
	case stagePre:
		kids = n.preSteps
	case stagePost:
		// Only the steps that registered a post, last registered first.
		for i := len(n.postSteps) - 1; i >= 0; i-- {
			if c := n.postSteps[i]; c.postRegistered {
				kids = append(kids, c)
			}
		}
	default:
		kids = n.children
	}
	inputs := expr.NewDictionary()
	for _, k := range in.keys {
		v, _ := in.Get(k)
		inputs.Add(k, expr.String(v))
	}
	// The pre stage has no context name in the runner, so its steps context is a
	// random one; post shares the scope of main (siblingScopeName).
	scope := n.scope
	if s.stage == stagePre {
		scope = "__pre_" + n.scope
	}
	j.mu.Lock()
	restoreGH := j.state.GitHubKeys(compositeGitHubKeys()...)
	restoreScope := j.state.PushScope(scope, inputs)
	j.state.SetGitHub("action_path", def.dir)
	j.mu.Unlock()
	restored := false
	restore := func() {
		if !restored {
			restored = true
			restoreScope()
			restoreGH()
		}
	}
	defer func() {
		j.mu.Lock()
		restore()
		j.mu.Unlock()
	}()
	ok := resSucceeded
	s.result = &ok
	for _, c := range kids {
		j.runEmbedded(s, c, scope)
	}
	// Outputs are evaluated in the steps context of the action and belong to the
	// step that uses it (ExecutionContext.SetOutput of the composite step).
	var outs []Pair
	if s.stage == stageMain {
		outs = j.compositeOutputs(s, def)
	}
	j.mu.Lock()
	restore()
	for _, o := range outs {
		j.state.SetOutput(s.st.ID, o.Key, o.Value)
	}
	j.mu.Unlock()
	return *s.result
}

// compositeOutputs is ProcessOutputs (CompositeActionHandler.cs:192-228): the
// "value" of each output of the action, evaluated with the steps and inputs
// of the action.
func (j *job) compositeOutputs(s *stepRun, def *actionDef) []Pair {
	var out []Pair
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, o := range def.act.Outputs {
		if o.Value == nil {
			continue
		}
		v, err := j.evalString(o.Value, s.env)
		if err != nil {
			j.mu.Unlock()
			j.errorf(s.num, "%s", templateNotValid(err, true))
			j.mu.Lock()
			m := resFailed
			s.result = &m
			return nil
		}
		out = append(out, Pair{o.Name, v})
	}
	return out
}

// runEmbedded is one iteration of CompositeActionHandler.RunStepsAsync: the
// step of the composite action runs inside the step that uses it, with its own
// environment, condition, cancellation watch and result. Its log lines belong
// to the outer step.
//
// scope is the steps context of the action; with the id of the step it is the
// id of the step's markers (the fully qualified context name).
func (j *job) runEmbedded(parent *stepRun, c *node, scope string) {
	s := &stepRun{j: j, num: parent.num, st: c.st, n: c, stage: parent.stage, parent: parent, fc: j.fc.Fork()}
	// The issues, summaries and state of an inner step belong to the step that
	// uses the action, whose collector counts them (10 per level, the outer
	// step's own first).
	s.scope = j.embeddedScope(parent, c.stateKey)
	defer s.fc.Cleanup()
	s.cmds = newCommandManager(s, j.cfg.Debug)
	j.mu.Lock()
	// github.action_status: the result of the composite action so far.
	j.state.SetGitHub("action_status", parent.actionStatus())
	j.mu.Unlock()
	env, envErr := j.stepEnvironment(s)
	s.env = env
	s.name = j.displayNameOf(c.st, env, true)
	// The display name is masked before it is cut and escaped. The runner masks
	// the finished line (ExecutionContext.Write), after SanitizeDisplayName and
	// EscapeValue, so a secret holding ";", "]" or "%" (escaped by then) or cut by
	// the 1000-character limit would escape its masker. Masking first is stricter;
	// the line is masked again when it is written, as every line is.
	id := scope + "." + c.st.ID
	if envErr != nil {
		// The error of the environment is logged and the step marked failed
		// (CompositeActionHandler.cs:296-302, SetStepConclusion :529-533), then the
		// runner does not stop: it prints the start marker (:308-324), evaluates
		// the condition (:372-390; success() of an embedded main step reads
		// github.action_status, set from the composite's result before the env was
		// evaluated, :246-248, SuccessFunction.cs:28, so the default condition
		// still holds) and runs the step (RunStepAsync :481-527) without the env
		// that failed. Its result stays Failed (nothing resets it), and the end
		// marker (:431-440) carries it with the elapsed time.
		j.errorf(s.num, "%s", templateNotValid(envErr, true))
		s.envFailed = true
		// SetStepConclusion publishes Failed to the steps context at once
		// (:529-533, UpdateGlobalStepsContext), so the condition of the step, and
		// of the steps after it, see steps.<id>.outcome == 'failure'. Nothing is
		// merged into the composite's result yet: that is done when the step ends
		// (:505-512, finishStep), which also replaces this entry with the final
		// outcome and conclusion.
		j.mu.Lock()
		j.state.SetStepResult(c.st.ID, string(resFailed.conclusion()), string(resFailed.conclusion()))
		j.mu.Unlock()
	}
	q := qstep{n: c, stage: parent.stage}
	switch parent.stage {
	case stagePre:
		if c.def != nil {
			q.cond = c.def.preIf()
		}
	case stagePost:
		q.cond = c.postCond
	}
	j.out(parent.num, startMarker(j.mask(s.name), id))
	start := j.now()
	r := j.runGuarded(s, q, true)
	// Outcome is the result before continue-on-error, conclusion the final one;
	// a skipped step takes no time (CompositeActionHandler.cs:415, :448).
	outcome := r
	if s.outcome != nil {
		outcome = *s.outcome
	}
	ms := j.now().Sub(start).Milliseconds()
	if r == resSkipped {
		ms = 0
	}
	j.out(parent.num, endMarker(id, markerResult(outcome), markerResult(r), ms))
}
