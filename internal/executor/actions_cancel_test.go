// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"strings"
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
