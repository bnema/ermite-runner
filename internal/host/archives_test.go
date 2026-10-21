// SPDX-License-Identifier: MIT

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/actionscache"
	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

type archiveRig struct {
	r       *Runner
	srv     *httptest.Server
	hits    atomic.Int32
	status  atomic.Int32
	archive *actionscache.Cache
}

func newArchiveRig(t *testing.T) *archiveRig {
	t.Helper()
	a := &archiveRig{}
	a.status.Store(http.StatusOK)
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		if s := int(a.status.Load()); s != http.StatusOK {
			http.Error(w, "slow down", s)
			return
		}
		_, _ = w.Write([]byte("tarball of " + r.URL.Path))
	}))
	t.Cleanup(a.srv.Close)
	ac, err := actionscache.New(actionscache.Config{Dir: filepath.Join(t.TempDir(), "a"), Max: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(t.TempDir(), "c"), Archives: ac})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseAll() })
	a.archive = ac
	a.r = &Runner{Cache: svc, Archives: ac}
	return a
}

func (a *archiveRig) job(id string) *jobRun {
	j := &jobRun{r: a.r, log: zerowrap.Logger{}, ctx: context.Background(),
		job:      &ermite.Job{JobId: id, Ref: "refs/heads/main"},
		contexts: `{"github":{"repository":"o/r"}}`}
	return j
}

func (a *archiveRig) action(sha string) (*ermite.ResolvedAction, *execpb.ResolvedAction) {
	o := &ermite.ResolvedAction{Ref: "x/y@v1", NameWithOwner: "x/y", ResolvedSha: sha, TarballUrl: a.srv.URL + "/x/y/tarball/" + sha, Token: "ignored-for-github"}
	c := &execpb.ResolvedAction{Ref: o.Ref, NameWithOwner: "x/y", ResolvedSha: sha, TarballUrl: o.TarballUrl, Token: "t", AuthOrigin: "o"}
	return o, c
}

// Two jobs resolving the same commit cost one download, and the sandbox is
// pointed at the cache with no credentials.
func TestCacheArchivesSharedAcrossJobs(t *testing.T) {
	a := newArchiveRig(t)
	sha := strings.Repeat("a", 40)
	for _, id := range []string{"j1", "j2"} {
		o, c := a.action(sha)
		a.job(id).cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
		if !strings.HasPrefix(c.TarballUrl, "http://127.0.0.1:9089"+cachesvc.ArchivePath) || c.Token != "" || c.AuthOrigin != "" {
			t.Fatalf("%s: not redirected to the cache: %+v", id, c)
		}
		if !strings.Contains(c.TarballUrl, "job="+id) {
			t.Errorf("%s: the URL is not bound to the job: %s", id, c.TarballUrl)
		}
	}
	if a.hits.Load() != 1 {
		t.Fatalf("%d downloads for one commit", a.hits.Load())
	}
}

// A moved tag resolves to a new sha, hence a new entry: the ref is never a key.
func TestCacheArchivesKeyedBySha(t *testing.T) {
	a := newArchiveRig(t)
	for _, sha := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		o, c := a.action(sha)
		a.job("j").cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
	}
	if a.hits.Load() != 2 {
		t.Fatalf("%d downloads for two commits", a.hits.Load())
	}
}

// Whatever cannot be cached keeps its original URL and token, so the sandbox
// downloads it with its own retries and messages.
func TestCacheArchivesFallsBack(t *testing.T) {
	a := newArchiveRig(t)
	a.status.Store(http.StatusTooManyRequests)
	o, c := a.action(strings.Repeat("a", 40))
	origURL := c.TarballUrl
	a.job("j").cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
	if c.TarballUrl != origURL || c.Token != "t" || c.AuthOrigin != "o" {
		t.Errorf("429: %+v", c)
	}

	a.status.Store(http.StatusOK)
	for name, mutate := range map[string]func(*ermite.ResolvedAction){
		"a ref instead of a sha": func(o *ermite.ResolvedAction) { o.ResolvedSha = "v1" },
		"no sha":                 func(o *ermite.ResolvedAction) { o.ResolvedSha = "" },
		"an error":               func(o *ermite.ResolvedAction) { o.Error = "boom" },
		"no url":                 func(o *ermite.ResolvedAction) { o.TarballUrl = "" },
	} {
		o, c := a.action(strings.Repeat("c", 40))
		mutate(o)
		before := a.hits.Load()
		a.job("j").cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
		if c.Token != "t" || strings.Contains(c.TarballUrl, cachesvc.ArchivePath) || a.hits.Load() != before {
			t.Errorf("%s: cached anyway: %+v", name, c)
		}
	}

	// No cache channel for the job (no repository): the guest has no relay.
	o, c = a.action(strings.Repeat("d", 40))
	j := a.job("j")
	j.contexts = `{}`
	j.cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
	if c.Token != "t" || strings.Contains(c.TarballUrl, cachesvc.ArchivePath) {
		t.Errorf("job without a cache scope: %+v", c)
	}

	// Cache off.
	a.r.Archives = nil
	o, c = a.action(strings.Repeat("e", 40))
	a.job("j").cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
	if c.Token != "t" {
		t.Errorf("cache off: %+v", c)
	}
}

func TestArchiveTimingLogs(t *testing.T) {
	a := newArchiveRig(t)
	var logs bytes.Buffer
	j := a.job("timing")
	j.log = zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &logs})
	for i := 0; i < 2; i++ {
		logs.Reset()
		o, c := a.action(strings.Repeat("a", 40))
		j.cacheArchives(context.Background(), []*ermite.ResolvedAction{o}, []*execpb.ResolvedAction{c})
		var fields map[string]any
		if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
			t.Fatal(err, logs.String())
		}
		if value, ok := fields["verify_ms"].(float64); !ok || value < 0 {
			t.Fatal(fields)
		}
		if fields["hit"] != (i == 1) {
			t.Fatal(fields)
		}
		if i == 0 {
			if value, ok := fields["fetch_ms"].(float64); !ok || value < 0 {
				t.Fatal(fields)
			}
			if value, ok := fields["bytes"].(float64); !ok || value <= 0 {
				t.Fatal(fields)
			}
		} else if _, ok := fields["fetch_ms"]; ok {
			t.Fatal("hit has fetch_ms", fields)
		}
	}
	if j.timeline["archive_misses"] != 1 || j.timeline["archive_miss_bytes"] <= 0 {
		t.Fatal(j.timeline)
	}
}
