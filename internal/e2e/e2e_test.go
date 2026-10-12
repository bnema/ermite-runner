// SPDX-License-Identifier: MIT

// Package e2e runs jobs through the real executor in a real sandbox. The tests
// are opt-in because they need Docker (and, for the VM, KVM and Firecracker):
//
//	ERMITE_RUNNER_IT_E2E=1 ERMITE_RUNNER_TEST_IMAGE=ermite-runner-ubuntu-24.04:dev go test ./internal/e2e
//
// On a workstation with a rootless Docker daemon the container driver is used
// with AllowRootless (a test-only option): the jobs run as in production except
// that the nested dockerd of the image cannot start, which these tests do not
// need. The production paths that need root (jailer, rootful Docker, sysbox)
// are not covered here.
package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/container"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

func needE2E(t *testing.T) string {
	t.Helper()
	if os.Getenv("ERMITE_RUNNER_IT_E2E") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_E2E=1 and ERMITE_RUNNER_TEST_IMAGE (needs Docker; see the package comment)")
	}
	image := os.Getenv("ERMITE_RUNNER_TEST_IMAGE")
	if image == "" {
		t.Fatal("set ERMITE_RUNNER_TEST_IMAGE to the runner image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not found")
	}
	return image
}

// buildRunner compiles the static runner binary that is attached to the sandbox.
func buildRunner(t *testing.T, dir string) string {
	t.Helper()
	exe := filepath.Join(dir, "ermite-runner")
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", exe, "bnema.dev/ermite-runner")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Dir = strings.TrimSpace(string(root))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the runner: %v\n%s", err, out)
	}
	return exe
}

