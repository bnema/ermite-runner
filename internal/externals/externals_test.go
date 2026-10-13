// SPDX-License-Identifier: MIT

package externals

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// ---- the table

func TestPinnedTable(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, a := range Pinned {
		key := a.Name + "/" + a.Arch
		if seen[key] {
			t.Errorf("%s listed twice", key)
		}
		seen[key] = true
		if !strings.HasPrefix(a.URL, "https://") || !strings.HasSuffix(a.URL, ".tar.gz") || !strings.Contains(a.URL, "v"+a.Version+"/") {
			t.Errorf("%s: URL %q", key, a.URL)
		}
		if !hex64.MatchString(a.SHA256) {
			t.Errorf("%s: SHA-256 %q", key, a.SHA256)
		}
		if a.StripTop != strings.HasPrefix(a.URL, nodeDist) {
			t.Errorf("%s: StripTop %v for %s", key, a.StripTop, a.URL)
		}
	}
	// actions/runner's externals.sh: node20 and node24 for both architectures,
	// the alpine builds for x64.
	for _, k := range []string{"node20/x64", "node20/arm64", "node24/x64", "node24/arm64", "node20_alpine/x64", "node24_alpine/x64"} {
		if !seen[k] {
			t.Errorf("%s missing", k)
		}
	}
	for _, a := range Pinned {
		if strings.HasPrefix(a.Name, "node20") && a.Version != "20.20.2" || strings.HasPrefix(a.Name, "node24") && a.Version != "24.21.0" {
			t.Errorf("%s pins %s", a.Name, a.Version)
		}
	}
	for _, a := range Pinned {
		if strings.HasSuffix(a.Name, "_alpine") && (a.Arch != "x64" || !strings.HasPrefix(a.URL, alpineDist+"v"+a.Version+"/node-v"+a.Version+"-alpine-x64.tar.gz")) {
			t.Errorf("%s: %+v", a.Name, a)
		}
	}
	if got := len(ForArch(Pinned, "arm64")); got != 2 {
		t.Errorf("arm64 has %d artifacts", got)
	}
}

func TestVerifySum(t *testing.T) {
	if err := VerifySum("AB12", "ab12"); err != nil {
		t.Error(err)
	}
	if err := VerifySum("ab12", "ab13"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("got %v", err)
	}
}

// ---- archives

type ent struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func targz(t *testing.T, ents ...ent) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: typ, Mode: e.mode, Size: int64(len(e.body))}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
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
	return buf.Bytes()
}

func nodeTree() []ent {
	return []ent{
		{name: "node-v1-linux-x64/", typ: tar.TypeDir, mode: 0o755},
		{name: "node-v1-linux-x64/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "node-v1-linux-x64/bin/node", mode: 0o755, body: "#!/bin/sh\necho v1\n"},
		{name: "node-v1-linux-x64/lib/node_modules/npm/bin/npm-cli.js", mode: 0o644, body: "x"},
		{name: "node-v1-linux-x64/bin/npm", typ: tar.TypeSymlink, link: "../lib/node_modules/npm/bin/npm-cli.js", mode: 0o777},
		{name: "node-v1-linux-x64/bin/nodejs", typ: tar.TypeLink, link: "node-v1-linux-x64/bin/node", mode: 0o755},
	}
}

func extractBytes(t *testing.T, data []byte) (string, error) {
	t.Helper()
	dir := t.TempDir()
	arch := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(arch, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst, extractTarGz(arch, dst, true)
}

func TestExtractLayoutAndModes(t *testing.T) {
	dst, err := extractBytes(t, targz(t, nodeTree()...))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "bin", "node"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("bin/node: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "lib/node_modules/npm/bin/npm-cli.js")); st == nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("npm-cli.js: %v", st)
	}
	if l, err := os.Readlink(filepath.Join(dst, "bin", "npm")); err != nil || l != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Fatalf("npm link: %q %v", l, err)
	}
	a, _ := os.Stat(filepath.Join(dst, "bin", "node"))
	b, _ := os.Stat(filepath.Join(dst, "bin", "nodejs"))
	if !os.SameFile(a, b) {
		t.Fatal("hard link not recreated")
	}
	if !isReady(dst) {
		t.Fatal("not ready")
	}
}

func TestModesDoNotDependOnTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dst, err := extractBytes(t, targz(t, nodeTree()...))
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{
		dst: 0o755, filepath.Join(dst, "bin"): 0o755, filepath.Join(dst, "lib/node_modules"): 0o755,
		filepath.Join(dst, "bin/node"): 0o755, filepath.Join(dst, "lib/node_modules/npm/bin/npm-cli.js"): 0o644,
	} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, st, err, want)
		}
	}
	m := &Manager{Dir: filepath.Join(t.TempDir(), "e"), Arch: "x64"}
	e := m.unavailable("node24", "why", false)
	for p, want := range map[string]os.FileMode{
		filepath.Dir(e.ext.Dir): 0o755, e.ext.Dir: 0o755, filepath.Join(e.ext.Dir, "bin"): 0o755,
		filepath.Join(e.ext.Dir, "bin/node"): 0o755, filepath.Join(e.ext.Dir, "UNAVAILABLE"): 0o644,
	} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, st, err, want)
		}
	}
}

