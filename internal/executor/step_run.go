// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"sync"

	"bnema.dev/ermite-actions/action"
	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
)

// qstep is a step of the job's queue (StepsRunner's JobSteps): a main step, the
// pre or post stage of an action, or a setup step (pull or build an image).
type qstep struct {
	n     *node
	stage stage
	name  string
	// cond, for a pre or post stage, replaces the step's own condition (the
	// action's pre-if or post-if); setup steps use "success()".
	cond string
	// setup, when set, is the whole step: it runs instead of the node.
	setup func(s *stepRun, cancel <-chan struct{}) stepResult
}

func (j *job) statusFuncs() workflow.StatusFuncs {
	st := j.state.Status()
	return workflow.StatusFuncs{Success: st == "success", Failure: st == "failure", Cancelled: st == "cancelled"}
}

// actionStatus is github.action_status of the steps of a composite action: the
// result of the action so far (CompositeActionHandler.cs:246), the job's
// cancellation included (:340). j.mu is held.
func (s *stepRun) actionStatus() string {
	if s.j.cancelled {
		return "cancelled"
	}
	if s.result != nil {
		return statusName(*s.result)
	}
	return "success"
}

// statusFuncs are the status functions of the step: success() and failure()
// of an embedded main step read action_status, the others the job's status
// (SuccessFunction.cs:28-45). j.mu is held.
func (s *stepRun) statusFuncs() workflow.StatusFuncs {
	if s.parent != nil && s.stage == stageMain {
		a := s.parent.actionStatus()
		return workflow.StatusFuncs{Success: a == "success", Failure: a == "failure", Cancelled: s.j.state.Status() == "cancelled"}
	}
	return s.j.statusFuncs()
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

// evalCondString evaluates the pre-if or post-if of an action (an expression
// without ${{ }}, evaluated as written, EvaluateStepIf on a bare condition).
func (j *job) evalCondString(cond string, env *ordered, st workflow.StatusFuncs) (bool, error) {
	sch, err := action.Schema()
	if err != nil {
		return false, err
	}
	tok := template.NewExpression(template.Origin{}, cond, sch.Info("step-if").WithContexts("vars", "needs"))
	te := j.evalEnv(env)
	funcs := map[string]expr.Func{}
	for k, f := range te.Funcs {
		funcs[k] = f
	}
	for k, f := range st.Funcs() {
		funcs[k] = f
	}
	te.Funcs = funcs
	v, err := template.EvaluateExpression(context.Background(), tok, te)
	if err != nil {
		return false, err
	}
	return expr.Truthy(v), nil
}

// stepEnvironment builds the environment of a step (StepsRunner.cs:108-139 and
// CompositeActionHandler.cs:233-262): the job's, plus for an embedded step the
// env of the steps around it, then its own env, evaluated against that.
func (j *job) stepEnvironment(s *stepRun) (*ordered, error) {
	j.mu.Lock()
	env := j.state.JobEnv()
	j.mu.Unlock()
	s.overrides = newOrdered()
	if p := s.parent; p != nil {
		for _, k := range p.overrides.Keys() {
			v, _ := p.env.Get(k)
			env.Set(k, v)
			s.overrides.Set(k, v)
		}
	}
	pairs, err := j.evalStepEnv(s.st, env)
	if err != nil {
		return env, err
	}
	for _, p := range pairs {
		env.Set(p.Key, p.Value)
		s.overrides.Set(p.Key, p.Value)
	}
	return env, nil
}

// evalStepEnv evaluates the env of a step: a mapping, or the expression that
// stands for one (a step of a composite action may write env: ${{ ... }}).
func (j *job) evalStepEnv(st *workflow.Step, env *ordered) ([]Pair, error) {
	if st.Env != nil || st.EnvExpression == nil {
		return j.evalMap(st.Env, env)
	}
	v, err := template.Evaluate(context.Background(), st.EnvExpression, j.evalEnv(env))
	if err != nil {
		return nil, err
	}
	return pairsOf(v)
}

func pairsOf(v expr.Value) ([]Pair, error) {
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

// runStep runs one step of the queue as a step of the job (its own number, its
// own record in the log).
func (j *job) runStep(num int, q qstep) {
	rep := j.cfg.Reporter
	s := &stepRun{j: j, num: num, stage: q.stage}
	if q.n != nil {
		s.st, s.n = q.n.st, q.n
		// The state saved by the action is the one of its node, whatever the stage.
		s.scope.stateKey = q.n.stateKey
	}
	s.cmds = newCommandManager(s, j.cfg.Debug)
	name := q.name
	if q.setup != nil {
		s.name = name
		rep.StepStarted(num, name, j.now())
		r := j.runGuarded(s, q, true)
		rep.StepCompleted(num, name, r.conclusion(), j.now())
		return
	}
	if q.n == nil {
		return
	}
	// github.action is the id of the step, set before the step's env is
	// evaluated (StepsRunner.cs:118), so that the env and the if of a step see
	// its own id (the dump step of the recordings reads __run_6, not __run_5).
	j.mu.Lock()
	j.state.SetGitHub("action", q.n.st.ID)
	j.mu.Unlock()
	env, envErr := j.stepEnvironment(s)
	s.env = env
	if name == "" {
		name = j.displayName(q.n.st, s.env)
		if q.stage == stagePre {
			name = "Pre " + name
		}
	}
	s.name = name
	rep.StepStarted(num, name, j.now())
	r := j.runGuarded(s, q, envErr == nil)
	if envErr != nil {
		j.errorf(num, "%s", templateNotValid(envErr, true))
		r = resFailed
		j.finishStep(s, r)
	}
	rep.StepCompleted(num, name, r.conclusion(), j.now())
}

// finishStep records the result of the step in the contexts
// (ExecutionContext.UpdateGlobalStepsContext) and, for a step of the job, in
// the result of the job.
func (j *job) finishStep(s *stepRun, res stepResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := s.st
	outcome := res
	if s.outcome != nil {
		outcome = *s.outcome
	}
	// Pre and post stages of a step of the job have no context name, and
	// generated ids are hidden: only the main stage and embedded steps count.
	if s.n != nil && (s.parent != nil || s.stage == stageMain) && st != nil {
		j.state.SetStepResult(st.ID, string(outcome.conclusion()), string(res.conclusion()))
	}
	if s.parent == nil {
		j.recordOutcome(s.num, outcome.conclusion())
	}
	if s.parent != nil {
		if res == resFailed || res == resCanceled {
			m := merge(s.parent.result, res)
			s.parent.result = &m
		}
		return
	}
	if res == resFailed {
		m := merge(j.result, resFailed)
		j.result = &m
		j.state.SetStatus(statusName(m))
	}
}

// runGuarded is the body of a step between its start and its end: the
// condition, the run with its cancellation watch, the commands' result,
// continue-on-error. ok is false when the env of the step did not evaluate (the
// error is reported by the caller).
func (j *job) runGuarded(s *stepRun, q qstep, ok bool) stepResult {
	num, st := s.num, s.st
	complete := func(r stepResult) stepResult { j.finishStep(s, r); return r }
	if !ok {
		return resFailed
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
	cond := func() (bool, error) {
		j.mu.Lock()
		defer j.mu.Unlock()
		switch {
		case q.setup != nil:
			return j.evalCondString("success()", s.env, j.statusFuncs())
		case q.cond != "":
			return j.evalCondString(q.cond, s.env, s.statusFuncs())
		}
		return st.EvaluateIf(context.Background(), j.evalEnv(s.env), s.statusFuncs())
	}
	// On runner shutdown conditions are not evaluated and the step is skipped.
	if cancelledAtStart && j.cancel.Reason() == CancelShutdown {
		return complete(resSkipped)
	}
	run, err := cond()
	switch {
	case err != nil:
		j.errorf(num, "%s", templateNotValid(err, false))
		return complete(resFailed)
	case !run:
		return complete(resSkipped)
	}
	// Cancellation while the step runs (StepsRunner.cs:149-186): the job becomes
	// cancelled and the step is stopped unless its condition still holds
	// (always()).
	stepCancel := make(chan struct{})
	var cancelOnce sync.Once
	closeCancel := func() { cancelOnce.Do(func() { close(stepCancel) }) }
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
				closeCancel()
			}
		}()
	}
	// timeout-minutes (StepsRunner.RunStepAsync): the timer cancels the body like
	// a job cancellation does, and does not cancel the job. An embedded step
	// has its own timer, the step around it has its own.
	var r stepResult
	stopTimeout, minutes := func() (func(), int) { return func() {}, 0 }()
	if q.setup == nil {
		stopTimeout, minutes = j.armStepTimeout(s, closeCancel)
	}
	if q.setup != nil {
		r = q.setup(s, stepCancel)
	} else {
		r = j.execute(s, stepCancel)
	}
	stopTimeout()
	close(stopWatch)
	watch.Wait()
	r = j.stepTimedOut(s, r, minutes)

	// Merge the result of the commands (StepsRunner.cs:347-351).
	if s.cmdResult != nil {
		r = merge(&r, *s.cmdResult)
	}
	// continue-on-error (ExecutionContext.ApplyContinueOnError, :1422).
	if r == resFailed && st != nil && st.ContinueOnError != nil {
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
	return complete(r)
}
