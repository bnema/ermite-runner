// SPDX-License-Identifier: MIT

package host

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/cachesvc"
)

// cacheTokenMargin is added to the job timeout for the life of the cache token:
// the archive URLs of a lookup expire with it.
const cacheTokenMargin = 15 * time.Minute

// maxCacheReadRefs bounds Job.cache.read_refs (own ref, base branch, default
// branch, with room to spare).
const maxCacheReadRefs = 8

// cacheScope is what the job may do in the cache of the runner.
//
// The server says (Job.cache, feature "ci.cache"): the repository, the one ref
// the job may write and the refs it may read, most specific first. It is used
// as given once checked: the repository must be the job's own (the
// "owner/name" of the github context, or its name alone, which is how
// Job.cache documents it) and the scope always carries the canonical
// "owner/name", a ref must be a full "refs/..." name, empty refs
// are dropped, and there are at most maxCacheReadRefs read refs. An empty
// write_ref makes the job read-only. A scope that fails the checks is an error:
// the job runs without a cache.
//
// A server that sends no Job.cache (an old server) gets the narrowest scope the
// job's own fields give: the repository of the github context and the ref that
// triggered the run, for reading and writing, with no fallback to the base or
// default branch. A job without a repository or a ref has no cache (ok false,
// no error).
func cacheScope(job *ermite.Job, contexts string) (sc cachesvc.Scope, ok bool, err error) {
	repo := githubRepository(contexts)
	jc := job.GetCache()
	if jc == nil {
		if repo == "" || job.Ref == "" {
			return cachesvc.Scope{}, false, nil
		}
		return cachesvc.Scope{Repository: repo, WriteRef: job.Ref, ReadRefs: []string{job.Ref}}, true, nil
	}
	name := repo[strings.LastIndex(repo, "/")+1:]
	if jc.Repository == "" || repo == "" || (jc.Repository != repo && jc.Repository != name) {
		return cachesvc.Scope{}, false, fmt.Errorf("the cache repository %q is not the job's repository %q", jc.Repository, repo)
	}
	// Always the job's canonical "owner/name", whichever way the server spelled
	// it: entries of two repositories with the same name under different owners
	// must never share a scope.
	sc.Repository = repo
	if w := jc.WriteRef; w != "" {
		if !fullRef(w) {
			return cachesvc.Scope{}, false, fmt.Errorf("the cache write ref %q is not a full ref name", w)
		}
		sc.WriteRef = w
	}
	for _, r := range jc.ReadRefs {
		if r == "" {
			continue
		}
		if !fullRef(r) {
			return cachesvc.Scope{}, false, fmt.Errorf("the cache read ref %q is not a full ref name", r)
		}
		sc.ReadRefs = append(sc.ReadRefs, r)
	}
	if len(sc.ReadRefs) > maxCacheReadRefs {
		return cachesvc.Scope{}, false, fmt.Errorf("%d cache read refs, at most %d", len(sc.ReadRefs), maxCacheReadRefs)
	}
	if sc.WriteRef == "" && len(sc.ReadRefs) == 0 {
		return cachesvc.Scope{}, false, nil // allowed to do nothing
	}
	return sc, true, nil
}

// fullRef reports whether s is a full ref name: "refs/" and something after it,
// without control characters or spaces.
func fullRef(s string) bool {
	if !strings.HasPrefix(s, "refs/") || len(s) == len("refs/") || len(s) > 1024 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// githubRepository is github.repository ("owner/name") of the contexts JSON.
func githubRepository(contexts string) string {
	var c struct {
		Github struct {
			Repository string `json:"repository"`
		} `json:"github"`
	}
	if json.Unmarshal([]byte(contexts), &c) != nil {
		return ""
	}
	return c.Github.Repository
}

// cacheFor returns the cache port and the token of a job, or 0 and "" when the
// runner serves no cache or the job has no scope.
func (j *jobRun) cacheFor(rt *Runner) (port uint32, token string) {
	if rt.Cache == nil || rt.CachePort == 0 {
		return 0, ""
	}
	sc, ok, err := cacheScope(j.job, j.contexts)
	if err != nil {
		// A bad scope from the server is not the job's failure: it runs without a
		// cache.
		j.log.Warn().Err(err).Msg("cache disabled for the job")
		return 0, ""
	}
	if !ok {
		return 0, ""
	}
	ttl := time.Duration(j.timeoutMinutes())*time.Minute + cacheTokenMargin
	return uint32(rt.CachePort), rt.Cache.Token(j.job.JobId, sc, ttl)
}
