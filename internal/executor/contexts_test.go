// SPDX-License-Identifier: MIT

package executor

import (
	"reflect"
	"strings"
	"testing"

	"bnema.dev/ermite-actions/expr"
)

const testContexts = `{
 "github": {"repository":"octo/hello","repository_owner":"octo","run_id":"42","run_number":"7","run_attempt":"1",
   "ref":"refs/heads/main","ref_name":"main","ref_type":"branch","sha":"abc","event_name":"push","job":"build",
   "ref_protected":false,"workflow":"CI","actor":"octo","server_url":"https://ermite.example",
   "event":{"ref":"refs/heads/main","n":1},"retention_days":"90","unknown_key":"x"},
 "needs": {"a":{"result":"success","outputs":{"o":"1"}}},
 "strategy": {"fail-fast":true,"job-index":0,"job-total":1,"max-parallel":1},
 "matrix": {"os":"linux"},
 "inputs": {},
 "vars": {"V":"1"}
}`

func newTestState(t *testing.T) *State {
	t.Helper()
	s, _, err := NewState(testContexts, `{"os":"linux"}`, "build", RunnerInfo{Name: "r1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewStateLayoutAndGitHub(t *testing.T) {
	s := newTestState(t)
	l := s.Layout()
	if l.Workspace != "/home/runner/work/hello/hello" || l.RunnerWorkspace != "/home/runner/work/hello" ||
		l.Temp != "/home/runner/work/_temp" || l.ToolCache != "/opt/hostedtoolcache" || l.Home != "/home/runner" {
		t.Fatalf("%+v", l)
	}
	if s.GitHub("workspace") != l.Workspace || s.GitHub("job") != "build" {
		t.Fatalf("workspace %q job %q", s.GitHub("workspace"), s.GitHub("job"))
	}
	// token comes first (ExecutionContext.cs:1023): an empty token, no GITHUB_TOKEN yet.
	gh := s.Values(nil)["github"].(*expr.Dictionary)
	if gh.Keys()[0] != "token" {
		t.Fatalf("keys %v", gh.Keys())
	}
}

func TestRuntimeEnv(t *testing.T) {
	s := newTestState(t)
	env := s.RuntimeEnv()
	want := map[string]string{
		"GITHUB_REPOSITORY": "octo/hello", "GITHUB_REPOSITORY_OWNER": "octo", "GITHUB_RUN_ID": "42",
		"GITHUB_RUN_NUMBER": "7", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REF_NAME": "main", "GITHUB_REF_TYPE": "branch", "GITHUB_SHA": "abc", "GITHUB_EVENT_NAME": "push",
		"GITHUB_JOB": "build", "GITHUB_REF_PROTECTED": "false", "GITHUB_WORKFLOW": "CI", "GITHUB_ACTOR": "octo",
		"GITHUB_SERVER_URL": "https://ermite.example", "GITHUB_RETENTION_DAYS": "90",
		"GITHUB_WORKSPACE": "/home/runner/work/hello/hello",
		"RUNNER_OS":        "Linux", "RUNNER_NAME": "r1", "RUNNER_ENVIRONMENT": "self-hosted",
		"RUNNER_TEMP": "/home/runner/work/_temp", "RUNNER_TOOL_CACHE": "/opt/hostedtoolcache",
		"RUNNER_WORKSPACE": "/home/runner/work/hello",
	}
	for k, v := range want {
		if got, ok := env.Get(k); !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
	// Only allow-listed string/boolean keys; objects are not exported.
	for _, k := range []string{"GITHUB_EVENT", "GITHUB_UNKNOWN_KEY", "GITHUB_TOKEN"} {
		if _, ok := env.Get(k); ok {
			t.Errorf("%s must not be set", k)
		}
	}
	if a, _ := env.Get("RUNNER_ARCH"); a != "X64" && a != "ARM64" {
		t.Errorf("RUNNER_ARCH = %q", a)
	}
}

func TestRunnerContextOrderAndValues(t *testing.T) {
	s := newTestState(t)
	r := s.Values(nil)["runner"].(*expr.Dictionary)
	if got := strings.Join(r.Keys(), ","); got != "os,arch,name,environment,tool_cache,temp,workspace" {
		t.Fatal(got)
	}
	s2, _, _ := NewState(testContexts, "", "build", RunnerInfo{Name: "h", Environment: "github-hosted", Arch: "ARM64"}, "")
	r2 := s2.Values(nil)["runner"].(*expr.Dictionary)
	if str(r2, "environment") != "github-hosted" || str(r2, "arch") != "ARM64" {
		t.Fatal(expr.ToJSON(r2))
	}
}

func TestServerContextsAndDefaults(t *testing.T) {
	s := newTestState(t)
	v := s.Values(nil)
	if expr.ToJSON(v["matrix"]) != "{\n  \"os\": \"linux\"\n}" {
		t.Fatal(expr.ToJSON(v["matrix"]))
	}
	for _, k := range []string{"needs", "strategy", "inputs", "vars", "steps", "env", "job", "secrets"} {
		if v[k] == nil {
			t.Errorf("context %s missing", k)
		}
	}
	// Absent contexts: needs/inputs/vars are empty objects, matrix is null
	// (also when the server sends {}), as the recordings show.
	s2, _, err := NewState(`{"github":{"repository":"o/r"}}`, "{}", "j", RunnerInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	v2 := s2.Values(nil)
	if _, ok := v2["matrix"].(expr.Null); !ok {
		t.Errorf("matrix without a matrix: %v, want null", v2["matrix"])
	}
	// The server's "{}" in the contexts is null too.
	s3, _, err := NewState(`{"github":{"repository":"o/r"},"matrix":{}}`, "{}", "j", RunnerInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s3.Values(nil)["matrix"].(expr.Null); !ok {
		t.Errorf("empty matrix: %v, want null", s3.Values(nil)["matrix"])
	}
	if d, ok := v2["needs"].(*expr.Dictionary); !ok || d.Len() != 0 {
		t.Errorf("needs %v", v2["needs"])
	}
}

func TestNewStateErrors(t *testing.T) {
	for _, tc := range []struct{ ctx, matrix, repo, want string }{
		{`[]`, "", "", "want a JSON object"},
		{`{`, "", "", "job contexts"},
		{`{"github":1}`, "", "", "github must be an object"},
		{`{"github":{}}`, "", "", "github.repository is missing"},
		{`{"github":{"repository":"o/r"}}`, "{", "", "job matrix"},
	} {
		_, _, err := NewState(tc.ctx, tc.matrix, "j", RunnerInfo{}, tc.repo)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.ctx, err, tc.want)
		}
	}
	// The repository name can come from the job when the context lacks it.
	if _, _, err := NewState(`{"github":{}}`, "", "j", RunnerInfo{}, "hello"); err != nil {
		t.Fatal(err)
	}
}

func TestJobStatus(t *testing.T) {
	s := newTestState(t)
	job := func() *expr.Dictionary { return s.Values(nil)["job"].(*expr.Dictionary) }
	if str(job(), "status") != "success" || job().Keys()[0] != "status" {
		t.Fatal(expr.ToJSON(job()))
	}
	s.SetStatus("failure")
	if str(job(), "status") != "failure" || s.Status() != "failure" {
		t.Fatal(expr.ToJSON(job()))
	}
}

func TestEnvOrderAndOverride(t *testing.T) {
	o := newOrdered()
	o.Set("A", "1")
	o.Set("B", "2")
	o.Set("A", "3") // keeps its position
	if !reflect.DeepEqual(o.Keys(), []string{"A", "B"}) || o.vals["A"] != "3" {
		t.Fatalf("%v", o)
	}
	c := o.Clone()
	c.Set("C", "4")
	if o.Len() != 2 || c.Len() != 3 {
		t.Fatal("Clone shares state")
	}
	// Case-sensitive keys, like Linux.
	o.Set("a", "x")
	if o.Len() != 3 {
		t.Fatal("keys must be case-sensitive")
	}
}

func TestStepsContext(t *testing.T) {
	s := newTestState(t)
	s.SetOutput("one", "out", "v")
	s.SetOutput("__run", "ignored", "x") // generated ids are not in the context
	s.SetStepResult("one", "failure", "success")
	s.SetStepResult("__run_2", "success", "success")
	s.SetStepResult("skipped", "skipped", "skipped")
	steps := s.Values(nil)["steps"].(*expr.Dictionary)
	if got := strings.Join(steps.Keys(), ","); got != "one,skipped" {
		t.Fatalf("keys %s", got)
	}
	one, _ := steps.Get("one")
	od := one.(*expr.Dictionary)
	if str(od, "outcome") != "failure" || str(od, "conclusion") != "success" {
		t.Fatal(expr.ToJSON(one))
	}
	outs, _ := od.Get("outputs")
	if str(outs.(*expr.Dictionary), "out") != "v" || s.Output("one", "out") != "v" || s.Output("none", "x") != "" {
		t.Fatal(expr.ToJSON(outs))
	}
	// A step that has output but has not completed has no outcome yet.
	s.SetOutput("running", "k", "v")
	r, _ := s.Values(nil)["steps"].(*expr.Dictionary).Get("running")
	if _, ok := r.(*expr.Dictionary).Get("outcome"); ok {
		t.Fatal("outcome before completion")
	}
}

func TestAddPathMovesRepeatedEntryToTheEnd(t *testing.T) {
	s := newTestState(t)
	for _, p := range []string{"/a", "/b", "/a"} {
		s.AddPath(p)
	}
	if !reflect.DeepEqual(s.Prepend(), []string{"/b", "/a"}) {
		t.Fatal(s.Prepend())
	}
}

func TestEnvContextForSteps(t *testing.T) {
	s := newTestState(t)
	s.SetEnv("JOB", "j")
	step := s.JobEnv()
	step.Set("STEP", "s")
	env := s.Values(step)["env"].(*expr.Dictionary)
	if str(env, "JOB") != "j" || str(env, "STEP") != "s" {
		t.Fatal(expr.ToJSON(env))
	}
	// The job environment is untouched by the step's copy.
	if _, ok := s.JobEnv().Get("STEP"); ok {
		t.Fatal("step env leaked into the job env")
	}
}

func TestEventJSON(t *testing.T) {
	s := newTestState(t)
	if got := s.EventJSON(); !strings.Contains(got, `"ref": "refs/heads/main"`) {
		t.Fatal(got)
	}
	s2, _, _ := NewState(`{"github":{"repository":"o/r"}}`, "", "j", RunnerInfo{}, "")
	if s2.EventJSON() != "" {
		t.Fatal("no event, no event.json")
	}
}

func TestWithLayout(t *testing.T) {
	s := newTestState(t)
	l := NewLayout("/tmp/h", "hello", "/tmp/tc")
	s.WithLayout(l)
	env := s.RuntimeEnv()
	for k, v := range map[string]string{"GITHUB_WORKSPACE": "/tmp/h/work/hello/hello", "RUNNER_TEMP": "/tmp/h/work/_temp", "RUNNER_TOOL_CACHE": "/tmp/tc"} {
		if got, _ := env.Get(k); got != v {
			t.Errorf("%s = %q", k, got)
		}
	}
}

func TestGitHubKeysAddedByTheRunnerKeepTheSortedOrder(t *testing.T) {
	// A server that lists its keys sorted, as the recordings do: the keys the
	// runner adds land at their sorted place, replaced keys stay, token is first.
	s, _, err := NewState(`{"github":{"actor":"a","event_name":"push","job":"j","repository":"o/r","sha":"s","zzz":"1"}}`, "", "j", RunnerInfo{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"secret_source", "artifacts", "action", "event_path"} {
		s.SetGitHub(k, "x")
	}
	s.SetGitHub("sha", "t")
	got := strings.Join(s.Values(nil)["github"].(*expr.Dictionary).Keys(), ",")
	want := "token,action,actor,artifacts,event_name,event_path,job,repository,secret_source,sha,workspace,zzz"
	if got != want {
		t.Errorf("keys\n got %s\nwant %s", got, want)
	}
}
