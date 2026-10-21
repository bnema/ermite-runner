// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite-runner/internal/host"
	"bnema.dev/ermite-runner/internal/host/hosttest"
	"github.com/bnema/zerowrap"
)

type timingWriter struct {
	mu    sync.Mutex
	lines [][]byte
}

func (w *timingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, append([]byte(nil), p...))
	return len(p), nil
}

func TestJobTiming(t *testing.T) {
	image := needE2E(t)
	drv := newDriver(t, image)
	srv := hosttest.New()
	defer srv.Close()
	srv.Queue(&ermite.Job{JobId: "timing", RunId: 1, Attempt: 1, JobToken: "ermitejob_timing", WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: "name: timing\non: push\njobs:\n  main:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo timing\n", JobKey: "main", Matrix: "{}", Contexts: chainContexts("main"), Repo: "hello", TimeoutMinutes: 2})
	r := &host.Runner{
		Cfg: host.Config{URL: srv.URL, Name: "timing", Capacity: 1, Version: "test"}, Driver: drv,
		Poll: host.NewClient(srv.URL, hosttest.RunnerToken, http.DefaultTransport),
		Job: func(tok string) ermiteconnect.RunnerServiceClient {
			return host.NewClient(srv.URL, tok, http.DefaultTransport)
		},
		Timing: host.Timing{LogFlush: 20 * time.Millisecond, NoJobPause: 100 * time.Millisecond},
	}
	var logs timingWriter
	ctx, cancel := context.WithCancel(zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "info", Format: "json", Output: &logs})))
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Minute):
		cancel()
		t.Fatal("job did not complete")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("runner did not stop")
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, line := range logs.lines {
		var fields map[string]any
		if json.Unmarshal(line, &fields) != nil || fields["message"] != "job finished" {
			continue
		}
		for _, key := range []string{"sandbox_ms", "guest_dockerd_ms"} {
			value, ok := fields[key].(float64)
			if !ok || value < 0 {
				t.Errorf("%s missing or negative: %v", key, fields)
			}
		}
		return
	}
	t.Fatal("no job finished log")
}
