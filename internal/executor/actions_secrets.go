// SPDX-License-Identifier: MIT

package executor

import "encoding/base64"

// extraSecrets (job.extraSecrets) are the values registered in the masker after
// the job started: the archive tokens of the actions (ActionManager.cs:1130,
// 1650). Integration point with the add-mask command of phase G, which feeds
// the same masker: it should append to j.extraSecrets under j.mu and rebuild
// the masker like addSecrets does.

// addSecrets registers values with the masker, and the base64 of
// "x-access-token:<value>" for tokens (ActionManager.CreateAuthHeader).
func (j *job) addSecrets(values ...string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.extraSecrets = append(j.extraSecrets, values...)
	all := append(j.state.Secrets(), j.extraSecrets...)
	j.masker.Store(newMasker(all...))
}

// addTokenSecret registers an archive token and its Basic form.
func (j *job) addTokenSecret(token string) {
	if token == "" {
		return
	}
	j.addSecrets(token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
}
