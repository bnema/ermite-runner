// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/externals"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/drivers"
)

func TestVMStubExecutor(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_VM") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_VM=1 (needs KVM, firecracker, a kernel and a base image; see README)")
	}
	need(t, "mkfs.ext4")
	needNetAdmin(t)
	ctx := ctxWithLog(t)
	cfg := loadConfig(t, sandbox.VM)
	if why := sandbox.VMUnavailable(cfg, sandbox.Prober{}); len(why) > 0 {
		t.Fatalf("vm isolation is not available here: %s", strings.Join(why, "; "))
	}

	scenario(t, ctx, cfg, sandbox.VM, func(t *testing.T, sb sandbox.Sandbox) {
		// Kill every VMM process of ours (found like Reap finds them: by id).
		pids := vmmPIDs()
		if len(pids) == 0 {
			t.Fatal("no VMM process to kill")
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// The guest must reach DNS on its gateway and nothing else on the host.
func TestVMNetworkPolicy(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_VM") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_VM=1")
	}
	need(t, "mkfs.ext4")
	needNetAdmin(t)
	ctx := ctxWithLog(t)
	cfg := loadConfig(t, sandbox.VM)
	drv, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()
	_ = drv.Reap(ctx)
	sb, err := drv.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Stop(context.Background()) }()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "netpolicy"})
	if err != nil {
		t.Fatal(err)
	}
	// 10.200.0.1 is the gateway of slot 0.
	script := `
probe() { timeout 4 bash -c "echo > /dev/tcp/$1/$2" >/dev/null 2>&1; echo "$1:$2 rc=$?"; }
probe 10.200.0.1 22        # host SSH-like port: must be DROPPED (timeout, rc=124), not refused
probe 10.200.0.1 8080
probe 169.254.169.254 80   # cloud metadata
probe 100.64.0.1 80        # CGNAT / VPN overlay
probe 192.168.0.1 80       # RFC 1918
probe 172.16.0.1 80
probe 10.0.0.1 80
python3 - <<'PY'
import socket
q=bytes.fromhex("abcd01000001000000000000076578616d706c6503636f6d0000010001")
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(8); s.sendto(q,("10.200.0.1",53))
try:
    r=s.recv(512); print("dns answered", len(r))
except Exception as e: print("dns silent", e)
PY
ip -6 addr show scope global | grep -c inet6 || true
`
	r, err := runJob(ctx, conn, script)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.out)
	for _, line := range strings.Split(r.out, "\n") {
		if !strings.Contains(line, " rc=") {
			continue
		}
		if strings.HasSuffix(line, "rc=0") {
			t.Errorf("a guest reached a forbidden destination: %s", line)
		}
	}
	if !strings.Contains(r.out, "10.200.0.1:22 rc=124") {
		t.Errorf("host port must be dropped (timeout), got:\n%s", r.out)
	}
	if !strings.Contains(r.out, "dns answered") {
		t.Errorf("the DNS forwarder on the gateway must answer:\n%s", r.out)
	}
}

