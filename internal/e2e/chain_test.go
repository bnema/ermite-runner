// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/host"
	"bnema.dev/ermite-runner/internal/host/hosttest"
)

// The chain test runs the whole path of a job: a fake RunnerService hands out a
// corpus job, the real host loop starts a real sandbox (the runner image in a
// container), the real executor runs the steps, and the host reports to the
// fake with AppendLog, UpdateJob and CompleteJob. What the fake stored is
// compared with the recording of the same job made on github.com.
//
// It needs a checkout of ermite-conformance (corpus/ and recordings/), found
// through ERMITE_CONFORMANCE_DIR or as a sibling of this repository, and is
// read-only on it.

func conformanceDir(t *testing.T) string {
	t.Helper()
	cands := []string{os.Getenv("ERMITE_CONFORMANCE_DIR")}
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != "."; d = filepath.Dir(d) {
		cands = append(cands, filepath.Join(d, "ermite-conformance", ".worktrees", "corpus"), filepath.Join(d, "ermite-conformance"))
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(c, "recordings")); err == nil && st.IsDir() {
			return c
		}
	}
	t.Skip("ermite-conformance checkout not found (set ERMITE_CONFORMANCE_DIR)")
	return ""
}

type recStep struct {
	Name       string   `json:"name"`
	Conclusion string   `json:"conclusion"`
	Log        []string `json:"log"`
}

type recJob struct {
	Name       string    `json:"name"`
	Conclusion string    `json:"conclusion"`
	Steps      []recStep `json:"steps"`
}

type recording struct {
	Pushes []struct {
		Runs []struct {
			Jobs []recJob `json:"jobs"`
		} `json:"runs"`
	} `json:"pushes"`
}

var (
	uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// Machine-dependent parts of the recordings, like the normalisation rules of
	// ermite-conformance: random script names and the repository name.
	repoRe = regexp.MustCompile(`/work/hello/hello`)
	// The runner version moves with each release (rule runner-version of
	// ermite-conformance).
	versionRe = regexp.MustCompile(`(Current runner version: ')[^']*(')`)
)

func normalize(l string) string {
	l = versionRe.ReplaceAllString(l, "${1}<runner-version>${2}")
	l = repoRe.ReplaceAllString(l, "/work/<repo>/<repo>")
	return uuidRe.ReplaceAllString(l, "<uuid>")
}

func chainContexts(job string) string {
	return fmt.Sprintf(`{"github":{"repository":"octo/hello","repository_owner":"octo","run_id":"1","run_number":"1","run_attempt":"1",
	"ref":"refs/heads/scn/x/r1","ref_name":"scn/x/r1","ref_type":"branch","ref_protected":false,"sha":"0123456789abcdef0123456789abcdef01234567",
	"event_name":"push","event":{"ref":"refs/heads/scn/x/r1"},"job":%q,"workflow":"w","actor":"octo","triggering_actor":"octo",
	"server_url":"https://ermite.example","retention_days":"90"}}`, job)
}