func TestExtractRefusesUnsafeArchives(t *testing.T) {
	top := "node-v1-linux-x64/"
	for name, ents := range map[string][]ent{
		"absolute path":      {{name: "/etc/passwd", body: "x"}},
		"dot dot":            {{name: top + "../../evil", body: "x"}},
		"dot dot in middle":  {{name: top + "a/../../evil", body: "x"}},
		"backslash":          {{name: top + `a\b`, body: "x"}},
		"absolute symlink":   {{name: top + "l", typ: tar.TypeSymlink, link: "/etc"}},
		"escaping symlink":   {{name: top + "l", typ: tar.TypeSymlink, link: "../../.."}},
		"escaping from deep": {{name: top + "a/b/l", typ: tar.TypeSymlink, link: "../../../x"}},
		"climb after a name": {{name: top + "a/l", typ: tar.TypeSymlink, link: "b/../../../x"}},
		"empty link target":  {{name: top + "l", typ: tar.TypeSymlink, link: ""}},
		"write through link": {
			{name: top + "d", typ: tar.TypeSymlink, link: "."},
			{name: top + "d/file", body: "x"},
		},
		"write through dir link": {
			{name: top + "sub/", typ: tar.TypeDir},
			{name: top + "sub/up", typ: tar.TypeSymlink, link: ".."},
			{name: top + "sub/up/file", body: "x"},
		},
		"hard link outside": {{name: top + "h", typ: tar.TypeLink, link: "/etc/passwd"}},
		"hard link dot dot": {{name: top + "h", typ: tar.TypeLink, link: top + "../x"}},
		"hard link to top":  {{name: top + "h", typ: tar.TypeLink, link: top}},
		"hard link to a link": {
			{name: top + "l", typ: tar.TypeSymlink, link: "x"},
			{name: top + "h", typ: tar.TypeLink, link: top + "l"},
		},
		"device":    {{name: top + "dev", typ: tar.TypeChar}},
		"fifo":      {{name: top + "fifo", typ: tar.TypeFifo}},
		"duplicate": {{name: top + "f", body: "1"}, {name: top + "f", body: "2"}},
	} {
		t.Run(name, func(t *testing.T) {
			dst, err := extractBytes(t, targz(t, ents...))
			if err == nil {
				t.Fatal("accepted")
			}
			// Nothing was written outside the destination, whatever the order.
			parent := filepath.Dir(dst)
			if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
				t.Fatal("wrote outside")
			}
			if _, err := os.Stat(filepath.Join(dst, "..", "file")); err == nil {
				t.Fatal("wrote outside through a link")
			}
		})
	}
}

func TestExtractRefusesGarbageAndTooMany(t *testing.T) {
	if _, err := extractBytes(t, []byte("not gzip")); err == nil {
		t.Error("garbage accepted")
	}
	var ents []ent
	for i := 0; i < maxEntries+1; i++ {
		ents = append(ents, ent{name: "t/d/", typ: tar.TypeDir})
	}
	if _, err := extractBytes(t, targz(t, ents...)); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("entry limit: %v", err)
	}
}

func TestLinkTargetRule(t *testing.T) {
	for _, c := range []struct {
		name, target string
		ok           bool
	}{
		{"bin/npm", "../lib/x", true},
		{"bin/npm", "../../x", false}, // one above the tree
		{"lib/a/b/l", "../../../x", true},
		{"lib/a/b/l", "../../../../x", false},
		{"l", "bin/node", true},
		{"l", "../x", false},
		{"l", "/abs", false},
		{"a/l", "b/../c", false},
	} {
		if err := checkLinkTarget(c.name, c.target); (err == nil) != c.ok {
			t.Errorf("%s -> %s: %v", c.name, c.target, err)
		}
	}
}

