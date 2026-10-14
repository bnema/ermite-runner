// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
	"testing"
)

func TestInitAndStopServiceOnly(t *testing.T) {
	f := newFakeCtr(t)
	f.set(t, "health", "starting\nhealthy\n")
	f.set(t, "ports", "6379/tcp -> 0.0.0.0:16379\n6379/tcp -> [::]:16379\n")
	rec, res := ctrRun(t, f, "    services:\n      redis:\n        image: redis:7\n        ports:\n          - 16379:6379\n        options: >-\n          --health-cmd \"redis-cli ping\"\n          --health-retries 10\n",
		"      - name: ctx\n        run: echo \"p=${{ job.services.redis.ports['6379'] }} n=${{ job.services.redis.network != '' }}\"\n", nil)
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v %v", res, rec.names())
	}
	if got := strings.Join(rec.names(), "|"); got != "Set up job|Initialize containers|ctx|Stop containers|Complete job" {
		t.Fatalf("steps %s", got)
	}
	init := logOf(rec.step(t, "Initialize containers"))
	for _, w := range []string{
		"##[group]Checking docker version\n##[command]", "Docker daemon API version: '1.48'", "Docker client API version: '1.48'",
		"##[group]Clean up resources from previous jobs", "##[group]Create local container network",
		"##[group]Starting redis service container", " network create --label ",
		"--network-alias redis -p 16379:6379 --health-cmd \"redis-cli ping\" --health-retries 10 -e GITHUB_ACTIONS=true -e CI=true redis:7",
		"##[group]Waiting for all services to be ready", "redis service is starting, waiting 0 seconds before checking again.", "redis service is healthy.",
	} {
		if !strings.Contains(init, w) {
			t.Errorf("init log lacks %q:\n%s", w, init)
		}
	}
	if strings.Contains(init, "--workdir") || strings.Contains(init, "--entrypoint") {
		t.Errorf("a service has neither workdir nor entrypoint:\n%s", init)
	}
	stop := logOf(rec.step(t, "Stop containers"))
	if !strings.Contains(stop, "Print service container logs: ") || !strings.Contains(stop, "Stop and remove container: ") ||
		!strings.Contains(stop, "Remove container network: github_network_") || !strings.Contains(stop, "network rm github_network_") {
		t.Errorf("stop:\n%s", stop)
	}
	want := "version version ps network prune network create pull create start ps port inspect inspect logs rm network rm"
	if got := strings.Join(f.verbs(t), " "); got != want {
		t.Errorf("docker calls:\n%s\nwant\n%s", got, want)
	}
	run := logOf(rec.step(t, "ctx"))
	if !strings.Contains(run, "p=16379 n=true") {
		t.Errorf("job.services not in the context:\n%s", run)
	}
}
