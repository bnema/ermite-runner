// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"path/filepath"
	"strings"

	"bnema.dev/ermite-actions/action"
)

// stageName is for messages.
func (s stage) String() string {
	switch s {
	case stagePre:
		return "Pre"
	case stagePost:
		return "Post"
	}
	return "Main"
}

// definition is ActionManager.LoadAction for the step: the manifest of a local
// action is read when the step runs; the other actions were loaded by the
// pre-job phase.
func (j *job) definition(s *stepRun) (*actionDef, error) {
	n := s.n
	switch n.uses.Kind {
	case action.UsesDocker:
		return &actionDef{kind: kindDocker, image: n.uses.Image}, nil
	case action.UsesLocal:
		dir := j.actionDir(n)
		def, err := j.loadDef(s.num, dir)
		if err == errNoAction {
			return nil, ghErrorf("Can't find 'action.yml', 'action.yaml' or 'Dockerfile' under '%s'. Did you forget to run actions/checkout before running your local action?", filepath.Clean(dir))
		}
		if err != nil {
			return nil, err
		}
		n.def = def
		return def, nil
	}
	if n.def == nil {
		return nil, ghErrorf("The action '%s' was not prepared", refString(n.uses))
	}
	return n.def, nil
}

// setActionRepository sets github.action_repository and action_ref
// (ActionRunner.cs:149-165): the repository and ref of a repository action, empty
// for anything else.
func (j *job) setActionRepository(n *node) {
	repo, ref := "", ""
	if n != nil && n.uses.Kind == action.UsesRepository {
		repo, ref = n.uses.Name, n.uses.Ref
	}
	j.mu.Lock()
	j.state.SetGitHub("action_repository", repo)
	j.state.SetGitHub("action_ref", ref)
	j.mu.Unlock()
}

// registerPost registers the post stage of the action (ActionRunner.cs:99-126,
// ExecutionContext.RegisterPostJobStep): a step "Post <name>" on the stack the
// job runs after its steps, last registered first; inside a composite action
// only the fact is recorded, the composite's own post runs the step.
func (j *job) registerPost(s *stepRun, def *actionDef) {
	n := s.n
	name := s.name
	if s.stage == stagePre && strings.HasPrefix(strings.ToLower(name), "pre ") {
		name = name[len("Pre "):]
	}
	j.debug(s.num, "Register post job cleanup for action: "+registeredRef(n.uses))
	if s.parent != nil {
		n.postRegistered, n.postCond = true, def.postIf()
		return
	}
	if n.postRegistered {
		return // StepsWithPostRegistered
	}
	n.postRegistered = true
	j.mu.Lock()
	j.postStack = append(j.postStack, qstep{n: n, stage: stagePost, name: "Post " + name, cond: def.postIf()})
	j.mu.Unlock()
}

// registeredRef is the text of the debug line of the registration.
func registeredRef(u action.Uses) string {
	path := ""
	if u.Path != "" {
		path = "/" + u.Path
	}
	if u.Ref == "" {
		return u.Name + path
	}
	return u.Name + path + "@" + u.Ref
}

// handlerGroup is Handler.PrintActionDetails's group name.
func handlerGroup(u action.Uses) string {
	switch u.Kind {
	case action.UsesDocker:
		return "Run docker://" + u.Image
	case action.UsesLocal:
		return "Run " + u.Path
	}
	if u.Path == "" {
		return fmt.Sprintf("Run %s@%s", u.Name, u.Ref)
	}
	return fmt.Sprintf("Run %s/%s@%s", u.Name, u.Path, u.Ref)
}

// printDetails is Handler.PrintActionDetails (Handler.cs:102-165): the group
// of the step with its inputs that are not empty and its environment. The post
// stage only prints "Post job cleanup.".
func (j *job) printDetails(s *stepRun, in *inputSet, env *ordered) {
	num := s.num
	if s.stage == stagePost {
		j.out(num, "Post job cleanup.")
		return
	}
	j.out(num, "##[group]"+handlerGroup(s.n.uses))
	if in.Len() > 0 {
		j.out(num, "with:")
		for _, k := range in.keys {
			if v, _ := in.Get(k); v != "" {
				j.out(num, fmt.Sprintf("  %s: %s", k, v))
			}
		}
	}
	if env.Len() > 0 {
		j.out(num, "env:")
		for _, k := range env.Keys() {
			v, _ := env.Get(k)
			j.out(num, fmt.Sprintf("  %s: %s", k, v))
		}
	}
	j.out(num, "##[endgroup]")
}

// runAction is ActionRunner.RunAsync (ActionRunner.cs:62-247) for a uses step
// in any stage: load the action, evaluate the inputs and the environment, print
// the details and run the handler of the action.
func (j *job) runAction(s *stepRun, cancel <-chan struct{}) stepResult {
	num, n := s.num, s.n
	fail := func(err error) stepResult { j.errorf(num, "%s", err.Error()); return resFailed }
	def, err := j.definition(s)
	if err != nil {
		return fail(err)
	}
	s.def = def
	local := n.uses.Kind == action.UsesLocal
	// A local composite action is expanded when it runs (ActionRunner.cs:77-96).
	if def.kind == kindComposite && s.stage == stageMain && local {
		if err := j.prepareLocalComposite(s, def, cancel); err != nil {
			if err == errSetupCancelled {
				return resCanceled // logged by the command that was stopped
			}
			return fail(err)
		}
	}
	if local && n.hasPre() {
		j.tagged(num, "##[warning]", fmt.Sprintf("`pre` execution is not supported for local action from '%s'", n.uses.Path))
	}
	if n.hasPostOf(def) && s.stage != stagePost {
		j.registerPost(s, def)
	}
	j.setActionRepository(n)
	if err := j.prepareFileCommands(s); err != nil {
		return fail(err)
	}
	defer s.fc.Process(s)

	j.debug(num, "Loading inputs")
	in, err := j.evalInputs(s.st, s.env)
	if err != nil {
		j.errorf(num, "%s", templateNotValid(err, true))
		return resFailed
	}
	if err := j.mergeInputs(s, def, in); err != nil {
		j.errorf(num, "%s", templateNotValid(err, true))
		return resFailed
	}
	j.debug(num, "Loading env")
	env := s.env.Clone()
	// The intra-action state, applied last (ActionRunner.cs:255-258): names as
	// first spelled, in the order they were saved.
	for _, p := range j.intraActionState(n.stateKey) {
		env.Set("STATE_"+p.Key, p.Value)
	}
	s.actEnv = env
	if err := j.nodeVersionFor(s, def); err != nil {
		return fail(err)
	}
	j.printDetails(s, in, env)
	switch def.kind {
	case kindNode:
		return j.runNode(s, def, in, env, cancel)
	case kindDocker:
		return j.runDocker(s, def, in, env, cancel)
	case kindComposite:
		return j.runComposite(s, def, in, env)
	}
	return fail(ghErrorf("Unsupported action type: %v", def.kind))
}

// hasPostOf is HasPost of the loaded definition of the node.
func (n *node) hasPostOf(def *actionDef) bool {
	switch def.kind {
	case kindComposite:
		return len(n.postSteps) > 0
	case kindScript:
		return false
	}
	if r := def.runs(); r != nil {
		return r.HasPost()
	}
	return false
}