// ---- the manager

type fixture struct {
	srv           *httptest.Server
	hits          atomic.Int64
	archive       []byte
	sum           string
	archiveAlpine []byte // no top directory, like actions/alpine_nodejs
	sumAlpine     string
	table         []Artifact
	dir           string
	m             *Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.archive = targz(t, nodeTree()...)
	s := sha256.Sum256(f.archive)
	f.sum = hex.EncodeToString(s[:])
	f.archiveAlpine = targz(t,
		ent{name: "./bin/", typ: tar.TypeDir, mode: 0o755},
		ent{name: "./bin/node", mode: 0o755, body: "#!/bin/sh\necho alpine\n"},
		ent{name: "./LICENSE", mode: 0o644, body: "x"})
	sa := sha256.Sum256(f.archiveAlpine)
	f.sumAlpine = hex.EncodeToString(sa[:])
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/alpine.tar.gz"):
			_, _ = w.Write(f.archiveAlpine)
		case strings.HasSuffix(r.URL.Path, "-linux-x64.tar.gz"):
			_, _ = w.Write(f.archive)
		case strings.HasSuffix(r.URL.Path, "tampered.tar.gz"):
			_, _ = w.Write(append([]byte(nil), f.archive[:len(f.archive)-1]...))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.dir = filepath.Join(t.TempDir(), "externals")
	f.table = []Artifact{
		{Name: "node20", Version: "1.0.0", Arch: "x64", URL: f.srv.URL + "/v1.0.0/node-v1.0.0-linux-x64.tar.gz", SHA256: f.sum, StripTop: true},
		{Name: "node24", Version: "2.0.0", Arch: "x64", URL: f.srv.URL + "/v2.0.0/node-v2.0.0-linux-x64.tar.gz", SHA256: f.sum, StripTop: true},
		{Name: "node20_alpine", Version: "1.0.0", Arch: "x64", URL: f.srv.URL + "/v1.0.0/alpine.tar.gz", SHA256: f.sumAlpine},
	}
	f.m = &Manager{Dir: f.dir, Arch: "x64", Table: f.table, Client: f.srv.Client()}
	return f
}

func byName(set []sandbox.External) map[string]sandbox.External {
	m := map[string]sandbox.External{}
	for _, e := range set {
		m[e.Name] = e
	}
	return m
}

func TestSyncDownloadsVerifiesAndReuses(t *testing.T) {
	f := newFixture(t)
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	set := byName(f.m.Externals())
	n20, n24, al := set["node20"], set["node24"], set["node20_alpine"]
	if n20.Unavailable || n24.Unavailable || n20.Dir != filepath.Join(f.dir, "node20-1.0.0-x64") {
		t.Fatalf("set: %+v", set)
	}
	if n20.ID == n24.ID {
		t.Error("two tools share an ID")
	}
	if b, err := os.ReadFile(filepath.Join(n24.Dir, "bin", "node")); err != nil || !strings.Contains(string(b), "echo v1") {
		t.Fatalf("node24: %q %v", b, err)
	}
	// The alpine archive has no top directory: bin/node is at its root.
	if al.Unavailable {
		t.Fatalf("alpine: %+v", al)
	}
	if b, err := os.ReadFile(filepath.Join(al.Dir, "bin", "node")); err != nil || !strings.Contains(string(b), "echo alpine") {
		t.Fatalf("alpine node: %q %v", b, err)
	}
	hits := f.hits.Load()
	if hits != 3 {
		t.Fatalf("downloads: %d", hits)
	}

	// A second start finds everything in place and downloads nothing.
	m2 := &Manager{Dir: f.dir, Arch: "x64", Table: f.table, Client: f.srv.Client()}
	if err := m2.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != hits {
		t.Fatal("downloaded again")
	}
	if got := byName(m2.Externals())["node24"]; got.ID != n24.ID {
		t.Fatalf("ID changed: %q -> %q", n24.ID, got.ID)
	}
}

