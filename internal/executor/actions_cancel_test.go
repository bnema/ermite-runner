// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// GO-006: the image setup of a local composite action (pull, retry sleep) stops
// with the step.
func TestLocalCompositeSetupIsCancellable(t *testing.T) {
	needTools(t, "bash")
	dir := t.TempDir()
	pulls := filepath.Join(dir, "pulls")
	script := "#!/bin/sh\necho x >> " + pulls + "\necho ready\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewCanceller()
	started := make(chan struct{})
	go func() {
		<-started
		c.Cancel(CancelUser)
	}()
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	writeFiles(t, layout.Workspace, files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: docker://alpine:3\n"})
	rec := newRecorder()
	rep := &signalReporter{recorder: rec, started: started, match: "ready"}
	done := make(chan Result, 1)
	go func() {
		done <- Run(c, Config{
			WorkflowYAML: []byte(wfHead + "    steps:\n      - uses: ./c\n      - if: always()\n        run: echo cleanup-ran\n"),
			JobKey:       "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rep,
			BaseEnv:   []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home},
			TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond, WaitDelay: 500 * time.Millisecond,
			Actions: ActionsConfig{DockerPath: filepath.Join(dir, "docker"), Backoff: func() time.Duration { return time.Hour }},
		})
	}()
	select {
	case res := <-done:
		if res.Conclusion != ConclusionCancelled {
			t.Fatalf("%+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the retry sleep of the pull ignored the cancellation")
	}
	if b, _ := os.ReadFile(pulls); strings.Count(string(b), "x") != 1 {
		t.Errorf("no retry after the cancellation: %q", b)
	}
	if got := rec.step(t, "Run ./c").Conclusion; got != ConclusionCancelled {
		t.Errorf("step %s", got)
	}
	if got := rec.step(t, "Run echo cleanup-ran").Conclusion; got != ConclusionSuccess {
		t.Errorf("always() step %s", got)
	}
}

// GO-014: what the timeout or the cancellation of a step breaks while a local
// composite action is prepared (a resolution, a download, an image pull and its
// retry sleep) ends the step as the cancellation of the step does: the timeout
// message and a failed step for timeout-minutes, a cancelled step for a user
// cancel, and no stray "The operation was canceled.".
func TestPreparationInterruptedByTheStepTimeout(t *testing.T) {
	skew := -(60*time.Second - 1500*time.Millisecond)
	leaf := files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: o/leaf@v1\n"}
	docker := files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: docker://alpine:3\n"}
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hang:
		}
	}))
	defer slow.Close()
	defer close(hang)
	failDocker := filepath.Join(t.TempDir(), "docker")
	writeFiles(t, filepath.Dir(failDocker), files{"docker": "#!/bin/sh\necho pull failed\nexit 1\n"})

	for name, tc := range map[string]struct {
		tree  files
		tweak func(*Config)
	}{
		"blocked resolver": {leaf, func(c *Config) {
			c.Actions.Resolver = func(ctx context.Context, _ []string) ([]ResolvedAction, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}},
		"blocked download": {leaf, func(c *Config) {
			c.Actions.Resolver = func(context.Context, []string) ([]ResolvedAction, error) {
				return []ResolvedAction{{Ref: "o/leaf@v1", NameWithOwner: "o/leaf", ResolvedSHA: strings.Repeat("a", 40), TarballURL: slow.URL + "/t"}}, nil
			}
		}},
		"docker pull retry sleep": {docker, func(c *Config) {
			c.Actions.DockerPath = failDocker
			c.Actions.Backoff = func() time.Duration { return time.Hour }
		}},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			rr, res := actionResults(t, tc.tree, "      - name: comp\n        timeout-minutes: 1\n        uses: ./c\n      - name: after\n        if: always()\n        run: echo after-ran\n", func(c *Config) {
				c.StepTimeoutSkew = skew
				tc.tweak(c)
			})
			if time.Since(start) > 15*time.Second {
				t.Fatalf("the preparation was not interrupted: %v", time.Since(start))
			}
			comp := rr.step(t, "comp")
			log := logOf(comp)
			if res.Conclusion != ConclusionFailure || comp.Conclusion != ConclusionFailure {
				t.Fatalf("job %s, step %s\n%s", res.Conclusion, comp.Conclusion, log)
			}
			if !strings.Contains(log, "##[error]The action 'comp' has timed out after 1 minutes.") {
				t.Errorf("no timeout message:\n%s", log)
			}
			if strings.Contains(log, "The operation was canceled.") {
				t.Errorf("a stray cancel text:\n%s", log)
			}
			if rr.step(t, "after").Conclusion != ConclusionSuccess {
				t.Error("the job is not cancelled by a step timeout")
			}
		})
	}
}

func TestPreparationInterruptedByAUserCancel(t *testing.T) {
	needTools(t, "bash")
	entered := make(chan struct{})
	var once sync.Once
	home := t.TempDir()
	layout := NewLayout(home, "hello", home+"/tc")
	writeFiles(t, layout.Workspace, files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: o/leaf@v1\n"})
	c := NewCanceller()
	go func() {
		<-entered
		c.Cancel(CancelUser)
	}()
	rec := newRecorder()
	done := make(chan Result, 1)
	go func() {
		done <- Run(c, Config{
			WorkflowYAML: []byte(wfHead + "    steps:\n      - name: comp\n        uses: ./c\n      - name: cleanup\n        if: always()\n        run: echo cleanup-ran\n"),
			JobKey:       "j", ContextsJSON: testGitHubContext("j"), Layout: &layout, Reporter: rec,
			BaseEnv:   []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home},
			TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond, WaitDelay: 500 * time.Millisecond,
			Actions: ActionsConfig{Resolver: func(ctx context.Context, _ []string) ([]ResolvedAction, error) {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return nil, ctx.Err()
			}},
		})
	}()
	select {
	case res := <-done:
		if res.Conclusion != ConclusionCancelled {
			t.Fatalf("%+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the resolution ignored the cancellation")
	}
	if got := rec.step(t, "comp").Conclusion; got != ConclusionCancelled {
		t.Errorf("step %s, want cancelled", got)
	}
	if got := rec.step(t, "cleanup").Conclusion; got != ConclusionSuccess {
		t.Errorf("always() step %s", got)
	}
}

// A genuine error of the preparation still fails the step.
func TestPreparationErrorStillFailsTheStep(t *testing.T) {
	tree := files{"c/action.yml": "name: c\nruns:\n  using: composite\n  steps:\n    - uses: o/leaf@v1\n"}
	rr, res := actionResults(t, tree, "      - name: comp\n        uses: ./c\n", func(c *Config) {
		c.Actions.Resolver = func(context.Context, []string) ([]ResolvedAction, error) {
			return []ResolvedAction{{Ref: "o/leaf@v1", Error: "Unable to resolve action `o/leaf@v1`, repository not found: `o/leaf`"}}, nil
		}
	})
	comp := rr.step(t, "comp")
	if res.Conclusion != ConclusionFailure || comp.Conclusion != ConclusionFailure || !strings.Contains(logOf(comp), "##[error]Unable to resolve action `o/leaf@v1`, repository not found: `o/leaf`") {
		t.Errorf("%s %s\n%s", res.Conclusion, comp.Conclusion, logOf(comp))
	}
}
