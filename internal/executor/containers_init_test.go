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

// GO-004: values of container variables are in the environment of the docker
// process; the arguments carry "-e NAME". The echoed line is the runner's
// (recorded: -e "NAME=value"), masked like any log.
func TestContainerEnvValuesStayOffTheArguments(t *testing.T) {
	f := newFakeCtr(t)
	rec, res := ctrRun(t, f, "    container:\n      image: ubuntu:24.04\n      env:\n        CVAR: s3cret-value\n        EMPTYV: \"\"\n        DOCKER_HOST: tcp://x:1\n",
		"      - name: p\n        run: echo\n      - name: q\n        env:\n          STEPVAR: s3cret-step\n        run: echo\n", func(c *Config) { f.set(t, "add_path", c.Layout.WorkRoot) })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	var create []string
	for _, c := range f.calls(t) {
		if c[0] == "create" {
			create = c
		}
	}
	joined := strings.Join(create, "\n")
	for _, w := range []string{"\n-e\nCVAR\n", "\n-e\nEMPTYV\n", "\n-e\nDOCKER_HOST=tcp://x:1\n", "ENV:CVAR=s3cret-value:EMPTYV=:"} {
		if !strings.Contains(joined, w) {
			t.Errorf("create lacks %q:\n%s", w, joined)
		}
	}
	if strings.Contains(joined, "CVAR=s3cret") && !strings.Contains(joined, "ENV:CVAR=s3cret") {
		t.Errorf("value on the arguments:\n%s", joined)
	}
	for _, a := range create {
		if strings.HasPrefix(a, "CVAR=") || strings.HasPrefix(a, "EMPTYV=") {
			t.Errorf("argument %q carries a value", a)
		}
	}
	// What the runner echoes (recorded in ctr-options).
	init := logOf(rec.step(t, "Initialize containers"))
	if !strings.Contains(init, ` -e "CVAR=s3cret-value" -e "EMPTYV" `) {
		t.Errorf("echo:\n%s", init)
	}
	// exec: no value of a step variable and no PATH on the arguments.
	for _, c := range f.execCalls(t) {
		for _, a := range c {
			if strings.HasPrefix(a, "ENV:") {
				continue // what the fake saw in its environment
			}
			if strings.Contains(a, "s3cret-step") || strings.HasPrefix(a, "PATH=") || strings.Contains(a, "/added") {
				t.Errorf("exec argument %q", a)
			}
		}
	}
	ex := f.execCalls(t)
	last := strings.Join(ex[len(ex)-1], "\n")
	if !strings.Contains(last, "\n-e\nPATH\n") || !strings.Contains(last, "ENV:") || !strings.Contains(last, "PATH=/added:") {
		t.Errorf("second exec:\n%s", last)
	}
}
