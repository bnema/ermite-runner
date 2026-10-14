// SPDX-License-Identifier: MIT

package executor

import "encoding/base64"

// addTokenMask registers an archive token with the masker of the job
// (ActionManager.cs:1130 adds the token, :1650 the base64 of
// "x-access-token:<token>" it sends as the Basic credential). job.addMask only
// adds: the masker is never replaced.
func (j *job) addTokenMask(token string) {
	if token == "" {
		return
	}
	j.addMask(token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
}
