// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"fmt"
	"strings"

	"bnema.dev/ermite-actions/action"
	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
)

// inputSet is the inputs of a step: a dictionary with OrdinalIgnoreCase keys
// that keeps insertion order (Dictionary<string,string>, which only keeps it
// while nothing is removed).
type inputSet struct {
	keys []string
	vals map[string]string
}

func newInputSet() *inputSet { return &inputSet{vals: map[string]string{}} }

func (in *inputSet) lower(k string) string { return strings.ToLower(k) }

func (in *inputSet) Set(k, v string) {
	l := in.lower(k)
	if _, ok := in.vals[l]; !ok {
		in.keys = append(in.keys, k)
	}
	in.vals[l] = v
}

func (in *inputSet) Get(k string) (string, bool) { v, ok := in.vals[in.lower(k)]; return v, ok }
func (in *inputSet) Has(k string) bool           { _, ok := in.vals[in.lower(k)]; return ok }
func (in *inputSet) Len() int                    { return len(in.keys) }

// evalInputs is EvaluateStepInputs: the "with" of the step (a mapping, or the
// expression of a composite action's step that stands for one).
func (j *job) evalInputs(st *workflow.Step, env *ordered) (*inputSet, error) {
	in := newInputSet()
	var tok template.Token
	switch {
	case st.With != nil:
		tok = st.With
	case st.WithExpression != nil:
		tok = st.WithExpression
	default:
		return in, nil
	}
	v, err := template.Evaluate(context.Background(), tok, j.evalEnv(env))
	if err != nil {
		return nil, err
	}
	d, ok := v.(*expr.Dictionary)
	if !ok {
		return in, nil
	}
	for _, p := range d.Pairs() {
		if k := p.Value.Kind(); k == expr.KindArray || k == expr.KindDictionary {
			return nil, ghErrorf("Unexpected type '%s' encountered while reading 'inputs value'. The type 'StringToken' was expected.", k)
		}
		in.Set(p.Key, expr.CoerceString(p.Value))
	}
	return in, nil
}

// mergeInputs is the second half of ActionRunner.RunAsync's input handling
// (ActionRunner.cs:121-186): deprecation warnings for the inputs the step
// gives, the defaults of the others (evaluated in the context of the step), and
// the warning about inputs the action does not declare. The missing required
// inputs are not checked: the runner has no such check.
func (j *job) mergeInputs(s *stepRun, def *actionDef, in *inputSet) error {
	var act *action.Action
	if def != nil {
		act = def.act
	}
	user := append([]string(nil), in.keys...)
	if act != nil {
		for _, k := range user {
			for i := range act.Inputs {
				if act.Inputs[i].Name == k && act.Inputs[i].Deprecated() {
					j.tagged(s.num, "##[warning]", fmt.Sprintf("Input '%s' has been deprecated with message: %s", k, act.Inputs[i].DeprecationMessage))
				}
			}
		}
	}
	valid := newInputSet()
	if def != nil && def.kind == kindDocker {
		// A container action always accepts 'entryPoint' and 'args'.
		valid.Set("entryPoint", "")
		valid.Set("args", "")
	}
	if act != nil {
		for i := range act.Inputs {
			name := act.Inputs[i].Name
			valid.Set(name, "")
			if in.Has(name) {
				continue
			}
			val, err := j.evalDefault(act.Inputs[i].Default, s.env)
			if err != nil {
				return err
			}
			in.Set(name, val)
		}
	}
	// Only for actions with a manifest or a repository reference (not docker://).
	if s.n.uses.Kind == action.UsesDocker {
		return nil
	}
	var unexpected []string
	for _, k := range user {
		if !valid.Has(k) {
			unexpected = append(unexpected, k)
		}
	}
	if len(unexpected) > 0 {
		j.tagged(s.num, "##[warning]", fmt.Sprintf("Unexpected input(s) '%s', valid inputs are ['%s']",
			strings.Join(unexpected, "', '"), strings.Join(valid.keys, "', '")))
	}
	return nil
}

// evalDefault is EvaluateDefaultInput: the default is a string or an expression
// over github, strategy, matrix, job, runner and hashFiles.
func (j *job) evalDefault(tok template.Token, env *ordered) (string, error) {
	if tok == nil {
		return "", nil
	}
	return j.evalString(tok, env)
}

// inputEnvName is the variable of an input (Handler.AddInputsToEnvironment).
func inputEnvName(key string) string {
	return "INPUT_" + strings.ToUpper(strings.ReplaceAll(key, " ", "_"))
}
