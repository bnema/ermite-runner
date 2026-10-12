// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run jobs of the ermite-conformance corpus (the CI-1 scenarios)
// through the executor on this machine and compare the steps, their
// conclusions and their logs with the recordings made on github.com. They need
// a checkout of ermite-conformance (the corpus and recordings directories),
// found through ERMITE_CONFORMANCE_DIR or as a sibling of this repository;
// they skip without it.

func conformanceDir(t *testing.T) string {
	t.Helper()
	cands := []string{os.Getenv("ERMITE_CONFORMANCE_DIR")}
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != "."; d = filepath.Dir(d) {
		cands = append(cands,
			filepath.Join(d, "ermite-conformance", ".worktrees", "corpus"),
			filepath.Join(d, "ermite-conformance"))
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(c, "corpus")); err == nil && st.IsDir() {
			return c
		}
	}
	t.Skip("ermite-conformance checkout not found (set ERMITE_CONFORMANCE_DIR)")
	return ""
}

func needTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not installed", n)
		}
	}
}

// recorder is a Reporter that keeps everything.
type recorder struct {
	mu          sync.Mutex
	steps       []*recStep
	byNum       map[int]*recStep
	result      Result
	done        bool
	lastLogTime time.Time
}

type recStep struct {
	Num        int
	Name       string
	Conclusion Conclusion
	Log        []string
	Started    time.Time
	Completed  time.Time
}

func newRecorder() *recorder { return &recorder{byNum: map[int]*recStep{}} }

func (r *recorder) StepStarted(n int, name string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &recStep{Num: n, Name: name, Started: at}
	r.steps = append(r.steps, s)
	r.byNum[n] = s
}
func (r *recorder) Log(n int, at time.Time, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.byNum[n]; s != nil {
		s.Log = append(s.Log, line)
	}
	r.lastLogTime = at
}
func (r *recorder) StepCompleted(n int, name string, c Conclusion, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byNum[n]
	s.Conclusion, s.Completed = c, at
	if name != "" {
		s.Name = name
	}
}
func (r *recorder) JobCompleted(res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.result, r.done = res, true
}

func (r *recorder) step(t *testing.T, name string) *recStep {
	t.Helper()
	for _, s := range r.steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in %v", name, r.names())
	return nil
}

func (r *recorder) names() []string {
	var out []string
	for _, s := range r.steps {
		out = append(out, s.Name)
	}
	return out
}

// testGitHubContext is a push on a branch, with the keys the corpus reads.
func testGitHubContext(job string) string {
	return fmt.Sprintf(`{"github":{"repository":"octo/hello","repository_owner":"octo","run_id":"1","run_number":"1","run_attempt":"1",
	"ref":"refs/heads/scn/x/r1","ref_name":"scn/x/r1","ref_type":"branch","ref_protected":false,"sha":"0123456789abcdef0123456789abcdef01234567",
	"event_name":"push","event":{"ref":"refs/heads/scn/x/r1"},"job":%q,"workflow":"w","actor":"octo","triggering_actor":"octo",
	"server_url":"https://ermite.example","retention_days":"90"}}`, job)
}

