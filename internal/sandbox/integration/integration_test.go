// SPDX-License-Identifier: MIT

// Package integration holds the end-to-end tests of the sandbox drivers with the
// real stub executor: output comes back, exit codes are reported, and nothing
// survives 10 jobs, including killed ones. They need privileges and host
// software, so they are opt-in:
//
//	ERMITE_RUNNER_IT_VM=1         Firecracker + KVM + kernel + base image,
//	                              CAP_NET_ADMIN (TAP, nftables, port 53 on the
//	                              TAP address)
//	ERMITE_RUNNER_IT_CONTAINER=1  a ROOTFUL Docker daemon (DOCKER_HOST) and the
//	                              runner image, CAP_NET_ADMIN for nftables
//
// See README.md ("Integration tests") for the exact commands, including how to
// run the VM test without root inside a user and network namespace.
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/netpolicy"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/drivers"
)

// stubScript is the CI-1 stub job of the plan.
const stubScript = "uname -a; id; cat /etc/os-release; docker info"

func ctxWithLog(t *testing.T) context.Context {
	return zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "info", Format: "console", Output: testWriter{t}}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// buildRunner builds the static runner executable the sandboxes attach.
func buildRunner(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "ermite-runner")
	cmd := exec.Command("go", "build", "-o", exe, "bnema.dev/ermite-runner")
	cmd.Dir = strings.TrimSpace(string(out))
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, b)
	}
	return exe
}

// loadConfig reads the sandbox settings from the environment, like the runner
// does, over a private state directory.
func loadConfig(t *testing.T, mode sandbox.Mode) sandbox.Config {
	t.Helper()
	getenv := func(k string) string {
		switch k {
		case sandbox.EnvIsolation:
			return string(mode)
		case sandbox.EnvStateDir:
			return stateDir(t)
		case sandbox.EnvCapacity:
			return "2"
		case sandbox.EnvJobDisk:
			if v := os.Getenv(k); v != "" {
				return v
			}
			return "4G"
		case sandbox.EnvJobMemory:
			if v := os.Getenv(k); v != "" {
				return v
			}
			return "1024"
		}
		return os.Getenv(k)
	}
	cfg, err := sandbox.LoadConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Executable = buildRunner(t)
	return cfg
}

var stateDirs = map[*testing.T]string{}

func stateDir(t *testing.T) string {
	if d, ok := stateDirs[t]; ok {
		return d
	}
	base := os.Getenv("ERMITE_RUNNER_IT_STATE_DIR")
	if base == "" {
		base = t.TempDir()
	} else {
		base = filepath.Join(base, fmt.Sprintf("it-%d", time.Now().UnixNano()))
		_ = os.MkdirAll(base, 0o755)
		t.Cleanup(func() { _ = os.RemoveAll(base) })
	}
	stateDirs[t] = base
	return base
}

type result struct {
	out, errOut string
	hello       *execpb.Hello
	exit        *execpb.Exit
}

func runJob(ctx context.Context, conn sandbox.Conn, script string) (result, error) {
	var so, se strings.Builder
	hello, exit, err := execstream.Drive(conn, &execpb.Run{Script: script}, nil, func(o *execpb.Output) {
		if o.Stream == execpb.StreamKind_STREAM_KIND_STDERR {
			se.Write(o.Data)
		} else {
			so.Write(o.Data)
		}
	})
	return result{so.String(), se.String(), hello, exit}, err
}

// need skips the test unless every command is available.
func need(t *testing.T, cmds ...string) {
	t.Helper()
	for _, c := range cmds {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("%s not found in PATH", c)
		}
	}
}

