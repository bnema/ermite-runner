// SPDX-License-Identifier: MIT

package executor

import (
	"strings"

	"bnema.dev/ermite-actions/expr"
)

// stepsScope is a steps context: the one of the job, or the one of a composite
// action's steps (StepsContext.GetScope in the runner), plus the inputs the
// steps of that composite see.
type stepsScope struct {
	order  []string
	steps  map[string]*stepCtx
	inputs *expr.Dictionary // nil: the job's inputs context
}

func newScope() *stepsScope { return &stepsScope{steps: map[string]*stepCtx{}} }

// PushScope makes the steps context called name (created empty on first use)
// the current one, with inputs as the inputs context, and returns the function
// that restores the previous one. A composite action's pre, main and post
// stages share one scope (the same name), as the runner's do.
func (s *State) PushScope(name string, inputs *expr.Dictionary) (restore func()) {
	prev := s.cur
	sc, ok := s.scopes[name]
	if name == "" {
		sc, ok = s.root, true
	}
	if !ok {
		sc = newScope()
		s.scopes[name] = sc
	}
	if inputs != nil {
		sc.inputs = inputs
	}
	s.cur = sc
	return func() { s.cur = prev }
}

// StepsValue returns the steps context in use (outputs of a composite action
// are evaluated against it).
func (s *State) StepsValue() *expr.Dictionary { return s.stepsValue() }

// GitHubKeys saves the current value of the given keys of the github context,
// to put them back with the returned function: a key that was absent is
// removed again. Composite actions' steps get their own copy of the context in
// the runner (a shallow copy, CompositeActionHandler.cs:152-161); the
// executor keeps one and restores it.
func (s *State) GitHubKeys(keys ...string) (restore func()) {
	type saved struct {
		key string
		val expr.Value
		has bool
	}
	var old []saved
	for _, k := range keys {
		v, has := s.github.Get(k)
		old = append(old, saved{k, v, has})
	}
	return func() {
		for _, o := range old {
			if o.has {
				s.github = setKey(s.github, o.key, o.val)
			} else {
				s.github = removeKey(s.github, o.key)
			}
		}
	}
}

// removeKey returns d without key (compared case-insensitively).
func removeKey(d *expr.Dictionary, key string) *expr.Dictionary {
	out := expr.NewDictionary()
	for _, p := range d.Pairs() {
		if !strings.EqualFold(p.Key, key) {
			out.Add(p.Key, p.Value)
		}
	}
	return out
}