// TestVMRunsAJob drives a job (not the stub script) through the executor in a
// micro-VM: the runner user, the work layout, file commands, outputs, a failing
// step and cancellation by SIGINT. The VM is rootful-free like the other VM
// tests (user + network namespace, jailer off).
func TestVMRunsAJob(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_VM") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_VM=1 (needs KVM, firecracker, a kernel and a base image; see README)")
	}
	need(t, "mkfs.ext4")
	needNetAdmin(t)
	ctx := ctxWithLog(t)
	cfg := loadConfig(t, sandbox.VM)
	if why := sandbox.VMUnavailable(cfg, sandbox.Prober{}); len(why) > 0 {
		t.Fatalf("vm isolation is not available here: %s", strings.Join(why, "; "))
	}
	drv, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = drv.Close()
		if d2, err := sandbox.Select(context.Background(), cfg, drivers.Factories(), sandbox.Prober{}); err == nil {
			_ = d2.Reap(context.Background())
			_ = d2.Close()
		}
	})
	if err := drv.Reap(ctx); err != nil {
		t.Fatal(err)
	}

	run := func(yaml string, cancelAfter time.Duration) (steps []string, logs []string, res *execpb.JobResult, exit *execpb.Exit) {
		t.Helper()
		sb, err := drv.New()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := sb.Stop(ctx); err != nil {
				t.Errorf("stop: %v", err)
			}
		}()
		conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "vm-job"})
		if err != nil {
			t.Fatal(err)
		}
		var cancel chan *execpb.Cancel
		if cancelAfter > 0 {
			cancel = make(chan *execpb.Cancel, 1)
			time.AfterFunc(cancelAfter, func() { cancel <- &execpb.Cancel{Reason: "test"} })
		}
		_, exit, err = execstream.DriveJob(conn, &execpb.Run{Job: &execpb.JobSpec{
			WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: yaml, JobKey: "j", Repo: "hello", RunnerName: "it", RunnerVersion: "test",
			RunnerEnvironment: "self-hosted", Matrix: "{}",
			Contexts: `{"github":{"repository":"octo/hello","event_name":"push","job":"j","ref":"refs/heads/main","sha":"0123456789abcdef0123456789abcdef01234567"}}`,
		}}, cancel, execstream.JobHandler{
			StepStarted: func(s *execpb.StepStarted) { steps = append(steps, s.Name) },
			Log:         func(l *execpb.Log) { logs = append(logs, l.Text) },
			JobResult:   func(r *execpb.JobResult) { res = r },
		})
		if err != nil {
			t.Fatalf("driving the job: %v", err)
		}
		return
	}

	_, logs, res, exit := run(`on: push
jobs:
  j:
    runs-on: x
    outputs:
      who: ${{ steps.a.outputs.who }}
    steps:
      - id: a
        run: |
          echo "who=$(id -un):$(id -u)" >> "$GITHUB_OUTPUT"
          echo "pwd=$PWD home=$HOME"
          echo "FOO=bar" >> "$GITHUB_ENV"
      - run: echo "FOO=$FOO CI=$CI"
      - run: exit 3
      - if: always()
        run: echo still-here
`, 0)
	all := strings.Join(logs, "\n")
	if res == nil || res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_FAILURE || exit.Code == 0 {
		t.Fatalf("result %v exit %v\n%s", res, exit, all)
	}
	if res.Outputs["who"] != "runner:1001" {
		t.Errorf("steps ran as %q\n%s", res.Outputs["who"], all)
	}
	for _, w := range []string{"pwd=/home/runner/work/hello/hello home=/home/runner", "FOO=bar CI=true", "##[error]Process completed with exit code 3.", "still-here"} {
		if !strings.Contains(all, w) {
			t.Errorf("log lacks %q\n%s", w, all)
		}
	}

	start := time.Now()
	_, logs, res, _ = run("on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: |\n          trap 'echo got-int; exit 130' INT\n          echo ready\n          while :; do sleep 0.2; done\n", 8*time.Second)
	all = strings.Join(logs, "\n")
	if res == nil || res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_CANCELLED || !strings.Contains(all, "got-int") {
		t.Fatalf("cancel: %v\n%s", res, all)
	}
	t.Logf("cancelled job ended %v after the start", time.Since(start))
}

// nodeCache serves the pinned Node.js archives from a local directory
// (ERMITE_RUNNER_IT_NODE_DIR holding node-v24.21.0-linux-x64.tar.gz, say): the
// sandbox of the VM tests has no internet, and this way the real archive is
// still checked against the pinned SHA-256 and unpacked by the real code.
type nodeCache string

func (d nodeCache) RoundTrip(r *http.Request) (*http.Response, error) {
	f, err := os.Open(filepath.Join(string(d), filepath.Base(r.URL.Path)))
	if err != nil {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody, Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: f, Request: r}, nil
}

