// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"errors"
	"strings"

	"bnema.dev/ermite-actions/action"
	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
)

// ---------------------------------------------------------------- steps

// stepRun is the state of one step being run; it implements Sink and
// commandHost. An embedded step (inside a composite action) is a stepRun too,
// with a parent and the number of the outer step: its log lines belong to it.
type stepRun struct {
	j         *job
	num       int
	st        *workflow.Step
	n         *node
	stage     stage
	parent    *stepRun
	env       *ordered // the step's environment: job env + step env
	overrides *ordered // the step env of this step and of its composite parents
	cmdResult *stepResult
	outcome   *stepResult
	cmds      *commandManager
	// result is ExecutionContext.Result of a step that runs embedded steps: it
	// gives action_status to them.
	result *stepResult
	// fc is the file commands of the step: the job's for a step of the job, a
	// fork for an embedded one.
	fc *FileCommands
	// actEnv, when set, is the environment of the process of an action step
	// (env context, STATE_ and INPUT_); rawEnvList replaces the whole
	// environment of the process (docker, whose -e options read it).
	actEnv     *ordered
	rawEnvList []string
	// displayName is the name of the step in the log; def the action it runs.
	displayName string
	def         *actionDef
	nodeVersion string
}

// depth is the number of composite actions around the step.
func (s *stepRun) depth() int {
	d := 0
	for p := s.parent; p != nil; p = p.parent {
		d++
	}
	return d
}

// procEnv is the environment of the process the step starts.
func (s *stepRun) procEnv() []string {
	if s.rawEnvList != nil {
		return s.rawEnvList
	}
	if s.actEnv != nil {
		return s.j.processEnv(s.actEnv)
	}
	return s.j.processEnv(s.env)
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
		// The name is built from the parsed reference (ActionRunner.cs:305-333):
		// the image of a docker:// step without its scheme, "Name[/Path]@Ref" for
		// a repository, the path for a local action.
		if u, err := action.ParseUses(st.Uses.Value); err == nil {
			if u.Kind == action.UsesDocker {
				return formatStepName("Run ", u.Image)
			}
			return formatStepName("Run ", refString(u))
		}
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
