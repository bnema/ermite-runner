// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/actionscache"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

const (
	// archiveFetchTimeout and maxArchiveBytes are the executor's limits for an
	// archive (actions_download.go).
	archiveFetchTimeout = 20 * time.Minute
	maxArchiveBytes     = 1 << 30
)

// cacheArchives points the actions of a resolution at the host's archive cache.
// The host downloads each archive once per (forge, repository, resolved sha) and
// the sandbox reads it from the cache channel instead of the forge, so that a
// busy runner does not trip the forge's rate limit (codeload 429).
//
// It only ever improves a resolution: an action that cannot be cached (no cache,
// no channel for this job, a ref that did not resolve to a sha, a download that
// failed, a full disk) keeps its original URL and token, and the sandbox
// downloads it as before, with its retries and its messages. The sandbox prints
// "Download action repository" in either case, so a hit reads like a miss in the
// job's log; the hit or miss is logged here.
func (j *jobRun) cacheArchives(ctx context.Context, orig []*ermite.ResolvedAction, conv []*execpb.ResolvedAction) {
	rt := j.r
	if rt == nil || rt.Archives == nil || rt.Cache == nil || len(orig) != len(conv) {
		return
	}
	if _, ok, err := cacheScope(j.job, j.contexts); err != nil || !ok {
		return // no channel for this job: the guest has no relay either
	}
	ttl := time.Duration(j.timeoutMinutes())*time.Minute + cacheTokenMargin
	for i, a := range orig {
		c := conv[i]
		if c.Error != "" || a.Error != "" || a.TarballUrl == "" {
			continue
		}
		origin := originOf(a.TarballUrl)
		key, ok := actionscache.Key(origin, a.NameWithOwner, a.ResolvedSha)
		if !ok {
			continue
		}
		hit, err := rt.Archives.Ensure(ctx, key, func(ctx context.Context, w io.Writer) error {
			return j.fetchArchive(ctx, a, origin, w)
		})
		log := j.log.With().Str("action", a.NameWithOwner).Str("sha", a.ResolvedSha).Logger()
		if err != nil {
			log.Warn().Err(err).Msg("action archive not cached; the sandbox downloads it")
			continue
		}
		u := rt.Cache.ArchiveURL(j.job.JobId, key, ttl)
		if u == "" {
			continue
		}
		log.Info().Bool("hit", hit).Msg("action archive cache")
		c.TarballUrl, c.Token, c.AuthOrigin = u, "", ""
	}
}

// fetchArchive downloads the archive of a from the forge, with the token only
// when the server gave one for an origin that is ermite (like resolveResponse).
func (j *jobRun) fetchArchive(ctx context.Context, a *ermite.ResolvedAction, origin string, w io.Writer) error {
	token := ""
	if a.Source == ermite.ActionSource_ACTION_SOURCE_ERMITE && a.Token != "" && (origin == j.publicOrigin || origin == j.serverOrigin) {
		token = a.Token
	}
	ctx, cancel := context.WithTimeout(ctx, archiveFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.TarballUrl, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	}
	req.Header.Set("User-Agent", "ermite-runner")
	resp, err := actionscache.NewClient(j.r.ArchiveHTTP).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err == nil && n > maxArchiveBytes {
		err = errors.New("the archive is larger than 1 GiB")
	}
	return err
}