// TestVMNodeExternals runs the pinned Node.js 24, provided by the host outside
// the image, inside a job's micro-VM: the externals disk is the fourth drive,
// mounted read-only at /home/runner/externals, in GitHub's layout.
//
// Needs the archive of the pinned node24 for this architecture, either in
// ERMITE_RUNNER_IT_NODE_DIR (offline) or downloadable from nodejs.org.
func TestVMNodeExternals(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_VM") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_VM=1 (needs KVM, firecracker, a kernel and a base image; see README)")
	}
	need(t, "mkfs.ext4")
	needNetAdmin(t)
	ctx := ctxWithLog(t)
	cfg := loadConfig(t, sandbox.VM)
	if why := sandbox.VMUnavailable(cfg, sandbox.Prober{}); len(why) > 0 {
		t.Fatalf("vm isolation is not available here: %s", strings.Join(why, "; "))
	}

	m := &externals.Manager{Dir: filepath.Join(cfg.StateDir, "externals"), Arch: externals.HostArch()}
	for _, a := range externals.ForArch(externals.Pinned, m.Arch) {
		if a.Name == "node24" {
			m.Table = append(m.Table, a)
		}
	}
	if len(m.Table) != 1 {
		t.Skipf("no pinned node24 for %q", m.Arch)
	}
	if dir := os.Getenv("ERMITE_RUNNER_IT_NODE_DIR"); dir != "" {
		m.Client = &http.Client{Transport: nodeCache(dir)}
	}
	if err := m.Sync(ctx); err != nil {
		t.Skipf("cannot get the pinned Node.js (set ERMITE_RUNNER_IT_NODE_DIR to a directory with %s): %v", filepath.Base(m.Table[0].URL), err)
	}
	cfg.Externals = m

	drv, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = drv.Close()
		if d2, err := sandbox.Select(context.Background(), cfg, drivers.Factories(), sandbox.Prober{}); err == nil {
			_ = d2.Reap(context.Background())
			_ = d2.Close()
		}
	})
	if err := drv.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	sb, err := drv.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Stop(context.Background()) }()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "node-externals"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runJob(ctx, conn, `
N=/home/runner/externals/node24/bin/node
$N -e 'console.log(process.version)'
echo "has-node24: $(test -x /home/runner/externals/node24/bin/node && echo yes)"
touch /home/runner/externals/node24/probe 2>/dev/null && echo "WRITABLE" || echo "read-only"
grep ' /home/runner/externals ' /proc/mounts
`)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.out + r.errOut)
	want := m.Table[0].Version
	for _, w := range []string{"v" + want + "\n", "has-node24: yes\n", "read-only\n", "/dev/vdd /home/runner/externals ext4 ro"} {
		if !strings.Contains(r.out, w) {
			t.Errorf("output lacks %q", w)
		}
	}
	if strings.Contains(r.out, "WRITABLE") {
		t.Error("the externals are writable from the guest")
	}
}

// TestVMBinfmtMisc checks what docker/setup-qemu-action needs from the guest:
// binfmt_misc mounted at boot, as on GitHub's image, and a handler registered
// with the F (fix binary) flag that tonistiigi/binfmt uses. The handler here is
// cat on an extension, so the test needs neither QEMU nor the network.
func TestVMBinfmtMisc(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_VM") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_VM=1")
	}
	need(t, "mkfs.ext4")
	needNetAdmin(t)
	ctx := ctxWithLog(t)
	cfg := loadConfig(t, sandbox.VM)
	drv, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()
	_ = drv.Reap(ctx)
	sb, err := drv.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Stop(context.Background()) }()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "binfmt"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runJob(ctx, conn, `
S=; [ "$(id -u)" = 0 ] || S="sudo -n"
grep ' /proc/sys/fs/binfmt_misc ' /proc/mounts
echo "status: $(cat /proc/sys/fs/binfmt_misc/status)"
echo ':ermitetest:E::ermitetest::/usr/bin/cat:F' | $S tee /proc/sys/fs/binfmt_misc/register >/dev/null
grep -x 'flags: F' /proc/sys/fs/binfmt_misc/ermitetest
printf 'ran through binfmt_misc\n' > /tmp/probe.ermitetest
chmod +x /tmp/probe.ermitetest
/tmp/probe.ermitetest
`)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.out + r.errOut)
	for _, w := range []string{"binfmt_misc /proc/sys/fs/binfmt_misc binfmt_misc", "status: enabled\n", "flags: F\n", "ran through binfmt_misc\n"} {
		if !strings.Contains(r.out, w) {
			t.Errorf("output lacks %q", w)
		}
	}
}
