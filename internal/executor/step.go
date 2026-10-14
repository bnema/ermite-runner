// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
)

// ---------------------------------------------------------------- steps

// stepRun is the state of one step being run; it implements Sink and
// commandHost.
type stepRun struct {
	j         *job
	num       int
	st        *workflow.Step
	env       *ordered // the step's environment: job env + step env
	cmdResult *stepResult
	outcome   *stepResult
	cmds      *commandManager
	// scope is what the step shares with the step that contains it, see
	// stepScope. name is the display name; timedOut is set when the step
	// reached its timeout-minutes.
	scope    stepScope
	name     string
	timedOut timedOutFlag
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
	v := strings.ToLower(envValue(s.j.baseEnv, name))
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
	// JobExtension evaluates the display names once, when the job starts, with
	// the contexts the server sent (github.job is empty there) and without the
	// step functions; ActionRunner.GenerateDisplayName only does it when every
	// context and function the name uses is available (CheckHasRequiredContext).
	// A name that is fully evaluated then stays as it is (recorded:
	// ctx-github-names, "Run echo \"job=\""); the others are evaluated again,
	// with the contexts of the step, when the step starts.
	var name string
	var err error
	if serverEvaluable(tok) {
		j.mu.Lock()
		sc := j.state.ServerContexts()
		j.mu.Unlock()
		name, err = template.EvaluateString(context.Background(), tok, template.Env{Contexts: sc})
	} else {
		name, err = j.evalString(tok, env)
	}
	if err != nil {
		name = tokenSource(tok)
	}
	return formatStepName(prefix, name)
}

// serverContextNames are the contexts of the job message (ContextData) when
// display names are first evaluated.
var serverContextNames = map[string]bool{"github": true, "needs": true, "strategy": true, "matrix": true, "inputs": true, "vars": true}

// wellKnownFuncs are the functions available without the step functions.
var wellKnownFuncs = map[string]bool{"case": true, "contains": true, "endswith": true, "format": true, "join": true, "startswith": true, "tojson": true, "fromjson": true}

// serverEvaluable is TemplateTokenExtensions.CheckHasRequiredContext for the
// first evaluation of a display name: every expression of the token only
// reads contexts of the job message and calls well-known functions.
func serverEvaluable(tok template.Token) bool {
	ok := true
	template.Walk(tok, func(t template.Token) bool {
		if be, isExpr := t.(*template.BasicExpression); isExpr {
			ok = ok && nodeServerEvaluable(be.Node())
		}
		return ok
	})
	return ok
}

func nodeServerEvaluable(n expr.Node) bool {
	switch n := n.(type) {
	case *expr.ContextAccess:
		return serverContextNames[strings.ToLower(n.Name.Lexeme)]
	case *expr.Unary:
		return nodeServerEvaluable(n.Expr)
	case *expr.Binary:
		return nodeServerEvaluable(n.Left) && nodeServerEvaluable(n.Right)
	case *expr.Grouping:
		return nodeServerEvaluable(n.Group)
	case *expr.IndexAccess:
		return nodeServerEvaluable(n.Expr) && nodeServerEvaluable(n.Index)
	case *expr.Logical:
		for _, a := range n.Args {
			if !nodeServerEvaluable(a) {
				return false
			}
		}
	case *expr.FunctionCall:
		if !wellKnownFuncs[strings.ToLower(n.Name.Lexeme)] {
			return false
		}
		for _, a := range n.Args {
			if !nodeServerEvaluable(a) {
				return false
			}
		}
	}
	return true
}

// exceptionClasser is implemented by the errors of package expr that stand for
// a .NET exception (expr.JSONError).
type exceptionClasser interface{ ExceptionClass() string }

// templateNotValid is the message of the TemplateValidationException the
// runner throws when the evaluation of a template fails
// (TemplateStrings.TemplateNotValidWithErrors): "The template is not valid. "
// and the errors. With a position the error reads "file (Line: l, Col: c):
// message" (recorded: expr-runtime-errors, step env and run). A step condition
// has no position: the error is the exception itself, "Class: message"
// (withLocation is false).
func templateNotValid(err error, withLocation bool) string {
	var te *template.Error
	if !errors.As(err, &te) {
		return err.Error()
	}
	if withLocation {
		return "The template is not valid. " + te.Error()
	}
	var c exceptionClasser
	if errors.As(err, &c) {
		return "The template is not valid. " + c.ExceptionClass() + ": " + te.Raw
	}
	return te.Error()
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
	// github.action is the id of the step, set before the step's env is
	// evaluated (StepsRunner.cs:118), so that the env and the if of a step see
	// its own id (the dump step of the recordings reads __run_6, not __run_5).
	j.state.SetGitHub("action", st.ID)
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
	s.name = name
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
		j.recordOutcome(num, outcome.conclusion())
		if res == resFailed {
			m := merge(j.result, resFailed)
			j.result = &m
			j.state.SetStatus(statusName(m))
		}
		j.mu.Unlock()
		rep.StepCompleted(num, name, res.conclusion(), j.now())
	}

	if envErr != nil {
		j.errorf(num, "%s", templateNotValid(envErr, true))
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
		j.errorf(num, "%s", templateNotValid(err, false))
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

	stopTimeout, minutes := j.armStepTimeout(s, closeCancel)
	defer stopTimeout()

	r := j.execute(s, stepCancel)
	close(stopWatch)
	watch.Wait()
	r = j.stepTimedOut(s, r, minutes)

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
	}
	return ""
}
