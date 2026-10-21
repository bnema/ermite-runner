// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
)

func TestContainerSetupGoFindsToolcache(t *testing.T) {
	image := needE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Distinguish a missing cache from an unavailable image or Docker daemon.
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "sh", image, "-c", "if test -d /opt/hostedtoolcache/go/1.27.1; then echo present; else echo absent; fi").CombinedOutput()
	if err != nil {
		t.Fatalf("probe toolcache: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) == "absent" {
		t.Skip("test image has no Go 1.27.1 toolcache")
	}
	d := newDriver(t, image)
	sb, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sb.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "e2e-toolcache"})
	if err != nil {
		t.Fatal(err)
	}
	spec := jobSpec(`on: push
jobs:
  j:
    runs-on: x
    steps:
      - uses: actions/setup-go@v7
        with:
          go-version: '1.27.1'
          cache: false
      - run: go version
`)
	var logs []string
	var result *execpb.JobResult
	_, exit, err := execstream.DriveJob(conn, &execpb.Run{Job: spec}, nil, execstream.JobHandler{
		Resolve: func(_ context.Context, r *execpb.ResolveRequest) *execpb.ResolveResponse {
			resp := &execpb.ResolveResponse{}
			for _, ref := range r.Uses {
				if ref != "actions/setup-go@v7" {
					return &execpb.ResolveResponse{Error: "unexpected action: " + ref}
				}
				resp.Actions = append(resp.Actions, &execpb.ResolvedAction{Ref: ref, NameWithOwner: "actions/setup-go", ResolvedSha: "v7", TarballUrl: "https://codeload.github.com/actions/setup-go/tar.gz/refs/tags/v7"})
			}
			return resp
		},
		Log:       func(l *execpb.Log) { logs = append(logs, l.Text) },
		JobResult: func(r *execpb.JobResult) { result = r },
	})
	all := strings.Join(logs, "\n")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, all)
	}
	if result == nil || result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || exit == nil || exit.Code != 0 {
		t.Fatalf("result %v exit %v\n%s", result, exit, all)
	}
	for _, want := range []string{"Found in cache", "go version go1.27.1"} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q\n%s", want, all)
		}
	}
}
