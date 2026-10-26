// SPDX-License-Identifier: MIT
package deps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
	"sync/atomic"
	"testing"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestCacheKey(t *testing.T) {
	r := &execpb.DepsRequest{GoVersion: "1.27.3", Sha: strings.Repeat("a", 40)}
	if got := cacheKey(r); got != "gomod-1.27.3-"+r.Sha {
		t.Fatal(got)
	}
	if len(cacheVersion) != 64 {
		t.Fatal(cacheVersion)
	}
}
func testArchive(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestExtractCacheRejectsUnsafe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		h     *tar.Header
		limit int64
		count int
	}{
		{"traversal", &tar.Header{Name: "cache/download/../../escape", Typeflag: tar.TypeReg}, 10, 10},
		{"absolute", &tar.Header{Name: "/cache/download/x", Typeflag: tar.TypeReg}, 10, 10},
		{"symlink", &tar.Header{Name: "cache/download/x", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, 10, 10},
		{"hardlink", &tar.Header{Name: "cache/download/x", Typeflag: tar.TypeLink, Linkname: "cache/download/y"}, 10, 10},
		{"device", &tar.Header{Name: "cache/download/x", Typeflag: tar.TypeChar}, 10, 10},
		{"size", &tar.Header{Name: "cache/download/x", Typeflag: tar.TypeReg, Size: 11}, 10, 10},
		{"count", &tar.Header{Name: "cache/download/x", Typeflag: tar.TypeReg}, 10, 0},
		{"modules", &tar.Header{Name: "example.org/mod/file", Typeflag: tar.TypeReg}, 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := extractCache(bytes.NewReader(testArchive(t, tc.h)), t.TempDir(), tc.limit, tc.count); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}
func TestExtractCacheRejectsExpandedBomb(t *testing.T) {
	// A valid tar followed by a long run of zeros: tiny once compressed, but
	// its expanded stream exceeds the limit.
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "cache/download", Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := gz.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if b.Len() > 64<<10 {
		t.Fatalf("bomb is %d bytes compressed", b.Len())
	}
	if err := extractCache(bytes.NewReader(b.Bytes()), t.TempDir(), 64<<10, 10); err == nil {
		t.Fatal("accepted an archive whose expanded stream exceeds the limit")
	}
}
func TestCacheArchiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache/download/example.org/a/@v")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "v1.mod"), []byte("module example.org/a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-cached"), []byte("no"), 0600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := archiveCache(dir, &b); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := extractCache(&b, out, cacheLimit, cacheFiles); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "cache/download/example.org/a/@v/v1.mod"))
	if err != nil || string(got) != "module example.org/a" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(out, "not-cached")); !os.IsNotExist(err) {
		t.Fatal("cached outside download")
	}
}

// runWithFakeDocker runs Run against a fake docker that records its arguments and a
// cache API that answers every call with 503. It returns the arguments, the log
// and the work directory.
func runWithFakeDocker(t *testing.T, cacheToken string) (args []string, log string, work string) {
	t.Helper()
	bin := t.TempDir()
	argFile := filepath.Join(t.TempDir(), "args")
	// One NUL-terminated record per argument: the update script has newlines.
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then printf '%s\\0' \"$@\" > " + argFile + "; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	r := request()
	r.Sha = strings.Repeat("a", 40)
	r.CloneUrl = "https://forge.example/o/r.git"
	r.CacheToken = cacheToken
	if cacheToken != "" {
		r.CacheMode = "write"
		r.RegistryPort = uint32(port)
	}
	work = t.TempDir()
	var buf bytes.Buffer
	if _, err := Run(context.Background(), r, &buf, work); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argFile)
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	return args, buf.String(), work
}

func hasMount(args []string, prefix, suffix string) bool {
	for i := 1; i < len(args); i++ {
		if args[i-1] == "-v" && strings.HasPrefix(args[i], prefix) && strings.HasSuffix(args[i], suffix) {
			return true
		}
	}
	return false
}

