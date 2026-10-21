// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/registryproxy"
)

type registryLogs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *registryLogs) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *registryLogs) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
func (b *registryLogs) reset()       { b.mu.Lock(); defer b.mu.Unlock(); b.b.Reset() }

func TestDockerHubRegistryAcrossJobs(t *testing.T) {
	image := needE2E(t)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get("https://registry-1.docker.io/v2/")
	if err != nil {
		t.Skipf("Docker Hub unreachable: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusOK {
		t.Skipf("Docker Hub unavailable: HTTP %d", resp.StatusCode)
	}
	var logs registryLogs
	p, err := registryproxy.New(registryproxy.Config{Dir: t.TempDir(), Max: 256 << 20, Log: zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs})})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := cachesvc.New(cachesvc.Config{Dir: t.TempDir(), Registry: p})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.CloseAll() }()
	d := newCacheDriver(t, image, svc)
	probe := runJob(t, d, jobSpec(`on: push
jobs:
  j:
    runs-on: x
    steps:
      - run: if docker info >/dev/null 2>&1; then echo nested-dockerd-ready; else echo nested-dockerd-unavailable; fi
`), nil)
	if !strings.Contains(strings.Join(probe.Logs, "\n"), "nested-dockerd-ready") {
		t.Skip("nested dockerd unavailable in sandbox; rootless Docker harness cannot run job containers")
	}
	for i := 0; i < 2; i++ {
		logs.reset()
		spec := jobSpec(`on: push
jobs:
  j:
    runs-on: x
    container: alpine:3
    steps:
      - run: echo alpine-job
`)
		// A registry-only job: the host sends the port and no token.
		spec.CachePort = cachesvc.DefaultPort
		s := runJob(t, d, spec, nil)
		if s.Result == nil || s.Result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS {
			t.Fatalf("pull job %d: %v\n%s", i, s.Result, strings.Join(s.Logs, "\n"))
		}
		all := logs.text()
		manifestHit := false
		for _, line := range strings.Split(all, "\n") {
			var event struct {
				Message string `json:"message"`
				Hit     bool   `json:"hit"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && event.Message == "registry manifest" && event.Hit {
				manifestHit = true
			}
		}
		if i == 1 && !manifestHit {
			t.Fatalf("second job missing manifest hit:\n%s", all)
		}
		t.Logf("pull job %d proxy logs:\n%s", i+1, all)
		if i == 1 && strings.Contains(all, `"message":"registry blob"`) {
			t.Fatalf("second job fetched upstream blobs:\n%s", all)
		}
	}
	// The mirror argv is unconditional: a job without the runtime channel
	// must still pull directly from Docker Hub.
	off := runJob(t, d, jobSpec(`on: push
jobs:
  j:
    runs-on: x
    container: alpine:3
    steps:
      - run: echo direct-pull
`), nil)
	if off.Result == nil || off.Result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS {
		t.Fatalf("proxy-off pull: %v\n%s", off.Result, strings.Join(off.Logs, "\n"))
	}
}
