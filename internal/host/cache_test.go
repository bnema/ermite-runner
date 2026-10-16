// SPDX-License-Identifier: MIT

package host

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/resultssvc"
)

const scopeCtx = `{"github":{"repository":"o/r"}}`

func TestCacheScopeFromJobCache(t *testing.T) {
	main, pr := "refs/heads/main", "refs/pull/7/merge"
	for name, tc := range map[string]struct {
		jc    *ermite.JobCache
		write string
		reads []string
	}{
		"pull request": {&ermite.JobCache{Repository: "o/r", WriteRef: pr, ReadRefs: []string{pr, "refs/heads/dev", main}}, pr, []string{pr, "refs/heads/dev", main}},
		// Job.cache documents the repository as its name.
		"name only":       {&ermite.JobCache{Repository: "r", WriteRef: main, ReadRefs: []string{main}}, main, []string{main}},
		"read-only":       {&ermite.JobCache{Repository: "o/r", ReadRefs: []string{main}}, "", []string{main}},
		"empties dropped": {&ermite.JobCache{Repository: "o/r", WriteRef: main, ReadRefs: []string{"", main, ""}}, main, []string{main}},
		"write-only":      {&ermite.JobCache{Repository: "o/r", WriteRef: main}, main, nil},
	} {
		// The server's scope wins over Job.Ref.
		sc, ok, err := cacheScope(&ermite.Job{Ref: "refs/heads/other", Cache: tc.jc}, scopeCtx)
		if err != nil || !ok || sc.Repository != "o/r" || sc.WriteRef != tc.write || strings.Join(sc.ReadRefs, ",") != strings.Join(tc.reads, ",") {
			t.Errorf("%s: %v %v %+v", name, ok, err, sc)
		}
	}
	// Nothing allowed: no cache, no error.
	if _, ok, err := cacheScope(&ermite.Job{Cache: &ermite.JobCache{Repository: "o/r", ReadRefs: []string{""}}}, scopeCtx); ok || err != nil {
		t.Errorf("empty scope: %v %v", ok, err)
	}
}

// A name alone and owner/name are the same repository, and the scope is always
// owner/name: two repositories with the same name under different owners never
// share entries.
func TestCacheScopeCanonicalRepository(t *testing.T) {
	ref := "refs/heads/main"
	ctxA, ctxB := `{"github":{"repository":"a/r"}}`, `{"github":{"repository":"b/r"}}`
	var got []string
	for _, tc := range []struct{ repo, ctx string }{{"r", ctxA}, {"a/r", ctxA}, {"r", ctxB}, {"b/r", ctxB}} {
		sc, ok, err := cacheScope(&ermite.Job{Cache: &ermite.JobCache{Repository: tc.repo, WriteRef: ref, ReadRefs: []string{ref}}}, tc.ctx)
		if err != nil || !ok {
			t.Fatalf("%+v: %v %v", tc, ok, err)
		}
		got = append(got, sc.Repository)
	}
	if strings.Join(got, ",") != "a/r,a/r,b/r,b/r" {
		t.Fatalf("scopes %v", got)
	}
	// A spelling that names the other owner is refused.
	if _, ok, err := cacheScope(&ermite.Job{Cache: &ermite.JobCache{Repository: "b/r", WriteRef: ref}}, ctxA); ok || err == nil {
		t.Fatal("b/r accepted for a/r")
	}
}

func TestCacheScopeInvalid(t *testing.T) {
	main := "refs/heads/main"
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "refs/heads/b" + string(rune('0'+i))
	}
	for name, tc := range map[string]struct {
		jc  *ermite.JobCache
		ctx string
	}{
		"other repository": {&ermite.JobCache{Repository: "o/other", WriteRef: main}, scopeCtx},
		"other owner":      {&ermite.JobCache{Repository: "x/r", WriteRef: main}, scopeCtx},
		"empty repository": {&ermite.JobCache{WriteRef: main}, scopeCtx},
		"job without repo": {&ermite.JobCache{Repository: "o/r", WriteRef: main}, `{}`},
		"short write ref":  {&ermite.JobCache{Repository: "o/r", WriteRef: "main"}, scopeCtx},
		"short read ref":   {&ermite.JobCache{Repository: "o/r", WriteRef: main, ReadRefs: []string{main, "dev"}}, scopeCtx},
		"bare refs/":       {&ermite.JobCache{Repository: "o/r", ReadRefs: []string{"refs/"}}, scopeCtx},
		"space in ref":     {&ermite.JobCache{Repository: "o/r", ReadRefs: []string{"refs/heads/a b"}}, scopeCtx},
		"nine read refs":   {&ermite.JobCache{Repository: "o/r", ReadRefs: nine}, scopeCtx},
	} {
		if sc, ok, err := cacheScope(&ermite.Job{Ref: main, Cache: tc.jc}, tc.ctx); ok || err == nil {
			t.Errorf("%s: accepted %+v", name, sc)
		}
	}
	// Eight are fine.
	if _, ok, err := cacheScope(&ermite.Job{Cache: &ermite.JobCache{Repository: "o/r", ReadRefs: nine[:8]}}, scopeCtx); !ok || err != nil {
		t.Errorf("eight read refs: %v %v", ok, err)
	}
}

