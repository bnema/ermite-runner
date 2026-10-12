// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

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
		// Kill the VMM process of the only running VM.
		out, err := exec.Command("pgrep", "-f", "--api-sock .*--id er|--id er[0-9a-f]{8} .*--api-sock|--id er[0-9a-f]{8} --api-sock").Output()
		if err != nil {
			t.Fatalf("no firecracker to kill: %v", err)
		}
		for _, f := range strings.Fields(string(out)) {
			pid := 0
			for _, c := range f {
				pid = pid*10 + int(c-'0')
			}
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
