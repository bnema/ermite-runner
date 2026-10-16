// SPDX-License-Identifier: MIT

package cachesvc

import (
	"crypto/hmac"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"bnema.dev/ermite-runner/internal/actionscache"
)

// ArchivePath is the prefix of the action archive route: the host downloads the
// archives of actions into its own cache (package actionscache) and the sandbox
// reads them here instead of from the forge. It is not part of the toolkit's
// protocol.
const ArchivePath = "/_ermite/action-archives/"

// ArchiveURL is the URL, as the guest sees it (its loopback, DefaultPort), from
// which the job can read the archive of key for ttl. Like the toolkit's archive
// URLs the signature is the credential: it is bound to the key, the job and the
// expiry, so a URL that left its sandbox is useless on any other channel, and a
// job can read only the archives the host resolved for it. It is "" when the
// service holds no archive cache.
func (s *Service) ArchiveURL(job, key string, ttl time.Duration) string {
	if s.archives == nil || !actionscache.ValidKey(key) {
		return ""
	}
	exp := s.now().Add(ttl).Unix()
	q := url.Values{"job": {job}, "exp": {strconv.FormatInt(exp, 10)}, "sig": {b64.EncodeToString(s.sign.mac("action-archive", []byte(key), []byte(job), []byte(strconv.FormatInt(exp, 10))))}}
	return "http://127.0.0.1:" + strconv.Itoa(DefaultPort) + ArchivePath + key + "?" + q.Encode()
}

func (s *Service) archiveOK(key, job string, exp int64, sig string) bool {
	got, err := b64.DecodeString(sig)
	if err != nil || !s.now().Before(time.Unix(exp, 0)) {
		return false
	}
	return hmac.Equal(got, s.sign.mac("action-archive", []byte(key), []byte(job), []byte(strconv.FormatInt(exp, 10))))
}

func (s *Service) serveActionArchive(channelJob string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		q := r.URL.Query()
		exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
		job := q.Get("job")
		if s.archives == nil || err != nil || job == "" || (channelJob != "" && job != channelJob) || !actionscache.ValidKey(key) || !s.archiveOK(key, job, exp, q.Get("sig")) {
			reply(w, http.StatusForbidden, "forbidden")
			return
		}
		f, err := s.archives.Open(key)
		if err != nil {
			if !errors.Is(err, actionscache.ErrNotFound) {
				s.log.Warn().Err(err).Msg("action archive")
			}
			reply(w, http.StatusNotFound, "no such archive")
			return
		}
		defer func() { _ = f.Close() }()
		rc := http.NewResponseController(w)
		remaining := s.remaining(exp)
		defer func() { _ = rc.SetWriteDeadline(deadline(s.progress, remaining)) }()
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeContent(&progressWriter{ResponseWriter: w, rc: rc, idle: s.progress, remaining: remaining}, r, "", time.Time{}, f)
	}
}
