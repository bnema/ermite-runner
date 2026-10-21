// SPDX-License-Identifier: MIT

package cachesvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	t   *testing.T
	svc *Service
	srv *httptest.Server
	clk *clock
	dir string
}

func newRig(t *testing.T, max int64) *rig {
	t.Helper()
	return newRigWith(t, Config{Max: max, MinCharge: 1})
}

// newRigWith starts a service with cfg on top of the test clock and directory.
func newRigWith(t *testing.T, cfg Config) *rig {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	dir := filepath.Join(t.TempDir(), "cache")
	cfg.Dir, cfg.Now, cfg.ReservationTTL = dir, clk.now, time.Hour
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(func() { srv.Close(); _ = svc.CloseAll() })
	return &rig{t: t, svc: svc, srv: srv, clk: clk, dir: dir}
}

// job is a client with the token of one job, in the shapes of the toolkit's v1
// client (cacheHttpClient.ts).
type job struct {
	r     *rig
	token string
}

func (r *rig) job(id, repo, write string, reads ...string) *job {
	return &job{r: r, token: r.svc.Token(id, Scope{Repository: repo, WriteRef: write, ReadRefs: reads}, 6*time.Hour)}
}

func (j *job) do(method, path string, body []byte, hdr map[string]string) (int, []byte, http.Header) {
	j.r.t.Helper()
	req, err := http.NewRequest(method, j.r.srv.URL+APIPath+path, bytes.NewReader(body))
	if err != nil {
		j.r.t.Fatal(err)
	}
	// getRequestOptions / createHttpClient: cacheHttpClient.ts:56-75.
	req.Header.Set("Accept", "application/json;api-version=6.0-preview.1")
	if j.token != "" {
		req.Header.Set("Authorization", "Bearer "+j.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		j.r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// reserve is reserveCache (:190): POST caches {key, version, cacheSize}.
func (j *job) reserve(key, version string, size int) (int, int64) {
	b, _ := json.Marshal(map[string]any{"key": key, "version": version, "cacheSize": size})
	code, body, _ := j.do("POST", "caches", b, map[string]string{"Content-Type": "application/json"})
	var out struct {
		CacheID int64 `json:"cacheId"`
	}
	_ = json.Unmarshal(body, &out)
	return code, out.CacheID
}

// chunk is uploadChunk (:206-250).
func (j *job) chunk(id int64, data []byte, start int) int {
	end := start + len(data) - 1
	code, _, _ := j.do("PATCH", fmt.Sprintf("caches/%d", id), data, map[string]string{
		"Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes %d-%d/*", start, end)})
	return code
}

func (j *job) commit(id int64, size int) int {
	b, _ := json.Marshal(map[string]int{"size": size})
	code, _, _ := j.do("POST", fmt.Sprintf("caches/%d", id), b, map[string]string{"Content-Type": "application/json"})
	return code
}

// save does reserve, chunks of 4 bytes uploaded in reverse order, commit.
func (j *job) save(key, version string, data []byte) int {
	j.r.t.Helper()
	code, id := j.reserve(key, version, len(data))
	if code != http.StatusCreated {
		return code
	}
	for start := (len(data) - 1) / 4 * 4; start >= 0; start -= 4 {
		if c := j.chunk(id, data[start:min(start+4, len(data))], start); c != http.StatusNoContent {
			return c
		}
	}
	return j.commit(id, len(data))
}

type lookupResult struct {
	Key, Scope, Version, Location string
	Time                          string
}

// lookup is getCacheEntry (:77-125): GET cache?keys=&version=.
func (j *job) lookup(version string, keys ...string) (int, lookupResult) {
	j.r.t.Helper()
	code, body, _ := j.do("GET", "cache?keys="+url.QueryEscape(strings.Join(keys, ","))+"&version="+version, nil, nil)
	var e struct {
		CacheKey, Scope, CacheVersion, CreationTime, ArchiveLocation string
	}
	if code == 200 {
		if err := json.Unmarshal(body, &e); err != nil {
			j.r.t.Fatalf("lookup body %q: %v", body, err)
		}
	}
	return code, lookupResult{e.CacheKey, e.Scope, e.CacheVersion, e.ArchiveLocation, e.CreationTime}
}

// fetch downloads the archive like downloadCacheHttpClient (downloadUtils.ts:168):
// a bare GET, no bearer token.
func (r *rig) fetch(loc string) (int, []byte) {
	r.t.Helper()
	resp, err := http.Get(loc)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (j *job) restore(version string, keys ...string) (string, []byte, bool) {
	j.r.t.Helper()
	code, e := j.lookup(version, keys...)
	if code == 204 {
		return "", nil, false
	}
	if code != 200 {
		j.r.t.Fatalf("lookup: %d", code)
	}
	c, data := j.r.fetch(e.Location)
	if c != 200 {
		j.r.t.Fatalf("archive: %d", c)
	}
	return e.Key, data, true
}

const (
	repoA = "bnema/app"
	repoB = "bnema/other"
	main_ = "refs/heads/main"
	feat  = "refs/heads/feat"
	v1    = "0123abcd"
)

func TestSaveAndRestoreByKeyAndPrefix(t *testing.T) {
	r := newRig(t, 1<<20)
	w := r.job("j1", repoA, main_, main_)
	if code := w.save("go-linux-abc", v1, []byte("0123456789")); code != http.StatusNoContent {
		t.Fatalf("save: %d", code)
	}
	rd := r.job("j2", repoA, "", main_)

	// A miss is 204 with no body (the toolkit treats it as "not found").
	if code, _ := rd.lookup(v1, "go-linux-zzz"); code != http.StatusNoContent {
		t.Fatalf("miss: %d", code)
	}
	// Exact primary key.
	key, data, ok := rd.restore(v1, "go-linux-abc")
	if !ok || key != "go-linux-abc" || string(data) != "0123456789" {
		t.Fatalf("exact: %v %q %q", ok, key, data)
	}
	// The primary key is never a prefix: only restore keys are.
	if code, _ := rd.lookup(v1, "go-linux-a"); code != http.StatusNoContent {
		t.Fatalf("primary key matched as prefix: %d", code)
	}
	key, _, ok = rd.restore(v1, "go-linux-zzz", "go-linux-")
	if !ok || key != "go-linux-abc" {
		t.Fatalf("restore key: %v %q", ok, key)
	}
	// Another version (paths, compression) is another cache.
	if code, _ := rd.lookup("ffff", "go-linux-abc"); code != http.StatusNoContent {
		t.Fatalf("other version: %d", code)
	}
	code, e := rd.lookup(v1, "go-linux-abc")
	if code != 200 || e.Scope != main_ || e.Version != v1 || e.Time == "" {
		t.Fatalf("entry %+v", e)
	}
}

func TestRestoreKeyPrefersNewestAndExact(t *testing.T) {
	r := newRig(t, 1<<20)
	w := r.job("j1", repoA, main_, main_)
	for _, k := range []string{"deps-1", "deps-2", "deps-3"} {
		if c := w.save(k, v1, []byte(k)); c != http.StatusNoContent {
			t.Fatal(c)
		}
		r.clk.add(time.Minute)
	}
	if k, _, _ := w.restore(v1, "nomatch", "deps-"); k != "deps-3" {
		t.Fatalf("newest prefix match: %q", k)
	}
	if k, _, _ := w.restore(v1, "nomatch", "deps-1"); k != "deps-1" {
		t.Fatalf("exact restore key: %q", k)
	}
}

func TestReadScopePriority(t *testing.T) {
	r := newRig(t, 1<<20)
	// The default branch built "k" and "pre-1"; the feature ref has its own "k".
	if c := r.job("j0", repoA, main_, main_).save("k", v1, []byte("from-main")); c != 204 {
		t.Fatal(c)
	}
	if c := r.job("j0b", repoA, main_, main_).save("pre-1", v1, []byte("main-pre")); c != 204 {
		t.Fatal(c)
	}
	if c := r.job("j1", repoA, feat, feat, main_).save("k", v1, []byte("from-feat")); c != 204 {
		t.Fatal(c)
	}
	pr := r.job("j2", repoA, feat, feat, main_)
	if _, data, _ := pr.restore(v1, "k"); string(data) != "from-feat" {
		t.Fatalf("own ref first: %q", data)
	}
	// Not in the own ref: the next read ref answers.
	if k, data, ok := pr.restore(v1, "zzz", "pre-"); !ok || k != "pre-1" || string(data) != "main-pre" {
		t.Fatalf("fallback ref: %v %q %q", ok, k, data)
	}
	// A job whose scopes do not include feat never sees its entry.
	other := r.job("j3", repoA, "refs/heads/other", "refs/heads/other", main_)
	if _, data, _ := other.restore(v1, "k"); string(data) != "from-main" {
		t.Fatalf("other ref must read main's: %q", data)
	}
	if code, _ := r.job("j4", repoA, "", "refs/heads/other").lookup(v1, "k"); code != 204 {
		t.Fatalf("read scope leak: %d", code)
	}
}

func TestReserveConflict(t *testing.T) {
	r := newRig(t, 1<<20)
	a := r.job("j1", repoA, feat, feat)
	code, id := a.reserve("k", v1, 4)
	if code != http.StatusCreated || id == 0 {
		t.Fatalf("%d %d", code, id)
	}
	// Reserved but not committed: a second reservation is a 409 (what the toolkit
	// reports as "another job may be creating this cache").
	if code, _ := r.job("j2", repoA, feat, feat).reserve("k", v1, 4); code != http.StatusConflict {
		t.Fatalf("reserved: %d", code)
	}
	// Another version, another ref, another repository do not conflict.
	if code, _ := a.reserve("k", "other", 4); code != http.StatusCreated {
		t.Fatalf("other version: %d", code)
	}
	if code, _ := r.job("j3", repoA, main_, main_).reserve("k", v1, 4); code != http.StatusCreated {
		t.Fatalf("other ref: %d", code)
	}
	if code, _ := r.job("j4", repoB, feat, feat).reserve("k", v1, 4); code != http.StatusCreated {
		t.Fatalf("other repo: %d", code)
	}
	if a.chunk(id, []byte("abcd"), 0) != 204 || a.commit(id, 4) != 204 {
		t.Fatal("upload")
	}
	// Committed: still a conflict.
	if code, _ := r.job("j5", repoA, feat, feat).reserve("k", v1, 4); code != http.StatusConflict {
		t.Fatalf("exists: %d", code)
	}
}

func TestWriteOutsideRefRefused(t *testing.T) {
	r := newRig(t, 1<<20)
	ro := r.job("j1", repoA, "", main_)
	b, _ := json.Marshal(map[string]any{"key": "k", "version": v1, "cacheSize": 4})
	code, body, _ := ro.do("POST", "caches", b, nil)
	if code != http.StatusForbidden || !strings.HasPrefix(string(body), `{"message":"cache write denied: `) {
		t.Fatalf("read-only job: %d %s", code, body)
	}
	// A reservation is bound to its job's write ref: the token of a job with
	// another ref (or another job) cannot write into it, even with the id.
	w := r.job("j2", repoA, feat, feat)
	_, id := w.reserve("k", v1, 4)
	for name, o := range map[string]*job{
		"other ref":  r.job("j3", repoA, main_, main_),
		"other job":  r.job("j4", repoA, feat, feat),
		"other repo": r.job("j2", repoB, feat, feat),
		"read only":  r.job("j2", repoA, "", feat),
	} {
		if c := o.chunk(id, []byte("abcd"), 0); c != http.StatusForbidden {
			t.Errorf("%s: chunk %d", name, c)
		}
		if c := o.commit(id, 4); c != http.StatusForbidden {
			t.Errorf("%s: commit %d", name, c)
		}
	}
	if n, _, _ := r.svc.store.stats(); n != 0 {
		t.Fatalf("entries: %d", n)
	}
	if w.chunk(id, []byte("abcd"), 0) != 204 || w.commit(id, 4) != 204 {
		t.Fatal("the owner can still upload")
	}
}

func TestOtherRepositoryNeverRestored(t *testing.T) {
	r := newRig(t, 1<<20)
	if c := r.job("j1", repoA, main_, main_).save("k", v1, []byte("secret-a")); c != 204 {
		t.Fatal(c)
	}
	b := r.job("j2", repoB, main_, main_)
	if code, _ := b.lookup(v1, "k"); code != 204 {
		t.Fatalf("lookup across repositories: %d", code)
	}
	if code, _ := b.lookup(v1, "x", "k"); code != 204 {
		t.Fatalf("restore key across repositories: %d", code)
	}
	// The signed URL of A's entry is not a way in for B either: the signature is
	// bound to the repository. Control: the genuine URL works; the same id and
	// expiry signed for repoB, and nothing else changed, is refused.
	_, e := r.job("j3", repoA, "", main_).lookup(v1, "k")
	u, _ := url.Parse(e.Location)
	idStr := strings.TrimPrefix(u.Path, APIPath+"artifacts/")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	exp, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if code, data := r.fetch(e.Location); code != 200 || string(data) != "secret-a" {
		t.Fatalf("control: %d %q", code, data)
	}
	forged := url.Values{"exp": {u.Query().Get("exp")}, "job": {"j3"}, "sig": {r.svc.sign.archiveSig(id, repoB, "j3", exp)}}
	if code, _ := r.fetch(r.srv.URL + APIPath + "artifacts/" + idStr + "?" + forged.Encode()); code != 403 {
		t.Fatalf("signature for the other repository: %d", code)
	}
}

func TestBadTokens(t *testing.T) {
	r := newRig(t, 1<<20)
	good := r.job("j1", repoA, main_, main_)
	other := newRig(t, 1<<20) // another runner: another key
	expired := r.svc.sign.token(Claims{JobID: "j", Scope: Scope{Repository: repoA, WriteRef: main_, ReadRefs: []string{main_}}, Expiry: r.clk.now().Add(-time.Second).Unix()})
	seg := strings.Split(good.token, ".")
	tampered := func() string {
		payload, _ := b64.DecodeString(seg[1])
		payload = bytes.Replace(payload, []byte(repoA), []byte(repoB), 1)
		return seg[0] + "." + b64.EncodeToString(payload) + "." + seg[2]
	}()
	algNone := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + seg[1] + "."
	// A token for the results service only has no cache scope.
	resultsOnly := r.svc.Token("j1", Scope{}, time.Hour, "Actions.Results:1:x")
	for name, tok := range map[string]string{
		"none": "", "garbage": "abc", "dots": "a.b", "other key": other.job("j1", repoA, main_, main_).token,
		"expired": expired, "tampered claims": tampered, "no mac": seg[0] + "." + seg[1] + ".",
		"two segments": seg[1] + "." + seg[2], "alg none": algNone, "results only": resultsOnly,
	} {
		j := &job{r: r, token: tok}
		for _, req := range []struct{ m, p string }{{"GET", "cache?keys=k&version=v"}, {"POST", "caches"}, {"PATCH", "caches/1"}, {"POST", "caches/1"}, {"GET", "caches?key=k"}} {
			if code, _, _ := j.do(req.m, req.p, []byte("{}"), nil); code != http.StatusForbidden {
				t.Errorf("%s %s %s: %d", name, req.m, req.p, code)
			}
		}
	}
	// A bare "Authorization: <token>" without the scheme is refused too.
	req, _ := http.NewRequest("GET", r.srv.URL+APIPath+"cache?keys=k&version=v", nil)
	req.Header.Set("Authorization", good.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("no scheme: %d", resp.StatusCode)
	}
	// A token is not an archive signature, nor the other way round.
	if r.svc.sign.archiveOK(1, repoA, "j1", 1<<40, good.token, r.clk.now()) {
		t.Fatal("token accepted as archive signature")
	}
	if code, _ := r.fetch(r.srv.URL + APIPath + "artifacts/1?exp=99999999999&sig=x"); code != 403 {
		t.Fatalf("archive without signature: %d", code)
	}
}

func TestChunkRulesAndCommitVerifiesSize(t *testing.T) {
	r := newRig(t, 1<<20)
	j := r.job("j1", repoA, main_, main_)
	_, id := j.reserve("k", v1, 8)

	if c := j.chunk(id, []byte("0123456789"), 0); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("beyond cacheSize: %d", c)
	}
	// Content-Range that disagrees with the body, either way.
	code, _, _ := j.do("PATCH", fmt.Sprintf("caches/%d", id), []byte("abc"), map[string]string{"Content-Range": "bytes 0-6/*"})
	if code != 400 {
		t.Fatalf("short body: %d", code)
	}
	code, _, _ = j.do("PATCH", fmt.Sprintf("caches/%d", id), []byte("abcdefg"), map[string]string{"Content-Range": "bytes 0-2/*"})
	if code != 400 {
		t.Fatalf("long body: %d", code)
	}
	for _, h := range []string{"", "bytes 5-2/*", "bytes a-b/*", "items 0-1/*", "bytes 0-1"} {
		if c, _, _ := j.do("PATCH", fmt.Sprintf("caches/%d", id), []byte("ab"), map[string]string{"Content-Range": h}); c != 400 {
			t.Errorf("Content-Range %q: %d", h, c)
		}
	}
	// Second half first, a hole at the start: commit refuses.
	if j.chunk(id, []byte("4567"), 4) != 204 {
		t.Fatal("chunk")
	}
	if c := j.commit(id, 8); c != 400 {
		t.Fatalf("commit with a hole: %d", c)
	}
	if j.chunk(id, []byte("0123"), 0) != 204 {
		t.Fatal("chunk")
	}
	if j.chunk(id, []byte("0123"), 0) != 204 { // a retry of the same range
		t.Fatal("chunk, repeated")
	}
	// The size of the commit must be the declared one.
	if c := j.commit(id, 7); c != 400 {
		t.Fatalf("commit with a wrong size: %d", c)
	}
	if c := j.commit(id, 8); c != 204 {
		t.Fatalf("commit: %d", c)
	}
	if c := j.commit(id, 8); c != 404 {
		t.Fatalf("second commit: %d", c)
	}
	if _, data, _ := j.restore(v1, "k"); string(data) != "01234567" {
		t.Fatalf("content %q", data)
	}
	// No temporary file is left.
	des, _ := os.ReadDir(filepath.Join(r.dir, "data"))
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".tmp") {
			t.Errorf("left over %s", de.Name())
		}
	}
}

