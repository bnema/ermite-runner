// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"sync"
)

// extraSecrets are the values the job registered in the masker after it
// started: the archive tokens of the actions (ActionManager.cs:1130,1650).
// Phase G's add-mask feeds the same masker; whoever merges second keeps both.
var extraSecretsMu sync.Mutex

// addSecrets registers values with the masker, and the base64 of
// "x-access-token:<value>" for tokens (ActionManager.CreateAuthHeader).
func (j *job) addSecrets(values ...string) {
	extraSecretsMu.Lock()
	defer extraSecretsMu.Unlock()
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
