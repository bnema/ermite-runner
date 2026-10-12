// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/cmdrun"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

// TestDockerAPIWiring runs the real Docker Engine calls (network with a named
// bridge, labelled container, bind mounts, wait, removal, reaping by
// label) against whatever daemon DOCKER_HOST points at, including a rootless
// one. It does not test isolation (a rootless daemon cannot run the nested
// dockerd): the integration test in internal/sandbox/integration does.
//
//	ERMITE_RUNNER_IT_DOCKER_API=1 ERMITE_RUNNER_TEST_IMAGE=<image> go test -run DockerAPIWiring ./internal/sandbox/container
func TestDockerAPIWiring(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_DOCKER_API") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_DOCKER_API=1 (needs a reachable Docker daemon)")
	}
	image := os.Getenv("ERMITE_RUNNER_TEST_IMAGE")
	if image == "" {
		t.Fatal("set ERMITE_RUNNER_TEST_IMAGE to an image with /bin/sh (e.g. the runner image)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI needed for the verification")
	}
	// A static test executable that plays the executor: the entrypoint of the
	// container is "<exe> init", and init runs "<exe> exec"... replaced here by a
	// tiny shell executor that speaks the stream through a Go helper binary.
	dir := t.TempDir()
	helper := filepath.Join(dir, "ermite-runner")
	build := exec.Command("go", "build", "-o", helper, "bnema.dev/ermite-runner")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}

	plumb := sandboxtest.NewPlumbing()
	cfg := sandbox.Config{
		Isolation: sandbox.Container, StateDir: filepath.Join(dir, "state"), Capacity: 1, Image: image,
		JobCPUs: 1, JobMemoryMiB: 512, JobDiskMiB: 1024, UIDBase: 900000,
		Network: mustPrefix("10.213.0.0/16"), Executable: helper, DNSPort: 0,
		LockFile: filepath.Join(dir, "host.lock"), JobPids: 4096,
	}
	ctx := context.Background()
	// Loop mounts need root: replace them by plain directories (mkfs still runs).
	run := noMountRunner{}
	empty := filepath.Join(dir, "mountinfo")
	_ = os.WriteFile(empty, nil, 0o644)
	d, err := New(ctx, cfg, Options{Net: plumb, UnsafeAllowRootlessForTests: true, ConnectTimeout: 60 * time.Second, Run: run,
		Mounter: &disk.Mounter{Run: run, Mountinfo: empty}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Reap(ctx); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		sb, err := d.New()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "api-wiring"})
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
		var out strings.Builder
		hello, exit, err := execstream.Drive(conn, &execpb.Run{Script: "echo wired; id -u; exit 4"}, nil, func(o *execpb.Output) { out.Write(o.Data) })
		if err != nil {
			t.Fatal(err)
		}
		if hello.Sandbox != "container" || exit.Code != 4 || !strings.HasPrefix(out.String(), "wired\n") {
			t.Fatalf("hello=%v exit=%v out=%q", hello, exit, out.String())
		}
		// While running, Docker knows our objects by label.
		if i == 0 {
			if ids := dockerLines(t, "ps", "-aq", "--filter", "label="+sandbox.LabelJob); len(ids) != 1 {
				t.Fatalf("expected one labelled container, got %v", ids)
			}
			if n := dockerLines(t, "network", "ls", "-q", "--filter", "label="+sandbox.LabelJob); len(n) != 1 {
				t.Fatalf("expected one labelled network, got %v", n)
			}
		}
		if err := sb.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"ps", "-aq", "--filter", "label=" + sandbox.LabelJob},
		{"network", "ls", "-q", "--filter", "label=" + sandbox.LabelJob},
	} {
		if l := dockerLines(t, args...); len(l) != 0 {
			t.Fatalf("docker %v left %v", args, l)
		}
	}
	if plumb.Live() != 0 {
		t.Fatal("firewall attachments leaked")
	}
}

// noMountRunner runs real commands except mount and umount, which succeed
// without doing anything.
type noMountRunner struct{}

func (noMountRunner) Run(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	if name == "mount" || name == "umount" {
		return nil, nil
	}
	return cmdrun.Exec{}.Run(ctx, in, name, args...)
}

func dockerLines(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.Fields(string(out))
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