func TestReservationWithoutSizeIsBoundedByTheLimit(t *testing.T) {
	r := newRig(t, 16)
	j := r.job("j1", repoA, main_, main_)
	code, id := j.reserve("k", v1, 0)
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	if c := j.chunk(id, make([]byte, 17), 0); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit: %d", c)
	}
	if code, _ := j.reserve("big", v1, 17); code != http.StatusBadRequest {
		t.Fatalf("cacheSize over the limit: %d", code)
	}
}

func TestUncommittedReservationsExpire(t *testing.T) {
	r := newRig(t, 100)
	j := r.job("j1", repoA, main_, main_)
	_, id := j.reserve("k", v1, 60)
	// The space is held: a second 60-byte cache has no room (nothing to evict).
	if code, _ := j.reserve("k2", v1, 60); code != http.StatusServiceUnavailable {
		t.Fatalf("held space: %d", code)
	}
	r.clk.add(2 * time.Hour)
	if c := j.chunk(id, []byte("x"), 0); c != http.StatusNotFound {
		t.Fatalf("chunk after expiry: %d", c)
	}
	if code, _ := j.reserve("k2", v1, 60); code != http.StatusCreated {
		t.Fatalf("after expiry: %d", code)
	}
	// The key is free again and the temporary file is gone.
	if code, _ := j.reserve("k", v1, 10); code != http.StatusCreated {
		t.Fatalf("key after expiry: %d", code)
	}
	if _, _, held := r.svc.store.stats(); held != 70 {
		t.Fatalf("held %d", held)
	}
	if _, err := os.Stat(r.svc.store.tmp(id)); !os.IsNotExist(err) {
		t.Fatalf("temp file: %v", err)
	}
}