// runStub runs the bin/node of an unavailable tool: it must fail with the reason
// on stderr, from wherever the tool is mounted.
func runStub(t *testing.T, e sandbox.External, want string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh needed")
	}
	// Run it through a symlinked path too, as a guest sees it at another place.
	cmd := exec.Command(filepath.Join(e.Dir, "bin", "node"), "-e", "x")
	var stderr, stdout bytes.Buffer
	cmd.Stderr, cmd.Stdout = &stderr, &stdout
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("the stub must exit 1, got %v", err)
	}
	if !strings.Contains(stderr.String(), want) || stdout.Len() != 0 {
		t.Fatalf("stub stderr %q stdout %q, want %q", stderr.String(), stdout.String(), want)
	}
	if b, _ := os.ReadFile(filepath.Join(e.Dir, "bin", "node")); strings.Contains(string(b), want) {
		t.Fatalf("the stub holds the reason: %q", b)
	}
}

func TestSyncRefusesAChecksumMismatchBeforeExtracting(t *testing.T) {
	f := newFixture(t)
	f.table[0].SHA256 = strings.Repeat("0", 64)
	f.m.Table = f.table
	err := f.m.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("got %v", err)
	}
	n20 := byName(f.m.Externals())["node20"]
	if !n20.Unavailable {
		t.Fatal("a tool with a wrong checksum is available")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "node20-1.0.0-x64")); err == nil {
		t.Fatal("extracted although the checksum differs")
	}
	reason, _ := os.ReadFile(filepath.Join(n20.Dir, sandbox.ExternalUnavailableFile))
	if !strings.Contains(string(reason), "SHA-256 mismatch") {
		t.Fatalf("reason: %q", reason)
	}
	// The other tool is unaffected.
	if byName(f.m.Externals())["node24"].Unavailable {
		t.Fatal("node24 lost to node20's failure")
	}
	// A checksum failure is a verdict: no retry is scheduled for it.
	if f.m.state["node20"].retry {
		t.Fatal("retry scheduled for a mismatch")
	}
	runStub(t, n20, "SHA-256 mismatch")
	ents, _ := os.ReadDir(f.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("leftover %s", e.Name())
		}
	}
}

func TestSyncRefusesAnUnsafeArchiveWithTheRightChecksum(t *testing.T) {
	f := newFixture(t)
	bad := targz(t, ent{name: "node-v1-linux-x64/bin/node", mode: 0o755, body: "x"}, ent{name: "node-v1-linux-x64/l", typ: tar.TypeSymlink, link: "/etc"})
	f.archive = bad
	s := sha256.Sum256(bad)
	f.table[0].SHA256, f.table[1].SHA256 = hex.EncodeToString(s[:]), hex.EncodeToString(s[:])
	f.m.Table = f.table
	err := f.m.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "node20-1.0.0-x64")); err == nil {
		t.Fatal("unsafe archive left in place")
	}
}

func TestSyncOfflineKeepsTheRunnerGoing(t *testing.T) {
	f := newFixture(t)
	f.srv.Close() // offline
	err := f.m.Sync(context.Background())
	if err == nil {
		t.Fatal("no error offline")
	}
	for _, n := range []string{"node20", "node24"} {
		e := byName(f.m.Externals())[n]
		if !e.Unavailable {
			t.Fatalf("%s available offline", n)
		}
		runStub(t, e, "is not available on this runner")
	}
	if !f.m.wantsRetry() {
		t.Fatal("a network failure must be retried")
	}
}