// runLocal runs one job of a workflow file on this machine under a temporary
// home. The environment is minimal and fixed.
func runLocal(t *testing.T, yaml, jobKey string, tweak func(*Config)) (*recorder, Result) {
	t.Helper()
	home := t.TempDir()
	layout := NewLayout(home, "hello", filepath.Join(home, "toolcache"))
	rec := newRecorder()
	cfg := Config{
		WorkflowPath: ".github/workflows/ci.yml", WorkflowYAML: []byte(yaml), JobKey: jobKey,
		ContextsJSON: testGitHubContext(jobKey), Runner: RunnerInfo{Name: "test-runner", Environment: "github-hosted"},
		RunnerVersion: "test", Layout: &layout, Reporter: rec,
		BaseEnv:   []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8"},
		TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond, WaitDelay: 500 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	res := Run(nil, cfg)
	return rec, res
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type recording struct {
	Pushes []struct {
		Runs []struct {
			Conclusion string `json:"conclusion"`
			Jobs       []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
				Steps      []struct {
					Name       string   `json:"name"`
					Conclusion string   `json:"conclusion"`
					Log        []string `json:"log"`
				} `json:"steps"`
			} `json:"jobs"`
		} `json:"runs"`
	} `json:"pushes"`
}

var (
	shellPathRe = regexp.MustCompile(`^shell: \S*/(\w+) `)
	startPathRe = regexp.MustCompile(`process '\S*/(\w+)'`)
	uuidRe      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// normalizeLine maps the machine-dependent parts of a line to the form of the
// recordings, like the normalisation rules of ermite-conformance: the
// directory of the shell binary, the home directory, the repository name and
// the random names of script files.
func normalizeLine(l, home string) string {
	l = shellPathRe.ReplaceAllString(l, "shell: /usr/bin/$1 ")
	l = startPathRe.ReplaceAllString(l, "process '/usr/bin/$1'")
	l = strings.ReplaceAll(l, home, "/home/runner")
	l = strings.ReplaceAll(l, "/work/hello/hello", "/work/<repo>/<repo>")
	return uuidRe.ReplaceAllString(l, "<uuid>")
}

// shIsBash reports whether sh on this machine is bash (Arch, Fedora): the
// image of ermite has dash, so a step that checks BASH_VERSION under sh would
// differ here for a reason that has nothing to do with the executor.
func shIsBash() bool {
	out, err := exec.Command("sh", "-c", `echo "${BASH_VERSION-unset}"`).Output()
	return err == nil && strings.TrimSpace(string(out)) != "unset"
}

// compareWithRecording runs every job without needs of a scenario and checks
// that the steps, conclusions and logs equal the recording of github.com. Set
// up job (runner and image details) and the dump step the harness appends are
// not compared.
func compareWithRecording(t *testing.T, scenario string, skipJobs ...string) {
	t.Helper()
	needTools(t, "bash", "sh")
	dir := conformanceDir(t)
	files, _ := filepath.Glob(filepath.Join(dir, "corpus", scenario, "tree", ".github", "workflows", "*.yml"))
	if len(files) != 1 {
		t.Fatalf("scenario %s: workflow files %v", scenario, files)
	}
	yaml := readFile(t, files[0])
	var rec recording
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "recordings", scenario+".json"))), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Pushes) == 0 || len(rec.Pushes[0].Runs) == 0 {
		t.Skip("scenario not recorded")
	}
	skip := map[string]bool{}
	for _, s := range skipJobs {
		skip[s] = true
	}
	n := 0
	for _, jr := range rec.Pushes[0].Runs[0].Jobs {
		if skip[jr.Name] {
			continue
		}
		jr := jr
		t.Run(jr.Name, func(t *testing.T) {
			var home string
			got, res := runLocal(t, yaml, jr.Name, func(c *Config) { home = c.Layout.Home })
			// Expected steps: the recording without Set up job and the dump step.
			type es struct {
				Name, Conclusion string
				Log              []string
			}
			var want []es
			for _, s := range jr.Steps {
				if s.Name == SetUpJobName || s.Name == "ermite-dump" {
					continue
				}
				want = append(want, es{s.Name, s.Conclusion, s.Log})
			}
			var have []es
			for _, s := range got.steps {
				if s.Name == SetUpJobName {
					continue
				}
				var log []string
				for _, l := range s.Log {
					log = append(log, normalizeLine(l, home))
				}
				have = append(have, es{s.Name, string(s.Conclusion), log})
			}
			if len(have) != len(want) {
				t.Fatalf("steps: got %v, want %d steps", got.names(), len(want))
			}
			for i := range want {
				w, h := want[i], have[i]
				if h.Name != w.Name || h.Conclusion != w.Conclusion {
					t.Errorf("step %d: got %q (%s), want %q (%s)", i+1, h.Name, h.Conclusion, w.Name, w.Conclusion)
					continue
				}
				if shIsBash() && strings.Contains(strings.Join(w.Log, "\n"), "shell: /usr/bin/sh ") {
					continue // sh is bash here, dash on the image: the output is not comparable
				}
				if strings.Join(h.Log, "\n") != strings.Join(w.Log, "\n") {
					t.Errorf("step %q log differs:\n--- got\n%s\n--- want\n%s", w.Name, strings.Join(h.Log, "\n"), strings.Join(w.Log, "\n"))
				}
			}
			if string(res.Conclusion) != jr.Conclusion && jr.Conclusion != "" {
				// The dump step fails on GitHub when an earlier step failed in
				// some scenarios; the job conclusion is still the one of the steps.
				t.Errorf("job conclusion %s, recording %s", res.Conclusion, jr.Conclusion)
			}
		})
		n++
	}
	if n == 0 {
		t.Fatal("no job compared")
	}
}

// Scenarios whose jobs have no dependencies and whose logs do not depend on
// the machine, in the order of the plan's list (shells, env, outputs, file
// commands, conditions).
func TestCorpusEnvPrecedence(t *testing.T)        { compareWithRecording(t, "env-precedence") }
func TestCorpusEnvExpressions(t *testing.T)       { compareWithRecording(t, "env-expressions") }
func TestCorpusEnvSpecialValues(t *testing.T)     { compareWithRecording(t, "env-special-values") }
func TestCorpusOutGithubEnv(t *testing.T)         { compareWithRecording(t, "out-github-env") }
func TestCorpusOutGithubEnvReserved(t *testing.T) { compareWithRecording(t, "out-github-env-reserved") }
func TestCorpusOutGithubPath(t *testing.T)        { compareWithRecording(t, "out-github-path") }
func TestCorpusOutStepOutputs(t *testing.T)       { compareWithRecording(t, "out-step-outputs") }
func TestCorpusCtxSteps(t *testing.T)             { compareWithRecording(t, "ctx-steps") }
func TestCorpusIfStepConditions(t *testing.T)     { compareWithRecording(t, "if-step-conditions") }
func TestCorpusShellWorkingDirectory(t *testing.T) {
	compareWithRecording(t, "shell-working-directory")
}
func TestCorpusShellExitCodes(t *testing.T) { compareWithRecording(t, "shell-exit-codes") }
func TestCorpusIfJobLevel(t *testing.T) {
	compareWithRecording(t, "if-job-level-fail-effects", "after-job-continue")
}

func envPath() string { return os.Getenv("PATH") }

func TestCorpusShellBuiltin(t *testing.T)    { compareWithRecording(t, "shell-builtin") }
func TestCorpusShellCustom(t *testing.T)     { compareWithRecording(t, "shell-custom") }
func TestCorpusStatusFunctions(t *testing.T) { compareWithRecording(t, "expr-status-functions") }

// GitHub rejects the workflow of expr-status-in-env (status functions are not
// allowed in a step's env): the recorded run has no jobs. The executor must
// refuse it at "Set up job" too.
func TestCorpusStatusInEnvIsRejected(t *testing.T) {
	dir := conformanceDir(t)
	yaml := readFile(t, filepath.Join(dir, "corpus", "expr-status-in-env", "tree", ".github", "workflows", "ci.yml"))
	rec, res := runLocal(t, yaml, "main", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(res.Error, "The workflow is not valid") || len(rec.steps) != 1 {
		t.Fatalf("%+v %v", res, rec.names())
	}
}
