// SPDX-License-Identifier: MIT

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/drivers"
)

func TestContainerStubExecutor(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_CONTAINER") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_CONTAINER=1 (needs a rootful Docker daemon and the runner image; see README)")
	}
	need(t, "docker")
	needNetAdmin(t)
	ctx := ctxWithLog(t)
	cfg := loadConfig(t, sandbox.Container)
	// A rootless daemon is refused with a clear error: nothing to test there.
	probe, err := drivers.Factories().Container(ctx, cfg)
	if err != nil {
		if strings.Contains(err.Error(), "rootless") {
			t.Skipf("the Docker daemon is rootless, as designed the driver refuses it: %v", err)
		}
		t.Fatal(err)
	}
	// The probe holds the host-wide lock: release it before the scenario takes it.
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	scenario(t, ctx, cfg, sandbox.Container, func(t *testing.T, sb sandbox.Sandbox) {
		out, err := exec.Command("docker", "ps", "-q", "--filter", "label="+sandbox.LabelJob).Output()
		if err != nil {
			t.Fatal(err)
		}
		ids := strings.Fields(string(out))
		if len(ids) == 0 {
			t.Fatal("no job container to kill")
		}
		if b, err := exec.Command("docker", append([]string{"kill"}, ids...)...).CombinedOutput(); err != nil {
			t.Fatalf("docker kill: %v: %s", err, b)
		}
	})
}