func TestBadStatusAndRedirectToHTTP(t *testing.T) {
	f := newFixture(t)
	f.table[0].URL = f.srv.URL + "/missing.tar.gz"
	f.m.Table = f.table
	if err := f.m.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
	if !byName(f.m.Externals())["node20"].Unavailable {
		t.Fatal("404 gives a tool")
	}

	// The default client refuses a redirect to plain http, and too many.
	check := newClient().CheckRedirect
	if err := check(&http.Request{URL: mustURL(t, "http://example.org/x")}, nil); err == nil {
		t.Error("redirect to http followed")
	}
	if err := check(&http.Request{URL: mustURL(t, "https://example.org/x")}, make([]*http.Request, 5)); err == nil {
		t.Error("redirect loop followed")
	}
	if err := check(&http.Request{URL: mustURL(t, "https://example.org/x")}, make([]*http.Request, 1)); err != nil {
		t.Error(err)
	}
	// A plain http URL in the table is refused outright.
	m2 := &Manager{Dir: filepath.Join(t.TempDir(), "e"), Arch: "x64", Client: f.srv.Client()}
	m2.Table = []Artifact{{Name: "node20", Version: "1", Arch: "x64", URL: "http://example.invalid/a.tar.gz", SHA256: f.sum}}
	if err := m2.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Fatalf("got %v", err)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSyncKeepsOnlyPinnedVersions(t *testing.T) {
	f := newFixture(t)
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Leftovers: an old version, an interrupted download, a stray file, an
	// incomplete directory under a pinned name.
	for _, d := range []string{"node20-0.9.0-x64", ".tmp-123", "node24-2.0.0-arm64", "node-old"} {
		if err := os.MkdirAll(filepath.Join(f.dir, d, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	ents, _ := os.ReadDir(f.dir)
	for _, e := range ents {
		got = append(got, e.Name())
	}
	want := "node20-1.0.0-x64 node20_alpine-1.0.0-x64 node24-2.0.0-x64"
	if strings.Join(got, " ") != want {
		t.Fatalf("kept %v, want %s", got, want)
	}
}

func TestSyncRebuildsAnIncompleteDirectory(t *testing.T) {
	f := newFixture(t)
	final := filepath.Join(f.dir, "node20-1.0.0-x64")
	if err := os.MkdirAll(filepath.Join(final, "bin"), 0o755); err != nil { // no bin/node
		t.Fatal(err)
	}
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !isReady(final) {
		t.Fatal("incomplete directory kept")
	}
}

func TestANewVersionChangesTheIDAndDropsTheOldDirectory(t *testing.T) {
	f := newFixture(t)
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := byName(f.m.Externals())["node24"]
	f.table[1].Version = "2.1.0"
	f.table[1].URL = f.srv.URL + "/v2.1.0/node-v2.1.0-linux-x64.tar.gz"
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := byName(f.m.Externals())["node24"]
	if n.ID == old.ID || n.Dir == old.Dir {
		t.Fatalf("same tool after an upgrade: %+v %+v", old, n)
	}
	if _, err := os.Stat(old.Dir); err == nil {
		t.Fatal("old version kept")
	}
}

func TestNoBuildForTheArchitecture(t *testing.T) {
	f := newFixture(t)
	f.m.Arch = "arm64" // the fixture table is x64 only
	if err := f.m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.m.Externals() {
		reason, _ := os.ReadFile(filepath.Join(e.Dir, sandbox.ExternalUnavailableFile))
		if !e.Unavailable || !strings.Contains(string(reason), "no "+e.Name+" build for linux-arm64") {
			t.Errorf("%s: %+v %q", e.Name, e, reason)
		}
	}
	if f.hits.Load() != 0 {
		t.Error("downloaded for the wrong architecture")
	}
}

func TestBeforeTheFirstSyncTheToolsAreBeingDownloaded(t *testing.T) {
	f := newFixture(t)
	set := f.m.Externals()
	if len(set) != 3 {
		t.Fatalf("set: %+v", set)
	}
	for _, e := range set {
		reason, _ := os.ReadFile(filepath.Join(e.Dir, sandbox.ExternalUnavailableFile))
		if !e.Unavailable || !strings.Contains(string(reason), "still being downloaded") {
			t.Errorf("%s: %q", e.Name, reason)
		}
	}
}

func TestFailedDownloadIsRetriedInTheBackground(t *testing.T) {
	f := newFixture(t)
	var up atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(f.archive)
	}))
	defer srv.Close()
	m := &Manager{Dir: f.dir, Arch: "x64", Client: srv.Client(), RetryEvery: time.Millisecond}
	m.Table = []Artifact{{Name: "node24", Version: "2.0.0", Arch: "x64", URL: srv.URL + "/a-linux-x64.tar.gz", SHA256: f.sum, StripTop: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, time.Minute)
	if !m.Externals()[0].Unavailable {
		t.Fatal("available while the server is down")
	}
	up.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if !m.Externals()[0].Unavailable {
			return
		}
	}
	t.Fatal("never retried")
}

func TestStartDoesNotWaitLongerThanAsked(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	m := &Manager{Dir: filepath.Join(t.TempDir(), "e"), Arch: "x64", Client: srv.Client()}
	m.Table = []Artifact{{Name: "node24", Version: "2.0.0", Arch: "x64", URL: srv.URL + "/a.tar.gz", SHA256: strings.Repeat("a", 64)}}
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	m.Start(ctx, 100*time.Millisecond)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %v", d)
	}
	// The download goes on in the background; stop it before the directory goes.
	cancel()
	close(block)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		m.mu.Lock()
		busy := m.syncing
		m.mu.Unlock()
		if !busy {
			return
		}
	}
	t.Fatal("the background sync did not stop")
}