func TestLRUEviction(t *testing.T) {
	r := newRig(t, 30)
	j := r.job("j1", repoA, main_, main_)
	save := func(k string) {
		t.Helper()
		if c := j.save(k, v1, bytes.Repeat([]byte{k[0]}, 10)); c != 204 {
			t.Fatalf("save %s: %d", k, c)
		}
		r.clk.add(time.Minute)
	}
	save("a")
	save("b")
	save("c")
	// Using "a" makes "b" the least recently used.
	if _, _, ok := j.restore(v1, "a"); !ok {
		t.Fatal("a")
	}
	r.clk.add(time.Minute)
	save("d")
	if code, _ := j.lookup(v1, "b"); code != 204 {
		t.Fatalf("b must be evicted: %d", code)
	}
	for _, k := range []string{"a", "c", "d"} {
		if code, _ := j.lookup(v1, k); code != 200 {
			t.Errorf("%s evicted: %d", k, code)
		}
	}
	if n, used, _ := r.svc.store.stats(); n != 3 || used != 30 {
		t.Fatalf("entries %d used %d", n, used)
	}
	des, _ := os.ReadDir(filepath.Join(r.dir, "data"))
	if len(des) != 3 {
		t.Fatalf("files on disk: %d", len(des))
	}
	// An entry in flight is not evicted, and committing never evicts itself.
	if c := j.save("e", v1, bytes.Repeat([]byte("e"), 25)); c != 204 {
		t.Fatalf("large entry: %d", c)
	}
	if n, used, _ := r.svc.store.stats(); n != 1 || used != 25 {
		t.Fatalf("after large entry: entries %d used %d", n, used)
	}
}