// An old server sends no Job.cache: the repository and Job.ref, both ways.
func TestCacheScopeFallback(t *testing.T) {
	sc, ok, err := cacheScope(&ermite.Job{Ref: "refs/heads/main"}, scopeCtx)
	if err != nil || !ok || sc.Repository != "o/r" || sc.WriteRef != "refs/heads/main" || len(sc.ReadRefs) != 1 || sc.ReadRefs[0] != "refs/heads/main" {
		t.Fatalf("%v %v %+v", ok, err, sc)
	}
	// No repository or no ref: no cache, rather than a scope that could be
	// shared by unrelated jobs.
	for name, tc := range map[string]struct {
		job *ermite.Job
		ctx string
	}{
		"no ref":        {&ermite.Job{}, scopeCtx},
		"no repository": {&ermite.Job{Ref: "refs/heads/main"}, `{"github":{}}`},
		"bad contexts":  {&ermite.Job{Ref: "refs/heads/main"}, `nope`},
		"no github":     {&ermite.Job{Ref: "refs/heads/main"}, `{}`},
	} {
		if _, ok, err := cacheScope(tc.job, tc.ctx); ok || err != nil {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
}

// A mismatched scope disables the cache of the job, with a log line; the job
// goes on.
func TestRuntimeForMismatchDisablesCache(t *testing.T) {
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	var logs bytes.Buffer
	j := &jobRun{
		job:      &ermite.Job{JobId: "j1", Ref: "refs/heads/main", Cache: &ermite.JobCache{Repository: "o/other", WriteRef: "refs/heads/main"}},
		contexts: scopeCtx, log: zerowrap.New(zerowrap.Config{Output: &logs, Format: "json", Level: "debug"}),
	}
	if port, tok, _, _ := j.runtimeFor(&Runner{Cache: svc}); port != 0 || tok != "" {
		t.Fatalf("cache for a mismatched scope: %d %q", port, tok)
	}
	if !strings.Contains(logs.String(), "cache disabled for the job") || !strings.Contains(logs.String(), "o/other") {
		t.Fatalf("log: %q", logs.String())
	}
}

func TestRuntimeForNeedsServiceAndScope(t *testing.T) {
	j := &jobRun{job: &ermite.Job{JobId: "j1", Ref: "refs/heads/main", TimeoutMinutes: 5}, contexts: `{"github":{"repository":"o/r"}}`}
	if port, tok, _, _ := j.runtimeFor(&Runner{}); port != 0 || tok != "" {
		t.Fatalf("no service: %d %q", port, tok)
	}
}

// The token of a job is the one the service accepts, for the job's repository
// and ref only.
func TestRuntimeForToken(t *testing.T) {
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	rt := &Runner{Cache: svc}
	j := &jobRun{job: &ermite.Job{JobId: "j1", Ref: "refs/heads/main", TimeoutMinutes: 5}, contexts: `{"github":{"repository":"o/r"}}`}
	port, tok, mode, results := j.runtimeFor(rt)
	// The port is the cache's loopback port in the guest, not a host port.
	if port != 9089 || tok == "" || results {
		t.Fatalf("%d %q %v", port, tok, results)
	}
	// No Job.cache: the narrowest scope writes the job's own ref.
	if mode != "write" {
		t.Fatalf("mode %q", mode)
	}
	do := func(token string) int {
		req, _ := http.NewRequest("POST", srv.URL+cachesvc.APIPath+"caches", strings.NewReader(`{"key":"k","version":"v","cacheSize":1}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := do(tok); c != http.StatusCreated {
		t.Fatalf("reserve with the job's token: %d", c)
	}
	// The job token of ermite is not a cache token, nor the other way round.
	if c := do("ermitejob_0123456789"); c != http.StatusForbidden {
		t.Fatalf("foreign token: %d", c)
	}
}

// ACTIONS_CACHE_MODE follows the write ref of the scope: none makes the job
// read-only.
func TestRuntimeForMode(t *testing.T) {
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	for _, tc := range []struct{ write, want string }{{"refs/heads/main", "write"}, {"", "read"}} {
		j := &jobRun{
			job:      &ermite.Job{JobId: "j1", TimeoutMinutes: 5, Cache: &ermite.JobCache{Repository: "o/r", WriteRef: tc.write, ReadRefs: []string{"refs/heads/main"}}},
			contexts: `{"github":{"repository":"o/r"}}`,
		}
		if _, _, mode, _ := j.runtimeFor(&Runner{Cache: svc}); mode != tc.want {
			t.Errorf("write ref %q: mode %q, want %q", tc.write, mode, tc.want)
		}
	}
}

// A job with the results service gets the channel even without a cache scope:
// its token carries the job's Actions.Results scope, the results service takes
// it and the cache refuses it. With a cache scope the token serves both.
func TestRuntimeForResults(t *testing.T) {
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	rt := &Runner{Cache: svc, Results: resultssvc.New(resultssvc.Config{Auth: svc}),
		Artifacts: func(string) ermiteconnect.JobArtifactServiceClient { return nil }}
	scope := resultssvc.Scope(42, "j1", 2)
	for _, tc := range []struct {
		name, ref string
		mode      string
	}{{"no cache scope", "", ""}, {"cache scope", "refs/heads/main", "write"}} {
		j := &jobRun{job: &ermite.Job{JobId: "j1", RunId: 42, Attempt: 2, Ref: tc.ref, TimeoutMinutes: 5},
			contexts: `{"github":{"repository":"o/r"}}`, features: jobFeatures{artifacts: true}}
		j.results = j.resultsOn(rt)
		port, tok, mode, results := j.runtimeFor(rt)
		if port != cachesvc.DefaultPort || tok == "" || mode != tc.mode || !results {
			t.Fatalf("%s: %d %q %q %v", tc.name, port, tok, mode, results)
		}
		job, scp, _, err := svc.RuntimeToken(tok)
		if err != nil || job != "j1" || !strings.Contains(" "+scp+" ", " "+scope+" ") {
			t.Fatalf("%s: %q %q %v", tc.name, job, scp, err)
		}
		if j.resultsHandler(rt) == nil {
			t.Fatalf("%s: no results handler", tc.name)
		}
	}
}

// The results service needs the runner's service, its cache channel, the
// client factory and the server's ci.artifacts; without any of them the job
// gets none, and its token no Actions.Results scope.
func TestResultsOnNeedsEverything(t *testing.T) {
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	res := resultssvc.New(resultssvc.Config{Auth: svc})
	art := func(string) ermiteconnect.JobArtifactServiceClient { return nil }
	full := &Runner{Cache: svc, Results: res, Artifacts: art}
	for name, tc := range map[string]struct {
		rt   *Runner
		feat bool
	}{
		"no feature": {full, false},
		"no service": {&Runner{Cache: svc, Artifacts: art}, true},
		"no cache":   {&Runner{Results: res, Artifacts: art}, true},
		"no client":  {&Runner{Cache: svc, Results: res}, true},
	} {
		j := &jobRun{job: &ermite.Job{JobId: "j1", RunId: 42, Ref: "refs/heads/main", TimeoutMinutes: 5},
			contexts: `{"github":{"repository":"o/r"}}`, features: jobFeatures{artifacts: tc.feat}}
		j.results = j.resultsOn(tc.rt)
		if j.results || j.resultsHandler(tc.rt) != nil {
			t.Errorf("%s: results on", name)
		}
		_, tok, _, results := j.runtimeFor(tc.rt)
		if results {
			t.Errorf("%s: results in the job spec", name)
		}
		if _, scp, _, err := svc.RuntimeToken(tok); err == nil && strings.Contains(scp, "Actions.Results") {
			t.Errorf("%s: results scope in the token", name)
		}
	}
	j := &jobRun{features: jobFeatures{artifacts: true}}
	if !j.resultsOn(full) {
		t.Fatal("everything there: results off")
	}
}
