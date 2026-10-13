// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Values recorded on github.com (expr-hashfiles), for the files the scenario
// creates: alpha, beta and gamma.
func TestHashFilesRecordedValues(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{"hf/a.txt": "alpha\n", "hf/b.txt": "beta\n", "hf/sub/c.md": "gamma\n"})
	for _, tc := range []struct {
		patterns []string
		want     string
	}{
		{[]string{"hf/a.txt"}, "4bb706b95c7ea23f44bc5d035ad8841af479871295d2ae0c685d07174705c880"},
		{[]string{"hf/a.txt", "hf/b.txt"}, "24d116e0411b3a4a8d3d5c9c88c150bc4d4603a490294bd4b23d3ef549e1f1a0"},
		{[]string{"hf/b.txt", "hf/a.txt"}, "a3013a35661fd0340c6651bfd9b89fd7c86e916011c7e9cfecfb835d90ec7505"},
		{[]string{"hf/*.txt"}, "24d116e0411b3a4a8d3d5c9c88c150bc4d4603a490294bd4b23d3ef549e1f1a0"},
		{[]string{"hf/**"}, "58d680606dbb1f9fab754d2d071e528858b6ca169b64be479ae9557d72c18d77"},
		{[]string{"hf/**/*.md"}, "b9f1217dc10c76d1ab32754b4df630a29025a4a878aeddd113fda7045f9faf46"},
		{[]string{"hf/**", "!hf/sub/**"}, "24d116e0411b3a4a8d3d5c9c88c150bc4d4603a490294bd4b23d3ef549e1f1a0"},
		{[]string{"hf/nothing-*"}, ""},
	} {
		got, err := hashFilesIn(t.Context(), ws, "/home/x", tc.patterns, false)
		if err != nil || got != tc.want {
			t.Errorf("%q = %q (%v), want %q", tc.patterns, got, err, tc.want)
		}
	}
	// "before files exist": the directory is not there yet.
	got, err := hashFilesIn(t.Context(), t.TempDir(), "/home/x", []string{"hf/**"}, false)
	if err != nil || got != "" {
		t.Errorf("empty workspace: %q %v", got, err)
	}
}

// The scenario end to end, in expressions, against the recording.
func TestCorpusHashFiles(t *testing.T) {
	compareWithRecording(t, "expr-hashfiles")
}

// Differential test: the official script (Misc/expressionFunc/hashFiles, run by
// node) and this port must agree. Opt in with ERMITE_HASHFILES_JS=<path of the
// script, Misc/layoutbin/hashFiles/index.js of actions/runner>.
func TestHashFilesMatchesOfficialScript(t *testing.T) {
	script := os.Getenv("ERMITE_HASHFILES_JS")
	if script == "" {
		t.Skip("set ERMITE_HASHFILES_JS to Misc/layoutbin/hashFiles/index.js of actions/runner")
	}
	needTools(t, "node")
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"a.txt": "a", "b.go": "b", ".hidden/x": "h", ".dotfile": "d", "src/main.go": "m", "src/util/u.go": "u",
		"src/util/u_test.go": "t", "src/Zeta.go": "z", "src/alpha.GO": "A", "docs/readme.md": "r", "docs/img/p.png": "p",
		"node_modules/pkg/index.js": "n", "weird [name]/f.txt": "w", "a b/c d.txt": "s", "go.mod": "g", "go.sum": "s2",
		"deep/1/2/3/4/f": "deep", "ünï/çode.txt": "u",
	})
	if err := os.Symlink("src", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(ws, "broken")); err != nil {
		t.Fatal(err)
	}
	for _, patterns := range [][]string{
		{"**"}, {"*"}, {"**/*.go"}, {"**/*.go", "!**/*_test.go"}, {"src/**", "!src/util/**"}, {"**/go.sum", "**/go.mod"},
		{"go.mod", "go.sum"}, {"src/*.go"}, {"src/?lpha.GO"}, {"src/[Zz]eta.go"}, {"**/*.{go,md}"}, {"docs"}, {"docs/"}, {"docs/*"},
		{"./src/main.go"}, {"**/.hidden/**"}, {".*"}, {"**/node_modules/**", "src/**"}, {"!src/**", "**/*.go"}, {"nothing"}, {"**/*.md", "# a comment", ""},
		{"weird [[]name]/f.txt"}, {"weird*/*"}, {"a b/*"}, {"src/**/", "docs/**"}, {"ünï/*"}, {"deep/**/f"}, {"**/f"}, {"link/**"}, {"link/main.go"},
		{"broken"}, {"**/*.GO"}, {"src/util/../main.go"}, {"/etc/hostname"}, {"src\\main.go"}, {"**/u*"}, {"*/*/*"}, {"src/**/*", "!src/util"},
	} {
		for _, follow := range []bool{false, true} {
			want := officialHash(t, script, ws, patterns, follow)
			got, err := hashFilesIn(t.Context(), ws, "/home/x", patterns, follow)
			if want.failed {
				if err == nil {
					t.Errorf("%q follow=%v: the script fails (%s), got %q", patterns, follow, want.msg, got)
				}
				continue
			}
			if err != nil || got != want.sum {
				t.Errorf("%q follow=%v: got %q (%v), script %q", patterns, follow, got, err, want.sum)
			}
		}
	}
}