func TestIndexPersists(t *testing.T) {
	r := newRig(t, 1<<20)
	if c := r.job("j1", repoA, main_, main_).save("k", v1, []byte("persist")); c != 204 {
		t.Fatal(c)
	}
	// An abandoned reservation and a stray file are cleaned at restart.
	r.job("j2", repoA, feat, feat).reserve("half", v1, 5)
	if err := os.WriteFile(filepath.Join(r.dir, "data", "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.CloseAll(); err != nil {
		t.Fatal(err)
	}
	svc2, err := New(Config{Dir: r.dir, Now: r.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc2.CloseAll() }()
	e, ok := svc2.store.find(repoA, []string{main_}, []string{"k"}, v1)
	if !ok || e.Size != 7 {
		t.Fatalf("after restart: %v %+v", ok, e)
	}
	des, _ := os.ReadDir(filepath.Join(r.dir, "data"))
	if len(des) != 1 {
		t.Fatalf("files after restart: %d", len(des))
	}
	// Tokens do not survive a restart: the key is in memory only.
	old := r.job("j1", repoA, main_, main_).token
	if _, err := svc2.sign.verify(old, r.clk.now()); err == nil {
		t.Fatal("token from the previous run accepted")
	}
}

func TestListDiagnostics(t *testing.T) {
	r := newRig(t, 1<<20)
	w := r.job("j1", repoA, main_, main_)
	w.save("go-1", v1, []byte("x"))
	code, body, _ := w.do("GET", "caches?key=go-", nil, nil)
	var out struct {
		TotalCount     int
		ArtifactCaches []struct{ CacheKey, Scope, CacheVersion string }
	}
	if code != 200 || json.Unmarshal(body, &out) != nil || out.TotalCount != 1 || out.ArtifactCaches[0].CacheKey != "go-1" {
		t.Fatalf("%d %s", code, body)
	}
}

func TestParseSize(t *testing.T) {
	const def = 7
	for in, want := range map[string]int64{"": def, "  ": def, "10G": 10 << 30, "500m": 500 << 20, "1GiB": 1 << 30, "2048": 2048, "3TB": 3 << 40, " 2G ": 2 << 30,
		"0": 0, "00": 0, "0G": 0, "0GiB": 0, "2GIB": 2 << 30} {
		if got, err := ParseSize(in, def); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"-1", "G", "ten", "1.5G", "99999999999T"} {
		_, err := ParseSize(in, def)
		if err == nil {
			t.Errorf("%q accepted", in)
		} else if strings.Contains(err.Error(), "ERMITE_") {
			t.Errorf("%q: the error names a variable: %v", in, err)
		}
	}
}

func TestAddSpan(t *testing.T) {
	var l []span
	for _, s := range []span{{8, 10}, {0, 4}, {4, 6}, {6, 8}} {
		l = addSpan(l, s)
	}
	if !complete(l, 10) || len(l) != 1 {
		t.Fatalf("%v", l)
	}
	if complete(addSpan(nil, span{1, 5}), 5) {
		t.Fatal("hole at the start")
	}
}
