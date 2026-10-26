// SPDX-License-Identifier: MIT
package deps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
func TestRunCacheFailureDoesNotFailJob(t *testing.T) {
	// Fake only Docker; Run still validates, prepares cache, and collects output.
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	r := request()
	r.Sha = strings.Repeat("a", 40)
	r.CloneUrl = "https://forge.example/o/r.git"
	r.CacheToken = "never-log-this"
	r.CacheMode = "write"
	r.RegistryPort = uint32(port)
	var log bytes.Buffer
	if _, err := Run(context.Background(), r, &log); err != nil {
		t.Fatal(err)
	}
	if strings.Count(log.String(), "warning:") != 1 || strings.Contains(log.String(), r.CacheToken) {
		t.Fatal(log.String())
	}
}
func TestCacheAPI(t *testing.T) {
	var uploaded []byte
	var committed bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cacheAPI+"artifacts/1" && r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing token")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == cacheAPI+"cache":
			if r.URL.Query().Get("keys") != "primary,prefix" || r.URL.Query().Get("version") != cacheVersion {
				t.Error(r.URL)
			}
			if uploaded == nil {
				w.WriteHeader(204)
			} else {
				_, _ = fmt.Fprintf(w, `{"cacheKey":"primary","archiveLocation":%q}`, server.URL+cacheAPI+"artifacts/1")
			}
		case r.Method == "GET":
			_, _ = w.Write(uploaded)
		case r.Method == "PATCH":
			uploaded, _ = io.ReadAll(r.Body)
			w.WriteHeader(204)
		case r.URL.Path == cacheAPI+"caches":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"cacheId":1}`)
		default:
			committed = true
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	c := &moduleCache{ctx: context.Background(), client: server.Client(), base: server.URL, token: "secret", key: "primary", dir: t.TempDir(), log: io.Discard}
	if err := c.restore("prefix"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "cache/download"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("not committed")
	}
	c.dir = t.TempDir()
	if err := c.restore("prefix"); err != nil {
		t.Fatal(err)
	}
	if !c.exact {
		t.Fatal("not exact")
	}
}

func TestFinishSkipsSave(t *testing.T) {
	for _, tc := range []struct {
		success, exact bool
		mode           string
	}{{false, false, "write"}, {true, true, "write"}, {true, false, "read"}} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
		dir := t.TempDir()
		c := &moduleCache{ctx: context.Background(), client: server.Client(), base: server.URL, dir: dir, exact: tc.exact, log: io.Discard}
		c.finish(tc.success, tc.mode)
		server.Close()
		if calls != 0 {
			t.Fatal("unexpected save")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("cache not cleaned")
		}
	}
}

func TestRestoreRejectsArchiveFailure(t *testing.T) {
	for _, bad := range []bool{false, true} {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == cacheAPI+"cache" {
				_, _ = fmt.Fprintf(w, `{"cacheKey":"primary","archiveLocation":%q}`, server.URL+"/archive")
				return
			}
			if bad {
				_, _ = io.WriteString(w, "invalid archive")
			} else {
				w.WriteHeader(503)
			}
		}))
		c := &moduleCache{ctx: context.Background(), client: server.Client(), base: server.URL, key: "primary", dir: t.TempDir(), log: io.Discard}
		if err := c.restore("prefix"); err == nil || c.exact {
			t.Fatal("accepted bad download")
		}
		server.Close()
	}
}