// ---- the directories of unavailable tools

func TestStubIsBuiltOnceAndNeverRewrittenInPlace(t *testing.T) {
	m := &Manager{Dir: filepath.Join(t.TempDir(), "e"), Arch: "x64"}
	e := m.unavailable("node24", "why", false)
	node := filepath.Join(e.ext.Dir, "bin", "node")
	file := filepath.Join(e.ext.Dir, sandbox.ExternalUnavailableFile)
	ino := func(p string) uint64 {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Sys().(*syscall.Stat_t).Ino
	}
	n0, f0 := ino(node), ino(file)

	// Again, from this process and from a fresh one (a restart): same files.
	m.unavailable("node24", "why", false)
	(&Manager{Dir: m.Dir, Arch: "x64"}).unavailable("node24", "why", false)
	if ino(node) != n0 || ino(file) != f0 {
		t.Fatal("an existing, correct stub was rewritten")
	}

	// A damaged or missing file is replaced by a rename (a new inode, so a reader
	// of the old one never sees it change), with the right mode.
	if err := os.WriteFile(node, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	(&Manager{Dir: m.Dir, Arch: "x64"}).unavailable("node24", "why", false)
	if b, _ := os.ReadFile(node); string(b) != unavailableStub {
		t.Fatalf("stub not repaired: %q", b)
	}
	if b, _ := os.ReadFile(file); string(b) != "why\n" {
		t.Fatalf("reason not repaired: %q", b)
	}
	if st, _ := os.Stat(node); st.Mode().Perm() != 0o755 || ino(node) == n0 {
		t.Fatalf("repair in place: %v", st.Mode())
	}
	runStub(t, e.ext, "why")
	left, _ := filepath.Glob(filepath.Join(e.ext.Dir, "*", ".tmp-*"))
	left2, _ := filepath.Glob(filepath.Join(e.ext.Dir, ".tmp-*"))
	if len(left)+len(left2) != 0 {
		t.Fatalf("temporary files left: %v %v", left, left2)
	}
}

// While Externals() and Sync() run in several goroutines, a reader that walks
// the directories of the unavailable tools (as the build of a VM's externals
// disk does) never sees a missing, empty or partial file.
func TestConcurrentExternalsAndSyncNeverShowAHalfWrittenStub(t *testing.T) {
	f := newFixture(t)
	f.srv.Close() // offline: every tool is unavailable
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Value
	check := func(dir string) {
		for _, c := range []struct{ name, want string }{{"bin/node", unavailableStub}} {
			b, err := os.ReadFile(filepath.Join(dir, c.name))
			if err != nil {
				bad.Store(fmt.Sprintf("%s/%s: %v", dir, c.name, err))
			} else if string(b) != c.want {
				bad.Store(fmt.Sprintf("%s/%s: %q", dir, c.name, b))
			}
		}
		b, err := os.ReadFile(filepath.Join(dir, sandbox.ExternalUnavailableFile))
		if err != nil || len(b) == 0 || !strings.HasSuffix(string(b), "\n") {
			bad.Store(fmt.Sprintf("%s/UNAVAILABLE: %q %v", dir, b, err))
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, e := range f.m.Externals() {
					check(e.Dir) // what a driver would hand to the disk build
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			dirs, _ := filepath.Glob(filepath.Join(f.dir, unavailableDir, "*"))
			for _, d := range dirs {
				if !strings.Contains(filepath.Base(d), ".tmp-") {
					check(d)
				}
			}
		}
	}()
	for i := 0; i < 5; i++ {
		_ = f.m.Sync(ctx)
	}
	close(stop)
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatalf("a reader saw an incomplete stub: %v", v)
	}
}

func TestPruneKeepsStubDirectoriesHandedOutInTheLastHour(t *testing.T) {
	f := newFixture(t)
	f.srv.Close()
	_ = f.m.Sync(context.Background()) // offline: stubs for the three tools
	root := filepath.Join(f.dir, unavailableDir)
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(p, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fresh := mk("node24-unavailable-aaaaaaaaaaaaaaaa", 5*time.Minute) // handed to a running job
	stale := mk("node24-unavailable-bbbbbbbbbbbbbbbb", 2*time.Hour)

	// A reason that is not current any more but was handed out again just now
	// (Externals touches what it returns) survives; one that was not, does not.
	cur := f.m.Externals()[0]
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(cur.Dir, old, old); err != nil {
		t.Fatal(err)
	}
	f.m.Externals() // handed out: touched
	if st, _ := os.Stat(cur.Dir); time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("Externals did not touch %s", cur.Dir)
	}
	// The set changes (the reasons are not the same any more), the touched
	// directory is no longer current.
	f.m.mu.Lock()
	f.m.state = map[string]entry{}
	f.m.mu.Unlock()
	f.m.prune(zerowrap.Logger{}, map[string]bool{})
	for _, p := range []string{fresh, cur.Dir} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was pruned although handed out in the last hour", p)
		}
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale stub directory was kept")
	}
}

