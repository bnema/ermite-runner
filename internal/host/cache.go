// SPDX-License-Identifier: MIT

package host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/resultssvc"
)

// cacheTokenMargin is added to the job timeout for the life of the cache token:
// the archive URLs of a lookup expire with it.
const cacheTokenMargin = 15 * time.Minute

// depsCacheRef is the only cache ref of a dependency job: its module cache
// never shares entries with the caches of workflow jobs, either way.
const depsCacheRef = "refs/ermite/deps"

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
// Deps jobs without Job.cache have no cache; they never use the legacy fallback.
// A workflow on a server that sends no Job.cache gets the narrowest scope the
// job's own fields give: the repository of the github context and the ref that
// triggered the run, for reading and writing, with no fallback to the base or
// default branch. A job without a repository or a ref has no cache (ok false,
// no error).
func cacheScope(job *ermite.Job, contexts string) (sc cachesvc.Scope, ok bool, err error) {
	repo := githubRepository(contexts)
	jc := job.GetCache()
	if jc == nil {
		if job.Deps != nil {
			return cachesvc.Scope{}, false, nil
		}
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
		if job.Deps != nil && w != depsCacheRef {
			return cachesvc.Scope{}, false, fmt.Errorf("the deps cache write ref %q is not %s", w, depsCacheRef)
		}
		if !fullRef(w) {
			return cachesvc.Scope{}, false, fmt.Errorf("the cache write ref %q is not a full ref name", w)
		}
		sc.WriteRef = w
	}
	for _, r := range jc.ReadRefs {
		if r == "" {
			continue
		}
		if job.Deps != nil && r != depsCacheRef {
			return cachesvc.Scope{}, false, fmt.Errorf("the deps cache read ref %q is not %s", r, depsCacheRef)
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

// runtimeFor returns what the executor needs of the job's runtime channel: the
// port it has in the guest, the job's ACTIONS_RUNTIME_TOKEN, the cache mode and
// whether the channel serves the results service. The channel exists when the
// runner serves a cache and the job has a cache scope, the results service,
// or the cache serves a registry proxy. Without it, 0, "", "" and false.
// A job with the results service and no cache scope gets an empty mode and a
// token that the cache refuses. A registry-only job (neither) gets the port
// alone: no token, and so no runtime environment variables for its steps.
func (j *jobRun) runtimeFor(rt *Runner) (port uint32, token, mode string, results bool) {
	if rt.Cache == nil {
		return 0, "", "", false
	}
	sc, ok, err := cacheScope(j.job, j.contexts)
	if err != nil {
		// A bad scope from the server is not the job's failure: it runs without a
		// cache.
		j.log.Warn().Err(err).Msg("cache disabled for the job")
		ok = false
	}
	if !ok {
		sc = cachesvc.Scope{}
	} else {
		mode = "read"
		if sc.WriteRef != "" {
			mode = "write"
		}
	}
	if !ok && !j.results {
		if rt.Cache.RegistryEnabled() {
			return cachesvc.DefaultPort, "", "", false
		}
		return 0, "", "", false
	}
	var scopes []string
	if j.results {
		scopes = append(scopes, resultssvc.Scope(j.job.RunId, j.job.JobId, j.job.Attempt))
	}
	ttl := time.Duration(j.timeoutMinutes())*time.Minute + cacheTokenMargin
	return cachesvc.DefaultPort, rt.Cache.Token(j.job.JobId, sc, ttl, scopes...), mode, j.results
}

// resultsOn says whether the job gets the results service: the runner has one
// (ERMITE_RUNNER_ARTIFACTS is not 0) with a cache channel to serve it on, and
// the server has the feature ci.artifacts. Without it artifact actions fail as
// on a GitHub Enterprise Server without artifacts v4.
func (j *jobRun) resultsOn(rt *Runner) bool {
	return rt.Results != nil && rt.Cache != nil && rt.Artifacts != nil && j.features.artifacts
}

// resultsHandler is the results service of the job's channel, nil without one.
// It calls ermite with the job token.
func (j *jobRun) resultsHandler(rt *Runner) http.Handler {
	if !j.results {
		return nil
	}
	return rt.Results.Handler(resultssvc.Job{ID: j.job.JobId, RunID: j.job.RunId, Attempt: j.job.Attempt, Client: rt.Artifacts(j.job.JobToken)})
}
