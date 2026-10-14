// SPDX-License-Identifier: MIT

package cachesvc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envSetupGoDist is the dist directory of a checkout of actions/setup-go
// (dist/setup/index.js and dist/cache-save/index.js bundle the real
// @actions/cache, 6.3.0 at the time of writing). TestSetupGoCacheRoundTrip runs
// them against the service: the protocol as the toolkit speaks it, not as the
// other tests of this package spell it. It needs node, tar, zstd and the
// checkout, and no network; without them the test is skipped.
const envSetupGoDist = "ERMITE_TEST_SETUP_GO_DIST"

// TestSetupGoCacheRoundTrip is AC7's "second run of a setup-go cache: true
// workflow logs a cache hit and restores", with the real action code: run 1
// misses, the post step saves, run 2 hits and restores the files; jobs of
// another repository or of another ref miss.
func TestSetupGoCacheRoundTrip(t *testing.T) {
	dist := os.Getenv(envSetupGoDist)
	if dist == "" {
		t.Skip(envSetupGoDist + " is not set (a built checkout of actions/setup-go)")
	}
	for _, bin := range []string{"node", "tar", "zstd", "go", "sed"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not found")
		}
	}
	r := newRig(t, 1<<30)
	root := t.TempDir()
	work := filepath.Join(root, "ws")
	modcache, gocache := filepath.Join(root, "gomod"), filepath.Join(root, "gocache")
	for _, d := range []string{work, filepath.Join(modcache, "pkg"), gocache, filepath.Join(root, "tmp"), filepath.Join(root, "tc"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cached := map[string]string{
		filepath.Join(modcache, "pkg", "a.txt"): "module cache file\n",
		filepath.Join(gocache, "blob"):          strings.Repeat("0123456789abcdef", 1<<16),
	}
	write := func() {
		t.Helper()
		for p, c := range cached {
			if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	remove := func() {
		for p := range cached {
			_ = os.Remove(p)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "go.sum"), []byte("sum\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write()

	// A "go" without the "-X:…" suffix that some distributions add to the
	// version: the setup-go key then has no ':' (a quirk of the local toolchain,
	// unrelated to the service).
	realGo, _ := exec.LookPath("go")
	shim := "#!/bin/sh\n" + realGo + " \"$@\" | sed 's/-X:[a-z0-9]*//g'\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	// step runs one entry point of the action as the job would run it.
	step := func(j *job, script string, extra ...string) (out, state, output string) {
		t.Helper()
		for _, f := range []string{"state", "output", "env", "path"} {
			if err := os.WriteFile(filepath.Join(root, f+".file"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", filepath.Join(dist, script, "index.js"))
		cmd.Dir = work
		cmd.Env = append([]string{
			"PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH"), "HOME=" + root,
			"GITHUB_SERVER_URL=http://forge.test", "GITHUB_ACTIONS=true", "GITHUB_WORKSPACE=" + work,
			"RUNNER_OS=Linux", "ImageOS=ubuntu24", "RUNNER_TEMP=" + filepath.Join(root, "tmp"), "RUNNER_TOOL_CACHE=" + filepath.Join(root, "tc"),
			// What the executor sets for node actions (executor.addRuntimeEnv).
			"ACTIONS_CACHE_URL=" + j.r.srv.URL + "/", "ACTIONS_RUNTIME_URL=" + j.r.srv.URL + "/", "ACTIONS_RUNTIME_TOKEN=" + j.token,
			"GITHUB_STATE=" + filepath.Join(root, "state.file"), "GITHUB_OUTPUT=" + filepath.Join(root, "output.file"),
			"GITHUB_ENV=" + filepath.Join(root, "env.file"), "GITHUB_PATH=" + filepath.Join(root, "path.file"),
			"GOMODCACHE=" + modcache, "GOCACHE=" + gocache, "GOPATH=" + filepath.Join(root, "gopath"),
			"INPUT_CACHE=true", "INPUT_CHECK-LATEST=false", "INPUT_GO-VERSION=", "INPUT_CACHE-DEPENDENCY-PATH=" + filepath.Join(work, "go.sum"),
		}, extra...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		cmd.WaitDelay = 5 * time.Second
		// Run is synchronous; the output is read after it returns.
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", script, err, buf.String())
		}
		st, _ := os.ReadFile(filepath.Join(root, "state.file"))
		op, _ := os.ReadFile(filepath.Join(root, "output.file"))
		return buf.String(), string(st), string(op)
	}
	// value reads "name<<delimiter\nvalue\ndelimiter" of a file command file.
	value := func(file, name string) string {
		lines := strings.Split(file, "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, name+"<<") && i+1 < len(lines) {
				return lines[i+1]
			}
		}
		return ""
	}

	a := r.job("run1", "bnema/app", main_, main_)
	out, state, output := step(a, "setup")
	if !strings.Contains(out, "Cache is not found") || value(output, "cache-hit") != "false" {
		t.Fatalf("run 1 must miss:\n%s", out)
	}
	key := value(state, "CACHE_KEY")
	if !strings.HasPrefix(key, "setup-go-Linux-") {
		t.Fatalf("state: %q", state)
	}
	out, _, _ = step(a, "cache-save", "STATE_CACHE_KEY="+key)
	if !strings.Contains(out, "Cache saved successfully") {
		t.Fatalf("save:\n%s", out)
	}
	if n, _, _ := r.svc.store.stats(); n != 1 {
		t.Fatalf("entries after save: %d", n)
	}

	// Run 2: a new job of the same repository and ref, with the caches gone.
	remove()
	out, _, output = step(r.job("run2", "bnema/app", main_, main_), "setup")
	if !strings.Contains(out, "Cache restored from key: "+key) || value(output, "cache-hit") != "true" {
		t.Fatalf("run 2 must hit:\n%s", out)
	}
	for p, want := range cached {
		if got, err := os.ReadFile(p); err != nil || string(got) != want {
			t.Errorf("%s not restored (%v)", p, err)
		}
	}

	// Another repository, and another ref that does not read main, restore nothing.
	for name, j := range map[string]*job{
		"another repository": r.job("run3", "bnema/other", main_, main_),
		"another ref":        r.job("run4", "bnema/app", feat, feat),
	} {
		remove()
		out, _, output = step(j, "setup")
		if !strings.Contains(out, "Cache is not found") || value(output, "cache-hit") != "false" {
			t.Fatalf("%s must miss:\n%s", name, out)
		}
		if _, err := os.Stat(filepath.Join(gocache, "blob")); err == nil {
			t.Fatalf("%s: files were restored", name)
		}
	}
}