// A symbolic link that leaves the workspace is not followed, whatever the
// script does: the executor may run as root and the workspace is the user's.
func TestHashFilesDoesNotLeaveTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret": "s"})
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{"a.txt": "alpha\n"})
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(ws, "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "leakdir")); err != nil {
		t.Fatal(err)
	}
	only, _ := hashFilesIn(t.Context(), ws, "/h", []string{"a.txt"}, false)
	for _, follow := range []bool{false, true} {
		got, err := hashFilesIn(t.Context(), ws, "/h", []string{"**"}, follow)
		if err != nil || got != only {
			t.Errorf("follow=%v: %q (%v), want the hash of a.txt alone %q", follow, got, err, only)
		}
	}
}

// A matched symbolic link without a target fails the call, like the script.
func TestHashFilesBrokenLinkFails(t *testing.T) {
	ws := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(ws, "broken")); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFilesIn(t.Context(), ws, "/h", []string{"**"}, false); err == nil {
		t.Error("want an error")
	}
	if got, err := hashFilesIn(t.Context(), ws, "/h", []string{"**"}, true); err != nil || got != "" {
		t.Errorf("followed, the link is omitted: %q %v", got, err)
	}
}

// A FIFO in the workspace must not hang the function: opening it for reading
// blocks until a writer comes. It is skipped, directly or through a symbolic
// link, with or without --follow-symbolic-links.
func TestHashFilesSkipsFIFOs(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{"a.txt": "alpha\n"})
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	if err := os.Symlink("pipe", filepath.Join(ws, "pipelink")); err != nil {
		t.Fatal(err)
	}
	only, err := hashFilesIn(t.Context(), ws, "/h", []string{"a.txt"}, false)
	if err != nil || only == "" {
		t.Fatalf("%q %v", only, err)
	}
	for _, tc := range []struct {
		patterns []string
		follow   bool
	}{
		{[]string{"**"}, false}, {[]string{"**"}, true}, {[]string{"pipe"}, false}, {[]string{"pipe"}, true},
		{[]string{"pipelink"}, false}, {[]string{"pipelink"}, true},
	} {
		done := make(chan struct{})
		var got string
		var err error
		go func() {
			defer close(done)
			got, err = hashFilesIn(t.Context(), ws, "/h", tc.patterns, tc.follow)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%q follow=%v: hangs on a FIFO", tc.patterns, tc.follow)
		}
		if err != nil {
			t.Errorf("%q follow=%v: %v", tc.patterns, tc.follow, err)
		}
		if tc.patterns[0] == "**" && got != only {
			t.Errorf("%q follow=%v: %q, want a.txt alone %q", tc.patterns, tc.follow, got, only)
		}
		if tc.patterns[0] != "**" && got != "" {
			t.Errorf("%q follow=%v: %q, want no file", tc.patterns, tc.follow, got)
		}
	}
}

type officialResult struct {
	sum    string
	failed bool
	msg    string
}

func officialHash(t *testing.T, script, ws string, patterns []string, follow bool) officialResult {
	t.Helper()
	cmd := exec.Command("node", script)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "patterns="+strings.Join(patterns, "\n"))
	if follow {
		cmd.Env = append(cmd.Env, "followSymbolicLinks=true")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return officialResult{failed: true, msg: string(out)}
	}
	s := string(out)
	i := strings.Index(s, "__OUTPUT__")
	j := strings.LastIndex(s, "__OUTPUT__")
	if i < 0 || j <= i {
		t.Fatalf("no output: %s", s)
	}
	return officialResult{sum: s[i+10 : j]}
}