func TestChainCorpus(t *testing.T) {
	image := needE2E(t)
	dir := conformanceDir(t)
	drv := newDriver(t, image)

	// scenario -> jobs to run (jobs with `needs` depend on the server and are left out)
	scenarios := []struct {
		name string
		jobs []string
	}{
		{"env-precedence", []string{"main", "other"}},
		{"env-expressions", []string{"main"}},
		{"out-github-env", []string{"main"}},
		{"out-github-env-reserved", []string{"node-options", "github-prefix"}},
		{"out-github-path", []string{"main"}},
		{"out-step-outputs", []string{"main"}},
		{"ctx-steps", []string{"main"}},
		{"if-step-conditions", []string{"main"}},
		{"shell-builtin", []string{"main"}},
		{"shell-custom", []string{"main"}},
		{"shell-exit-codes", nil}, // every job of the workflow
		{"shell-working-directory", nil},
		{"out-job-outputs", []string{"producer"}},
	}

	srv := hosttest.New()
	t.Cleanup(srv.Close)

	type want struct {
		scenario string
		job      recJob
	}
	wants := map[string]want{}
	var queue []*ermite.Job
	for _, sc := range scenarios {
		files, _ := filepath.Glob(filepath.Join(dir, "corpus", sc.name, "tree", ".github", "workflows", "*.yml"))
		if len(files) != 1 {
			t.Fatalf("%s: workflow files %v", sc.name, files)
		}
		yaml, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "recordings", sc.name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var rec recording
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		for _, rj := range rec.Pushes[0].Runs[0].Jobs {
			if sc.jobs != nil && !contains(sc.jobs, rj.Name) {
				continue
			}
			id := sc.name + "/" + rj.Name
			queue = append(queue, &ermite.Job{
				JobId: id, RunId: 1, Attempt: 1, JobToken: "ermitejob_" + strings.ReplaceAll(id, "/", "_"),
				WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: string(yaml), JobKey: rj.Name, Matrix: "{}",
				Contexts: chainContexts(rj.Name), Repo: "hello", TimeoutMinutes: 10,
			})
			wants[id] = want{sc.name, rj}
		}
	}
	srv.Queue(queue...)

	r := &host.Runner{
		Cfg:    host.Config{URL: srv.URL, Name: "chain", Capacity: 2, Version: "test"},
		Driver: drv,
		Poll:   host.NewClient(srv.URL, hosttest.RunnerToken, http.DefaultTransport),
		Job: func(tok string) ermiteconnect.RunnerServiceClient {
			return host.NewClient(srv.URL, tok, http.DefaultTransport)
		},
		Timing: host.Timing{LogFlush: 20 * time.Millisecond, NoJobPause: 100 * time.Millisecond},
	}
	ctx, cancel := context.WithCancel(zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "info", Format: "console", Output: testWriter{t}})))
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Error("Run did not stop")
		}
	}()

	deadline := time.After(5 * time.Minute)
	for n := 0; n < len(queue); n++ {
		select {
		case <-srv.Done():
		case <-deadline:
			t.Fatalf("only %d of %d jobs finished", n, len(queue))
		}
	}

	var ids []string
	for id := range wants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := wants[id]
		t.Run(id, func(t *testing.T) { compareJob(t, srv.Result(id), w.job, w.scenario) })
	}
	for _, e := range srv.Violations() {
		t.Errorf("the fake server saw a contract violation: %s", e)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// compareJob checks the steps, conclusions, outputs and log lines reported for
// a job against the recording. The ermite-dump step that the harness appends to
// every job is not compared, and neither are the three groups of "Set up job"
// that describe GitHub's fleet and image (ermite-conformance drops them from
// the recordings, rule infra-groups): the other lines of "Set up job" are.
func compareJob(t *testing.T, got hosttest.Result, want recJob, scenario string) {
	if got.Complete == nil {
		t.Fatal("the job did not complete")
	}
	// The conclusion of the job as GitHub recorded it, except where the dump
	// step failed on GitHub (it only fails after a failed step in some
	// scenarios and is not part of the job here).
	wantConcl := map[string]ermite.CiConclusion{
		"success": ermite.CiConclusion_CI_CONCLUSION_SUCCESS, "failure": ermite.CiConclusion_CI_CONCLUSION_FAILURE,
		"cancelled": ermite.CiConclusion_CI_CONCLUSION_CANCELLED,
	}
	jobWant := wantConcl[want.Conclusion]
	for _, s := range want.Steps {
		if s.Name == "ermite-dump" && s.Conclusion != "success" && want.Conclusion == "failure" {
			// the job failed only because of the dump step
			jobWant = ermite.CiConclusion_CI_CONCLUSION_SUCCESS
			for _, o := range want.Steps {
				if o.Name != "ermite-dump" && o.Conclusion == "failure" {
					jobWant = ermite.CiConclusion_CI_CONCLUSION_FAILURE
				}
			}
		}
	}
	if got.Complete.Conclusion != jobWant {
		t.Errorf("job conclusion %v, want %v", got.Complete.Conclusion, jobWant)
	}

	// Steps by number, with their lines.
	byStep := map[int32][]string{}
	for _, l := range got.Lines {
		byStep[l.Step] = append(byStep[l.Step], normalize(l.Text))
	}
	var steps []int32
	for n := range got.Steps {
		steps = append(steps, n)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })

	type es = recStep
	var wantSteps, gotSteps []es
	for _, s := range want.Steps {
		if s.Name == "ermite-dump" {
			continue
		}
		wantSteps = append(wantSteps, s)
	}
	conc := map[ermite.CiConclusion]string{
		ermite.CiConclusion_CI_CONCLUSION_SUCCESS: "success", ermite.CiConclusion_CI_CONCLUSION_FAILURE: "failure",
		ermite.CiConclusion_CI_CONCLUSION_CANCELLED: "cancelled", ermite.CiConclusion_CI_CONCLUSION_SKIPPED: "skipped",
	}
	for _, n := range steps {
		u := got.Steps[n]
		if u.State != ermite.StepState_STEP_STATE_COMPLETED || u.StartedAt == nil || u.CompletedAt == nil {
			t.Errorf("step %d %q is not a complete snapshot: %v", n, u.Name, u)
		}
		gotSteps = append(gotSteps, es{Name: u.Name, Conclusion: conc[u.Conclusion], Log: byStep[n]})
	}
	if len(gotSteps) != len(wantSteps) {
		var names []string
		for _, s := range gotSteps {
			names = append(names, s.Name)
		}
		t.Fatalf("steps %q, want %d steps", names, len(wantSteps))
	}
	for i := range wantSteps {
		w, g := wantSteps[i], gotSteps[i]
		if g.Name != w.Name || g.Conclusion != w.Conclusion {
			t.Errorf("step %d: %q (%s), want %q (%s)", i+1, g.Name, g.Conclusion, w.Name, w.Conclusion)
			continue
		}
		if strings.Join(g.Log, "\n") != strings.Join(w.Log, "\n") {
			t.Errorf("step %q log differs:\n--- got\n%s\n--- want\n%s", w.Name, strings.Join(g.Log, "\n"), strings.Join(w.Log, "\n"))
		}
	}

	if scenario == "out-job-outputs" {
		checkProducerOutputs(t, got)
	}
}

// checkProducerOutputs compares the outputs of the producer job of
// out-job-outputs with what the recorded consumer printed from `needs`.
func checkProducerOutputs(t *testing.T, got hosttest.Result) {
	t.Helper()
	// The values the consumer printed on GitHub (recordings/out-job-outputs.json,
	// step "print needs outputs"): empty outputs are absent.
	want := map[string]string{
		"composed": "hello-push", "from-env": "env-value", "from-failed": "written-before-failure",
		"overwritten": "second", "simple": "hello", "multi": "line one\nline two", "literal": "fixed-text",
	}
	if len(got.Complete.Outputs) != len(want) {
		t.Errorf("outputs %v, want %v", got.Complete.Outputs, want)
	}
	for k, v := range want {
		if got.Complete.Outputs[k] != v {
			t.Errorf("output %q = %q, want %q", k, got.Complete.Outputs[k], v)
		}
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