// needNetAdmin skips unless the process may create TAP devices and nftables
// tables (root, CAP_NET_ADMIN, or a user+network namespace).
func needNetAdmin(t *testing.T) {
	t.Helper()
	need(t, "ip", "nft")
	const probe = "ertapprobe"
	if out, err := exec.Command("ip", "tuntap", "add", "dev", probe, "mode", "tap").CombinedOutput(); err != nil {
		t.Skipf("cannot create a TAP device (need CAP_NET_ADMIN): %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("ip", "link", "del", "dev", probe).Run()
	if out, err := exec.Command("nft", "list", "tables").CombinedOutput(); err != nil {
		t.Skipf("cannot use nftables (need CAP_NET_ADMIN): %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// audit fails the test when anything of a job survives: network devices, the
// nftables table, state files, processes.
func audit(t *testing.T, stateDir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var left []string
	for {
		left = leftovers(stateDir)
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(left) != 0 {
		t.Fatalf("leftovers after the jobs:\n  %s", strings.Join(left, "\n  "))
	}
}

var tapRe = regexp.MustCompile(`(?m)^\d+: (ertap\d+|erbr\d+)[:@]`)

func leftovers(stateDir string) []string {
	var left []string
	if out, err := exec.Command("ip", "-o", "link", "show").Output(); err == nil {
		for _, m := range tapRe.FindAllStringSubmatch(string(out), -1) {
			left = append(left, "network device "+m[1])
		}
	}
	if out, err := exec.Command("nft", "list", "tables").Output(); err == nil && strings.Contains(string(out), netpolicy.Table) {
		left = append(left, "nftables table "+netpolicy.Table)
	}
	for _, d := range []string{"vm", "vms", "jail", "containers"} {
		_ = filepath.WalkDir(filepath.Join(stateDir, d), func(p string, e os.DirEntry, err error) error {
			if err == nil && p != filepath.Join(stateDir, d) {
				rel, _ := filepath.Rel(stateDir, p)
				left = append(left, "file "+rel)
			}
			return nil
		})
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(args) > 0 && filepath.Base(args[0]) == "firecracker" {
			for i, a := range args {
				if a == "--id" && i+1 < len(args) && sandbox.ValidID(args[i+1]) {
					left = append(left, "process "+p+" "+args[i+1])
				}
			}
		}
	}
	return left
}

func checkStubOutput(t *testing.T, r result, wantSandbox string) {
	t.Helper()
	if r.hello == nil || r.hello.Sandbox != wantSandbox {
		t.Errorf("hello: %v", r.hello)
	}
	for _, want := range []string{"Linux", "uid=", "Ubuntu", "Server Version"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s\nstderr:\n%s", want, r.out, r.errOut)
		}
	}
	if r.exit == nil || r.exit.Code != 0 {
		t.Errorf("stub exit: %v\nstderr: %s", r.exit, r.errOut)
	}
}

// jobs runs the common scenario on a driver: 10 jobs, 2 at a time where
// capacity allows, including a failing job, a job killed from outside, and a
// job abandoned by a "crashed" runner; then audits.
func scenario(t *testing.T, ctx context.Context, cfg sandbox.Config, mode sandbox.Mode, killSandbox func(t *testing.T, sb sandbox.Sandbox)) {
	t.Helper()
	drv, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
	if err != nil {
		t.Fatal(err)
	}
	if drv.Mode() != mode {
		t.Fatalf("driver mode %s, want %s", drv.Mode(), mode)
	}
	if err := drv.Reap(ctx); err != nil {
		t.Fatalf("initial reap: %v", err)
	}

	start := func(id string) (sandbox.Sandbox, sandbox.Conn) {
		sb, err := drv.New()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: id})
		if err != nil {
			_ = sb.Stop(ctx)
			t.Fatalf("job %s: %v", id, err)
		}
		return sb, conn
	}

	// Jobs 1-7: the stub, in pairs (capacity 2) except the last.
	for i := 1; i <= 7; i++ {
		sb, conn := start(fmt.Sprintf("stub-%d", i))
		r, err := runJob(ctx, conn, stubScript)
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
		checkStubOutput(t, r, string(mode))
		if err := sb.Stop(ctx); err != nil {
			t.Fatalf("job %d stop: %v", i, err)
		}
	}

	// Job 8: a failing script: the exit code comes back.
	{
		sb, conn := start("failing")
		r, err := runJob(ctx, conn, "echo before; echo oops >&2; exit 42")
		if err != nil {
			t.Fatal(err)
		}
		if r.exit.Code != 42 || !strings.Contains(r.out, "before") || !strings.Contains(r.errOut, "oops") {
			t.Errorf("failing job: %+v", r)
		}
		if err := sb.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Job 9: killed from outside while running (the VMM or the container dies).
	{
		sb, conn := start("killed")
		done := make(chan error, 1)
		go func() { _, err := runJob(ctx, conn, "echo started; sleep 600"); done <- err }()
		time.Sleep(2 * time.Second)
		killSandbox(t, sb)
		select {
		case err := <-done:
			if err == nil {
				t.Error("a killed sandbox must make the session fail")
			}
		case <-time.After(30 * time.Second):
			t.Error("session did not end after the sandbox was killed")
		}
		if err := sb.Stop(ctx); err != nil {
			t.Fatalf("stop after kill: %v", err)
		}
	}

	// Job 10: the runner itself dies mid-job (no Stop, driver closed); the next
	// start reaps everything.
	{
		_, conn := start("abandoned")
		go func() { _, _ = runJob(ctx, conn, "sleep 600") }()
		time.Sleep(2 * time.Second)
		_ = drv.Close() // what a crash does: locks and listeners vanish, the sandbox stays
		_ = conn.Close()
		drv2, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = drv2.Close() }()
		if err := drv2.Reap(ctx); err != nil {
			t.Fatalf("reap after crash: %v", err)
		}
	}
	audit(t, cfg.StateDir)
}
