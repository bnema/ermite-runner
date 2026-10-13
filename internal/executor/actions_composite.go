// SPDX-License-Identifier: MIT

package executor

import (
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

// prepareLocalComposite is the first part of ActionRunner.RunAsync for a local
// composite action (ActionRunner.cs:77-96): its steps are prepared when the
// step runs, in the log of the step, and the pulls and builds they need run
// first as embedded steps.
func (j *job) prepareLocalComposite(s *stepRun, def *actionDef) error {
	n := s.n
	if n.children != nil {
		return nil // pre and post stages reuse the nodes of the main stage
	}
	kids, err := newNodes(def.act.Runs.Steps, n, n.scope)
	if err != nil {
		return err
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
	if err := j.prepare(j.ctx(), s.num, kids, 1, n); err != nil {
		return err
	}
	setups := j.setupSteps()
	j.actions.pullOrder, j.actions.buildOrder = saved.pullOrder, saved.buildOrder
	j.actions.pullUsers, j.actions.buildUsers = saved.pullUsers, saved.buildUsers
	for _, q := range setups {
		if r := q.setup(s, nil); r != resSucceeded {
			return errSetupFailed
		}
	}
	return nil
}

// errSetupFailed ends a composite whose image setup failed; the cause is in the
// log already.
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
		j.runEmbedded(s, c)
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
func (j *job) runEmbedded(parent *stepRun, c *node) {
	s := &stepRun{j: j, num: parent.num, st: c.st, n: c, stage: parent.stage, parent: parent, fc: j.fc.Fork()}
	defer s.fc.Cleanup()
	s.cmds = newCommandManager(s, j.cfg.Debug)
	j.mu.Lock()
	// github.action_status: the result of the composite action so far.
	j.state.SetGitHub("action_status", parent.actionStatus())
	j.mu.Unlock()
	env, envErr := j.stepEnvironment(s)
	s.env = env
	s.displayName = j.displayName(c.st, env)
	if envErr != nil {
		j.errorf(s.num, "%s", templateNotValid(envErr, true))
		j.finishStep(s, resFailed)
		return
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
	j.runGuarded(s, q, true)
}
