// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Real docker: set ERMITE_RUNNER_IT_DOCKER=1 on a host with a Docker daemon
// and the images alpine:3.20 and busybox:latest (no pulls are needed; the
// tests do pull, so the network must work otherwise).
func realDocker(t *testing.T) string {
	t.Helper()
	if os.Getenv("ERMITE_RUNNER_IT_DOCKER") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_DOCKER=1 (needs a Docker daemon)")
	}
	p, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker")
	}
	return p
}

func TestRealDockerJobContainerAndService(t *testing.T) {
	docker := realDocker(t)
	needTools(t, "bash")
	yaml := `    container: alpine:3.20
    services:
      web:
        image: busybox:latest
        options: >-
          --health-cmd "true" --health-interval 1s --health-retries 3
          --entrypoint sh
        command: -c "sleep 300"
    steps:
      - name: where
        run: |
          echo "pwd=$PWD"
          echo "id=$(id -u)"
          echo "home=$HOME"
          echo "temp=$RUNNER_TEMP"
          ls -d /__w /__t /__e /github/home /github/workflow
          echo "hi from alpine" >> "$GITHUB_STEP_SUMMARY"
          echo "k=v" >> "$GITHUB_OUTPUT"
      - name: svc
        run: ping -c1 web >/dev/null && echo "web resolves"
`
	rec, res := runLocal(t, wfHead+yaml, "j", func(c *Config) {
		c.Actions.DockerPath = docker
		c.BaseEnv = append(c.BaseEnv, "ACTIONS_SERVICE_CONTAINER_COMMAND=true", "DOCKER_HOST="+os.Getenv("DOCKER_HOST"), "XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"))
		c.Actions.ExternalsDir = fakeNode(t)
	})
	log := logOf(rec.step(t, "where")) + logOf(rec.step(t, "svc"))
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v\n%s\n%s", res, logOf(rec.step(t, "Initialize containers")), log)
	}
	for _, w := range []string{"pwd=/__w/hello/hello", "home=/github/home", "temp=/__w/_temp", "/github/workflow", "web resolves"} {
		if !strings.Contains(log, w) {
			t.Errorf("lacks %q:\n%s", w, log)
		}
	}
	if out, _ := exec.Command(docker, "ps", "-a", "--format", "{{.Names}}", "--filter", "name=busybox").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("containers left: %s", out)
	}
	if out, _ := exec.Command(docker, "network", "ls", "--format", "{{.Name}}", "--filter", "name=github_network_").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("networks left: %s", out)
	}
}

func TestRealDockerCancelRemovesEverything(t *testing.T) {
	docker := realDocker(t)
	needTools(t, "bash")
	cancel := NewCanceller()
	go func() { time.Sleep(6 * time.Second); cancel.Cancel(CancelUser) }()
	rec, res := runLocalC(t, cancel, wfHead+"    container: alpine:3.20\n    steps:\n      - name: long\n        run: sleep 120\n", "j", func(c *Config) {
		c.Actions.DockerPath = docker
		c.BaseEnv = append(c.BaseEnv, "XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"))
	})
	if res.Conclusion != ConclusionCancelled {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if out, _ := exec.Command(docker, "network", "ls", "--format", "{{.Name}}", "--filter", "name=github_network_").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("networks left: %s", out)
	}
}
