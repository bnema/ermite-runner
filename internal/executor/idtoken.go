// SPDX-License-Identifier: MIT

package executor

// addIDTokenEnv is the OIDC part of the handlers (ScriptHandler.cs:315-319,
// NodeScriptActionHandler.cs:66-70, ContainerActionHandler.cs:232-236):
// ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN, which
// @actions/core's getIDToken reads, for run steps, node actions and docker
// actions. A composite action sets nothing itself: its steps get them from
// their own handlers. Nothing is set unless the job has both (the server gives
// them only to a trusted job with "id-token: write").
func (j *job) addIDTokenEnv(env *ordered) {
	if j.cfg.IDTokenURL == "" || j.cfg.IDTokenRequestToken == "" {
		return
	}
	env.Set("ACTIONS_ID_TOKEN_REQUEST_URL", j.cfg.IDTokenURL)
	env.Set("ACTIONS_ID_TOKEN_REQUEST_TOKEN", j.cfg.IDTokenRequestToken)
}

// scriptEnv returns the environment of the process of a run step: the env of
// the step, plus the variables its handler adds (addIDTokenEnv). The env of
// the step itself, which the env context and the header show, is left alone.
func (j *job) scriptEnv(s *stepRun) *ordered {
	env := s.env.Clone()
	j.addIDTokenEnv(env)
	return env
}
