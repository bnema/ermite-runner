// SPDX-License-Identifier: MIT

package executor

// SaveState keeps a value for the later stages of the action of the step: it
// becomes STATE_<name> in its environment (ActionRunner.cs:225-229). The runner
// keeps it per action: for a step of the job in the intra-action state of the
// step's contexts, for a step inside a composite action at the root, by the
// id of the step (FileCommandManager.cs:305-318, ActionCommandManager.cs:395-
// 405). Here both are the state of the step's node, which its pre, main and post
// stages share. Names compare without case.
//
// This is the integration point with the workflow commands: the `save-state`
// command and the GITHUB_STATE file both end here.
func (s *stepRun) SaveState(name, value string) {
	if s.n == nil {
		return
	}
	s.n.setState(name, value)
}