func TestRunMountsModuleCacheOnWorkDir(t *testing.T) {
	args, log, work := runWithFakeDocker(t, "never-log-this")
	script := args[len(args)-1]
	if !hasMount(args, work+string(os.PathSeparator)+"ermite-gomod-", ":/gomodcache") {
		t.Errorf("no module cache mount under the work dir: %q", args)
	}
	if !hasMount(args, work+string(os.PathSeparator)+"ermite-deps-", ":/out") {
		t.Errorf("no output mount under the work dir: %q", args)
	}
	for _, want := range []string{"export GOMODCACHE=/gomodcache\n", "trap 'chmod -R a+rwX /gomodcache || true' EXIT\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// The failing cache costs one warning that names the stage, not the job.
	if strings.Count(log, "warning:") != 1 || !strings.Contains(log, "restore: lookup: HTTP 503") {
		t.Errorf("log: %q", log)
	}
	if left, _ := os.ReadDir(work); len(left) != 0 {
		t.Errorf("work dir not cleaned: %v", left)
	}
}

func TestRunWithoutCacheTokenHasNoCacheMount(t *testing.T) {
	args, log, _ := runWithFakeDocker(t, "")
	for _, a := range args {
		if strings.Contains(a, "gomodcache") || strings.Contains(a, "GOMODCACHE") {
			t.Errorf("cache wiring without a token: %q", a)
		}
	}
	if strings.Contains(log, "warning:") {
		t.Errorf("log: %q", log)
	}
}

func TestWorkBase(t *testing.T) {
	dir := t.TempDir()
	if got := workBase(dir); got != dir {
		t.Errorf("%q", got)
	}
	for _, base := range []string{"", filepath.Join(dir, "missing")} {
		if got := workBase(base); got != os.TempDir() {
			t.Errorf("%q: %q", base, got)
		}
	}
}

// fakeCache is a cache API that records what it receives. Zero values answer
// every call successfully.
type fakeCache struct {
	t   *testing.T
	srv *httptest.Server
	// lookup answers the lookup; archive is the body served at /archive.
	lookup                                                   func(w http.ResponseWriter, archiveURL string)
	archive                                                  []byte
	archiveStatus, reserveStatus, uploadStatus, commitStatus int

	lookups, archives, reserves, uploads, commits atomic.Int32

	mu      sync.Mutex
	reserve map[string]any
	commit  map[string]any
	patch   []byte
	rng     string
}

func newFakeCache(t *testing.T) *fakeCache {
	f := &fakeCache{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCache) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	reply := func(status, ok int) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(ok)
	}
	switch {
	case r.Method == "GET" && r.URL.Path == cacheAPI+"cache":
		f.lookups.Add(1)
		if auth != "Bearer secret" || r.Header.Get("Content-Type") != "" {
			f.t.Errorf("lookup headers: %q %q", auth, r.Header.Get("Content-Type"))
		}
		if r.URL.Query().Get("keys") != "primary,prefix" || r.URL.Query().Get("version") != cacheVersion {
			f.t.Error(r.URL)
		}
		f.lookup(w, f.srv.URL+"/archive")
	case r.Method == "GET" && r.URL.Path == "/archive":
		f.archives.Add(1)
		if auth != "" || r.Header.Get("Content-Type") != "" {
			f.t.Errorf("archive request carries credentials or a body type: %q", auth)
		}
		if f.archiveStatus != 0 {
			w.WriteHeader(f.archiveStatus)
			return
		}
		_, _ = w.Write(f.archive)
	case r.Method == "POST" && r.URL.Path == cacheAPI+"caches":
		f.reserves.Add(1)
		if auth != "Bearer secret" {
			f.t.Errorf("reserve auth %q", auth)
		}
		f.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&f.reserve)
		f.mu.Unlock()
		if f.reserveStatus != 0 {
			w.WriteHeader(f.reserveStatus)
			return
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"cacheId":7}`)
	case r.Method == "PATCH" && r.URL.Path == cacheAPI+"caches/7":
		f.uploads.Add(1)
		if auth != "Bearer secret" {
			f.t.Errorf("upload auth %q", auth)
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.patch, f.rng = b, r.Header.Get("Content-Range")
		f.mu.Unlock()
		reply(f.uploadStatus, 204)
	case r.Method == "POST" && r.URL.Path == cacheAPI+"caches/7":
		f.commits.Add(1)
		if auth != "Bearer secret" {
			f.t.Errorf("commit auth %q", auth)
		}
		f.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&f.commit)
		f.mu.Unlock()
		reply(f.commitStatus, 204)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}
}

func hitWith(key string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, archiveURL string) {
		_, _ = fmt.Fprintf(w, `{"cacheKey":%q,"archiveLocation":%q}`, key, archiveURL)
	}
}

func hitAt(location func(archiveURL string) string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, archiveURL string) {
		_, _ = fmt.Fprintf(w, `{"cacheKey":"primary","archiveLocation":%q}`, location(archiveURL))
	}
}

func (f *fakeCache) newModuleCache(t *testing.T) *moduleCache {
	return &moduleCache{ctx: context.Background(), client: f.srv.Client(), base: f.srv.URL, token: "secret", key: "primary", dir: t.TempDir(), tmp: t.TempDir(), mode: "write", log: io.Discard}
}

func goodArchive(t *testing.T) []byte {
	return testArchive(t,
		&tar.Header{Name: "cache/download", Typeflag: tar.TypeDir, Mode: 0700},
		&tar.Header{Name: "cache/download/a.mod", Typeflag: tar.TypeReg, Mode: 0600, Size: 5})
}

func TestRestore(t *testing.T) {
	var other *httptest.Server
	var otherHits atomic.Int32
	other = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { otherHits.Add(1) }))
	t.Cleanup(other.Close)
	for _, tc := range []struct {
		name         string
		setup        func(f *fakeCache)
		wantErr      string
		wantExact    bool
		wantFile     bool
		wantArchives int32
		wantReserves int32 // after a job that downloaded a new module
	}{
		{name: "exact hit", setup: func(f *fakeCache) { f.lookup, f.archive = hitWith("primary"), goodArchive(t) }, wantExact: true, wantFile: true, wantArchives: 1},
		{name: "prefix hit saves later", setup: func(f *fakeCache) { f.lookup, f.archive = hitWith("prefix"), goodArchive(t) }, wantFile: true, wantArchives: 1, wantReserves: 1},
		{name: "miss", setup: func(f *fakeCache) { f.lookup = func(w http.ResponseWriter, _ string) { w.WriteHeader(204) } }, wantReserves: 1},
		{name: "lookup unavailable", setup: func(f *fakeCache) {
			f.lookup = func(w http.ResponseWriter, _ string) { w.WriteHeader(503) }
		}, wantErr: "restore: lookup: HTTP 503"},
		{name: "bad JSON", setup: func(f *fakeCache) {
			f.lookup = func(w http.ResponseWriter, _ string) { _, _ = io.WriteString(w, "{not json") }
		}, wantErr: "restore: lookup: invalid reply"},
		{name: "foreign host", setup: func(f *fakeCache) {
			f.lookup = hitAt(func(string) string { return other.URL + "/archive" })
		}, wantErr: "restore: lookup: invalid archive URL"},
		{name: "userinfo", setup: func(f *fakeCache) {
			f.lookup = hitAt(func(a string) string { return strings.Replace(a, "http://", "http://user@", 1) })
		}, wantErr: "restore: lookup: invalid archive URL"},
		{name: "other scheme", setup: func(f *fakeCache) {
			f.lookup = hitAt(func(a string) string { return strings.Replace(a, "http://", "https://", 1) })
		}, wantErr: "restore: lookup: invalid archive URL"},
		{name: "archive 503", setup: func(f *fakeCache) {
			f.lookup, f.archiveStatus = hitWith("primary"), 503
		}, wantErr: "restore: download: HTTP 503", wantArchives: 1},
		{name: "corrupt archive", setup: func(f *fakeCache) {
			f.lookup, f.archive = hitWith("primary"), []byte("invalid archive")
		}, wantErr: "restore: extract:", wantArchives: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			otherHits.Store(0)
			f := newFakeCache(t)
			tc.setup(f)
			c := f.newModuleCache(t)
			err := c.restore("prefix")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") {
					t.Fatalf("error leaks a secret or URL: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if c.exact != tc.wantExact {
				t.Errorf("exact = %v", c.exact)
			}
			if got := f.archives.Load(); got != tc.wantArchives {
				t.Errorf("archive requests = %d, want %d", got, tc.wantArchives)
			}
			if got := otherHits.Load(); got != 0 {
				t.Errorf("foreign server got %d requests", got)
			}
			if tc.wantErr != "" {
				return
			}
			if _, err := os.Stat(filepath.Join(c.dir, "cache/download/a.mod")); (err == nil) != tc.wantFile {
				t.Errorf("restored file present = %v", err == nil)
			}
			// The job downloads a module, then finishes successfully.
			if err := os.MkdirAll(filepath.Join(c.dir, "cache/download"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.dir, "cache/download/new.mod"), []byte("module"), 0600); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			c.log = &log
			dir := c.dir
			c.finish(true)
			if got := f.reserves.Load(); got != tc.wantReserves {
				t.Fatalf("reservations = %d, want %d (%s)", got, tc.wantReserves, log.String())
			}
			if log.Len() != 0 {
				t.Errorf("log: %s", log.String())
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Error("cache not cleaned")
			}
			if tc.wantReserves == 0 {
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			size := len(f.patch)
			if f.reserve["key"] != "primary" || f.reserve["version"] != cacheVersion || f.reserve["cacheSize"] != float64(size) {
				t.Errorf("reservation %v for a %d byte archive", f.reserve, size)
			}
			if want := fmt.Sprintf("bytes 0-%d/*", size-1); size == 0 || f.rng != want {
				t.Errorf("Content-Range %q, want %q", f.rng, want)
			}
			if f.commit["size"] != float64(size) || f.commits.Load() != 1 {
				t.Errorf("commit %v after %d bytes", f.commit, size)
			}
		})
	}
}

func TestSaveErrorsNameTheStage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(f *fakeCache)
		wantErr string
	}{
		{"reserve conflict", func(f *fakeCache) { f.reserveStatus = 409 }, "save: reserve: HTTP 409"},
		{"upload", func(f *fakeCache) { f.uploadStatus = 500 }, "save: upload: HTTP 500"},
		{"commit", func(f *fakeCache) { f.commitStatus = 503 }, "save: commit: HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCache(t)
			tc.setup(f)
			c := f.newModuleCache(t)
			if err := os.MkdirAll(filepath.Join(c.dir, "cache/download"), 0700); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			c.log = &log
			c.finish(true)
			if !strings.Contains(log.String(), tc.wantErr) || strings.Count(log.String(), "warning:") != 1 {
				t.Fatalf("log %q, want %q", log.String(), tc.wantErr)
			}
			if strings.Contains(log.String(), "secret") || strings.Contains(log.String(), "127.0.0.1") {
				t.Fatalf("log leaks a secret or URL: %q", log.String())
			}
		})
	}
}

func TestFinishSkipsSave(t *testing.T) {
	for _, tc := range []struct {
		name    string
		success bool
		exact   bool
		mode    string
	}{{"failed job", false, false, "write"}, {"exact hit", true, true, "write"}, {"read-only scope", true, false, "read"}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			t.Cleanup(server.Close)
			dir := t.TempDir()
			c := &moduleCache{ctx: context.Background(), client: server.Client(), base: server.URL, dir: dir, tmp: t.TempDir(), exact: tc.exact, mode: tc.mode, log: io.Discard}
			c.finish(tc.success)
			if calls.Load() != 0 {
				t.Fatal("unexpected save")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("cache not cleaned")
			}
		})
	}
}

func TestArchiveCacheRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "cache/download")
	if err := os.MkdirAll(d, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	if err := archiveCache(dir, io.Discard); err == nil {
		t.Fatal("archived a symlink")
	}
}

func TestCappedWriter(t *testing.T) {
	var b bytes.Buffer
	w := &cappedWriter{&b, 4}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write([]byte("d")); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write([]byte("e")); err == nil || n != 0 {
		t.Fatal("wrote past the limit", n, err)
	}
	if b.String() != "abcd" {
		t.Fatal(b.String())
	}
}