// ---- retries

func TestRetriesRunOnATickerWithoutAnyJob(t *testing.T) {
	f := newFixture(t)
	var up atomic.Bool
	var calls atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(f.archive)
	}))
	defer srv.Close()
	m := &Manager{Dir: f.dir, Arch: "x64", Client: srv.Client(), RetryEvery: 20 * time.Millisecond}
	m.Table = []Artifact{{Name: "node24", Version: "2.0.0", Arch: "x64", URL: srv.URL + "/a-linux-x64.tar.gz", SHA256: f.sum, StripTop: true}}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx, time.Minute)

	// Nobody calls Externals(): the ticker alone retries.
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 3 {
		t.Fatalf("%d attempts without any job", calls.Load())
	}
	up.Store(true)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		ok := !m.state["node24"].ext.Unavailable
		m.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.Externals()[0].Unavailable {
		t.Fatal("never recovered")
	}

	// Once everything is there the ticker stops downloading, and with the context
	// it stops altogether.
	before := calls.Load()
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != before {
		t.Fatal("the ticker keeps requesting what it has")
	}
	cancel()
	m.mu.Lock()
	m.state["node24"] = entry{ext: sandbox.External{Name: "node24", Unavailable: true}, retry: true}
	m.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != before {
		t.Fatal("a retry ran after the context ended")
	}
}

// A verdict (wrong checksum) is not retried by the ticker.
func TestNoRetryForAVerdict(t *testing.T) {
	f := newFixture(t)
	f.table[0].SHA256 = strings.Repeat("0", 64)
	m := &Manager{Dir: f.dir, Arch: "x64", Table: f.table[:1], Client: f.srv.Client(), RetryEvery: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, time.Minute)
	hits := f.hits.Load()
	time.Sleep(150 * time.Millisecond)
	if f.hits.Load() != hits {
		t.Fatal("a checksum mismatch was retried")
	}
}

// ---- the real archives, when they are at hand

// The actions/alpine_nodejs archives have no top directory (./bin/node). Set
// ERMITE_RUNNER_TEST_ALPINE_DIR, or leave the files in /tmp/alpine-node.
func TestRealAlpineArchives(t *testing.T) {
	dir := os.Getenv("ERMITE_RUNNER_TEST_ALPINE_DIR")
	if dir == "" {
		dir = "/tmp/alpine-node"
	}
	ran := false
	for _, a := range Pinned {
		if !strings.HasSuffix(a.Name, "_alpine") {
			continue
		}
		archive := filepath.Join(dir, filepath.Base(a.URL))
		b, err := os.ReadFile(archive)
		if err != nil {
			t.Logf("%s: %v", a.Name, err)
			continue
		}
		ran = true
		t.Run(a.Name, func(t *testing.T) {
			sum := sha256.Sum256(b)
			if err := VerifySum(hex.EncodeToString(sum[:]), a.SHA256); err != nil {
				t.Fatalf("the pinned checksum is not the file's: %v", err)
			}
			dst := t.TempDir()
			if err := extractTarGz(archive, dst, a.StripTop); err != nil {
				t.Fatal(err)
			}
			if !isReady(dst) {
				t.Fatal("no executable bin/node")
			}
			if st, err := os.Stat(filepath.Join(dst, "LICENSE")); err != nil || st.Mode().Perm() != 0o644 {
				t.Fatalf("LICENSE: %v %v", st, err)
			}
			// Taking the first component off would lose bin/node.
			wrong := t.TempDir()
			_ = extractTarGz(archive, wrong, true)
			if isReady(wrong) {
				t.Fatal("the alpine archive must not be unpacked with the top directory stripped")
			}
		})
	}
	if !ran {
		t.Skipf("no alpine archive in %s", dir)
	}
}