// newDriver starts the container driver with test-only settings: no firewall
// plumbing, no loop mounts, a rootless daemon allowed.
func newDriver(t *testing.T, image string) sandbox.Driver {
	t.Helper()
	dir := t.TempDir()
	cfg := sandbox.Config{
		Isolation: sandbox.Container, StateDir: filepath.Join(dir, "state"), Capacity: 2, Image: image,
		JobCPUs: 2, JobMemoryMiB: 1024, JobDiskMiB: 2048, UIDBase: 900000,
		Network: netPrefix("10.214.0.0/16"), Executable: buildRunner(t, dir), DNSPort: 0,
		LockFile: filepath.Join(dir, "host.lock"), JobPids: 4096,
	}
	empty := filepath.Join(dir, "mountinfo")
	_ = os.WriteFile(empty, nil, 0o644)
	run := rootlessMountRunner{image: image, mountinfo: empty, mu: &sync.Mutex{}}
	d, err := container.New(context.Background(), cfg, container.Options{
		Net: sandboxtest.NewPlumbing(), AllowRootless: true, ConnectTimeout: 90 * time.Second, Run: run,
		Mounter: &disk.Mounter{Run: run, Mountinfo: empty},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// rootlessMountRunner stands in for mount and umount, which need root. The
// scratch directory stays a plain directory; "mount" records it in a fake
// mountinfo file so that the driver believes it is mounted and calls "umount"
// at the end. On a rootless daemon the files that the runner user (uid 1001 in
// the container) creates belong to a subordinate uid that the host user cannot
// delete, so "umount" empties the directory from inside a container, where
// root can. With the real loop mount the scratch image is simply unmounted and
// deleted.
type rootlessMountRunner struct {
	image     string
	mountinfo string
	mu        *sync.Mutex
}

func (r rootlessMountRunner) Run(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	switch name {
	case "mount":
		r.mu.Lock()
		defer r.mu.Unlock()
		f, err := os.OpenFile(r.mountinfo, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		_, err = fmt.Fprintf(f, "1 1 0:1 / %s rw - ext4 fake rw\n", args[len(args)-1])
		return nil, err
	case "umount":
		dir := args[len(args)-1]
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "sh", "-v", dir+":/d", r.image,
			"-c", "rm -rf /d/* /d/.[!.]* 2>/dev/null; true").CombinedOutput()
		r.mu.Lock()
		defer r.mu.Unlock()
		b, _ := os.ReadFile(r.mountinfo)
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" && !strings.Contains(l, " "+dir+" ") {
				keep = append(keep, l)
			}
		}
		_ = os.WriteFile(r.mountinfo, []byte(strings.Join(keep, "\n")+"\n"), 0o644)
		return out, err
	}
	return sandboxtest.NoMountRunner{}.Run(ctx, in, name, args...)
}

// session is the outcome of one job driven through the stream.
type session struct {
	Steps   []string
	Concl   map[string]execpb.StepConclusion
	Logs    []string // text of every log line, in order
	LogStep []int32
	Result  *execpb.JobResult
	Exit    *execpb.Exit
}

// runJob starts a sandbox, drives the job and stops the sandbox.
func runJob(t *testing.T, d sandbox.Driver, spec *execpb.JobSpec, cancel <-chan *execpb.Cancel) *session {
	t.Helper()
	sb, err := d.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sb.Stop(context.Background()); err != nil {
			t.Errorf("stopping the sandbox: %v", err)
		}
	}()
	conn, err := sb.Start(context.Background(), sandbox.JobSpec{JobID: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{Concl: map[string]execpb.StepConclusion{}}
	_, exit, err := execstream.DriveJob(conn, &execpb.Run{Job: spec}, cancel, execstream.JobHandler{
		StepStarted: func(x *execpb.StepStarted) { s.Steps = append(s.Steps, x.Name) },
		StepCompleted: func(x *execpb.StepCompleted) {
			s.Concl[x.Name] = x.Conclusion
		},
		Log:       func(l *execpb.Log) { s.Logs = append(s.Logs, l.Text); s.LogStep = append(s.LogStep, l.Step) },
		JobResult: func(r *execpb.JobResult) { s.Result = r },
	})
	if err != nil {
		t.Fatalf("driving the job: %v", err)
	}
	s.Exit = exit
	return s
}

func jobSpec(yaml string) *execpb.JobSpec {
	return &execpb.JobSpec{
		WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: yaml, JobKey: "j", Repo: "hello",
		Contexts: `{"github":{"repository":"octo/hello","repository_owner":"octo","event_name":"push","job":"j","ref":"refs/heads/main","ref_name":"main","ref_type":"branch","sha":"0123456789abcdef0123456789abcdef01234567","run_id":"1","run_number":"1","run_attempt":"1","server_url":"https://ermite.example","workflow":"w","actor":"octo","event":{"ref":"refs/heads/main"}}}`,
		Matrix:   "{}", RunnerName: "e2e", RunnerVersion: "test", RunnerEnvironment: "self-hosted", TimeoutMinutes: 10,
	}
}

func TestContainerRunsAJobAsTheRunnerUser(t *testing.T) {
	image := needE2E(t)
	d := newDriver(t, image)
	s := runJob(t, d, jobSpec(`on: push
jobs:
  j:
    runs-on: x
    outputs:
      who: ${{ steps.id.outputs.who }}
    steps:
      - id: id
        run: |
          echo "who=$(id -un):$(id -u):$(id -g)" >> "$GITHUB_OUTPUT"
          echo "home=$HOME pwd=$PWD"
          echo "ws=$GITHUB_WORKSPACE temp=$RUNNER_TEMP tool=$RUNNER_TOOL_CACHE"
          touch "$RUNNER_TEMP/t" "$GITHUB_WORKSPACE/w" "$HOME/h"
          echo "script=$0"
      - name: env and path
        run: |
          mkdir -p "$RUNNER_TEMP/bin" && printf '#!/bin/sh\necho from-path\n' > "$RUNNER_TEMP/bin/mytool" && chmod +x "$RUNNER_TEMP/bin/mytool"
          echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"
          echo "FOO=bar" >> "$GITHUB_ENV"
      - run: mytool; echo "FOO=$FOO CI=$CI GITHUB_ACTIONS=$GITHUB_ACTIONS ERMITE_CONNECT=${ERMITE_CONNECT-unset}"
      - name: python
        shell: python3 {0}
        run: print("py", 6*7)
`), nil)
	if s.Result == nil || s.Result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || s.Exit.Code != 0 {
		t.Fatalf("result %v exit %v\n%s", s.Result, s.Exit, strings.Join(s.Logs, "\n"))
	}
	if got := s.Result.Outputs["who"]; got != "runner:1001:1001" {
		t.Errorf("steps run as %q, want runner:1001:1001", got)
	}
	all := strings.Join(s.Logs, "\n")
	for _, want := range []string{
		"home=/home/runner pwd=/home/runner/work/hello/hello",
		"ws=/home/runner/work/hello/hello temp=/home/runner/work/_temp tool=/opt/hostedtoolcache",
		"from-path", "FOO=bar CI=true GITHUB_ACTIONS=true ERMITE_CONNECT=unset", "py 42",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q\n%s", want, all)
		}
	}
	if !strings.HasPrefix(strings.Join(s.Steps, "|"), "Set up job|Run echo") || s.Steps[len(s.Steps)-1] != "Complete job" {
		t.Errorf("steps %v", s.Steps)
	}
}

func TestContainerCancelStopsTheJobAndOrphans(t *testing.T) {
	image := needE2E(t)
	d := newDriver(t, image)
	cancel := make(chan *execpb.Cancel, 1)
	time.AfterFunc(4*time.Second, func() { cancel <- &execpb.Cancel{Reason: "test", Kind: execpb.CancelKind_CANCEL_KIND_USER} })
	start := time.Now()
	s := runJob(t, d, jobSpec(`on: push
jobs:
  j:
    runs-on: x
    steps:
      - run: |
          (sleep 300 &)
          trap 'echo got-int; exit 130' INT
          echo ready
          while :; do sleep 0.2; done
      - if: always()
        run: echo cleanup-ran
`), cancel)
	if s.Result == nil || s.Result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_CANCELLED {
		t.Fatalf("result %v\n%s", s.Result, strings.Join(s.Logs, "\n"))
	}
	all := strings.Join(s.Logs, "\n")
	if !strings.Contains(all, "got-int") || !strings.Contains(all, "cleanup-ran") {
		t.Errorf("SIGINT or the always() step missing:\n%s", all)
	}
	// "Complete job" kills the background sleep that the step left behind.
	if !strings.Contains(all, "Terminate orphan process: pid (") || !strings.Contains(all, ") (sleep)") {
		t.Errorf("orphan cleanup missing:\n%s", all)
	}
	if time.Since(start) > 40*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}
