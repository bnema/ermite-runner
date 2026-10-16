// SPDX-License-Identifier: MIT

package resultssvc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/cachesvc"
)

// The checkouts of actions/upload-artifact and actions/download-artifact
// (v4 or later: action.yml and the built dist/) that TestArtifactActions runs
// against the service: the protocol as @actions/artifact speaks it, not as
// the other tests of this package spell it. The test needs node 20 or later
// (ERMITE_TEST_NODE, else node from PATH) and no network; without the
// checkouts it is skipped.
const (
	envUploadDist   = "ERMITE_TEST_UPLOAD_ARTIFACT_DIST"
	envDownloadDist = "ERMITE_TEST_DOWNLOAD_ARTIFACT_DIST"
	envNode         = "ERMITE_TEST_NODE"
)

// chJob is a job of TestArtifactActions: its Job, the URL of its channel and
// its runtime token.
type chJob struct {
	Job
	url   string
	token string
}

// manifest is what the test needs of an action.yml: the script and the input
// defaults.
type manifest struct {
	main     string
	defaults map[string]string
}

// readManifest reads action.yml with a few lines of parsing: the inputs'
// "default:" keys and runs.main, which is all these manifests hold that the
// test uses.
func readManifest(t *testing.T, dir string) manifest {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	m := manifest{defaults: map[string]string{}}
	unquote := func(v string) string {
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v
	}
	section, input := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case indent == 0:
			section, _, _ = strings.Cut(trimmed, ":")
		case section == "inputs" && indent == 2 && strings.HasSuffix(trimmed, ":"):
			input = strings.TrimSuffix(trimmed, ":")
			m.defaults[input] = ""
		case section == "inputs" && indent == 4 && strings.HasPrefix(trimmed, "default:"):
			m.defaults[input] = unquote(strings.TrimPrefix(trimmed, "default:"))
		case section == "runs" && indent == 2 && strings.HasPrefix(trimmed, "main:"):
			m.main = unquote(strings.TrimPrefix(trimmed, "main:"))
		}
	}
	if m.main == "" {
		t.Fatalf("%s/action.yml: no runs.main", dir)
	}
	return m
}

// TestArtifactActions runs the real upload-artifact and download-artifact
// against the service in front of the fake ermite: round trips of one file,
// of several blocks and of an empty file, merge-multiple, overwrite, a
// download by another job of the run, a token refused on another job's
// channel, and a job of another run that sees nothing. Tokens and channels are
// cachesvc's, as in production.
func TestArtifactActions(t *testing.T) {
	upDir, downDir := os.Getenv(envUploadDist), os.Getenv(envDownloadDist)
	if upDir == "" || downDir == "" {
		t.Skip(envUploadDist + " and " + envDownloadDist + " are not set (checkouts of actions/upload-artifact and actions/download-artifact)")
	}
	node := os.Getenv(envNode)
	if node == "" {
		var err error
		if node, err = exec.LookPath("node"); err != nil {
			t.Skip("node not found")
		}
	}
	out, err := exec.Command(node, "--version").Output()
	major, _ := strconv.Atoi(strings.TrimPrefix(strings.SplitN(strings.TrimSpace(string(out)), ".", 2)[0], "v"))
	if err != nil || major < 20 {
		t.Skipf("node %q is not 20 or later", strings.TrimSpace(string(out)))
	}
	upload, download := readManifest(t, upDir), readManifest(t, downDir)

	// The production path: cachesvc mints and checks the runtime tokens and
	// serves each job's channel, with the results service of that job beside
	// the cache.
	cache, err := cachesvc.New(cachesvc.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.CloseAll() })
	f := newFakeErmite()
	ermiteURL := f.serve(t)
	results := New(Config{Auth: cache})
	root := t.TempDir()
	const run = 1001
	nJobs := 0
	// newJob registers a job with ermite and serves its channel on a TCP
	// listener of its own.
	newJob := func(run int64, id string) *chJob {
		t.Helper()
		nJobs++
		et := "ermitejob_" + strconv.Itoa(nJobs)
		f.addJob(et, run, id, 1)
		j := Job{ID: id, RunID: run, Attempt: 1, Client: client(ermiteURL, et)}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.Serve("ch-"+id, ln, id, cachesvc.WithResults(results.Handler(j))); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cache.Stop("ch-" + id) })
		return &chJob{Job: j, url: "http://" + ln.Addr().String(), token: cache.Token(id, cachesvc.Scope{}, time.Hour, Scope(run, id, 1))}
	}
	build, test := newJob(run, "build"), newJob(run, "test")
	intruder := newJob(run+1, "intruder")

	ws := func(j *chJob) string { return filepath.Join(root, "ws-"+j.ID) }
	write := func(j *chJob, rel string, data []byte) {
		t.Helper()
		p := filepath.Join(ws(j), rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(j *chJob, rel string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(ws(j), rel))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	outputRE := regexp.MustCompile(`(?ms)^(\S+)<<(\S+)\n(.*?)\n\S+$`)

	// step runs the main script of an action as the runner would run it for
	// job j, and returns whether it succeeded, its outputs and its log.
	step := func(dir string, m manifest, j *chJob, inputs map[string]string) (bool, map[string]string, string) {
		t.Helper()
		tmp := filepath.Join(root, "tmp-"+j.ID)
		for _, d := range []string{ws(j), tmp} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		files := map[string]string{"output": "", "env": "", "summary": "", "event.json": "{}"}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(tmp, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		runID := strconv.FormatInt(j.RunID, 10)
		env := []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + tmp,
			"GITHUB_SERVER_URL=https://ermite.ghe.localhost", "GITHUB_REPOSITORY=bnema/app",
			"GITHUB_RUN_ID=" + runID, "GITHUB_RUN_ATTEMPT=" + strconv.Itoa(int(j.Attempt)), "GITHUB_JOB=" + j.ID,
			"GITHUB_WORKSPACE=" + ws(j), "RUNNER_TEMP=" + tmp, "RUNNER_OS=Linux",
			"GITHUB_OUTPUT=" + filepath.Join(tmp, "output"), "GITHUB_ENV=" + filepath.Join(tmp, "env"),
			"GITHUB_STEP_SUMMARY=" + filepath.Join(tmp, "summary"), "GITHUB_EVENT_PATH=" + filepath.Join(tmp, "event.json"),
			"ACTIONS_RESULTS_URL=" + j.url + "/", "ACTIONS_RUNTIME_TOKEN=" + j.token,
		}
		for name, def := range m.defaults {
			v, ok := inputs[name]
			if !ok {
				v = strings.NewReplacer("${{ github.repository }}", "bnema/app", "${{ github.run_id }}", runID, "${{ github.token }}", "").Replace(def)
			}
			env = append(env, "INPUT_"+strings.ToUpper(strings.ReplaceAll(name, " ", "_"))+"="+v)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		// node itself, not a shell: a shell would drop the INPUT_ variables
		// whose names have a '-'.
		cmd := exec.CommandContext(ctx, node, filepath.Join(dir, m.main))
		cmd.Dir, cmd.Env = ws(j), env
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		raw, _ := os.ReadFile(filepath.Join(tmp, "output"))
		outs := map[string]string{}
		for _, m := range outputRE.FindAllStringSubmatch(string(raw), -1) {
			outs[m[1]] = m[3]
		}
		return err == nil, outs, buf.String()
	}
	up := func(j *chJob, inputs map[string]string) (bool, map[string]string, string) {
		t.Helper()
		return step(upDir, upload, j, inputs)
	}
	down := func(j *chJob, inputs map[string]string) (bool, string) {
		t.Helper()
		ok, _, log := step(downDir, download, j, inputs)
		return ok, log
	}
	random := func(n int) []byte {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return b
	}

	t.Run("single file", func(t *testing.T) {
		write(build, "one.txt", []byte("hello artifacts\n"))
		ok, outs, log := up(build, map[string]string{"name": "single", "path": "one.txt"})
		if !ok || outs["artifact-id"] == "" {
			t.Fatalf("upload (%v):\n%s", outs, log)
		}
		// Another job of the run downloads it.
		if ok, log := down(test, map[string]string{"name": "single", "path": "got-single"}); !ok {
			t.Fatalf("download:\n%s", log)
		}
		if got := read(test, "got-single/one.txt"); string(got) != "hello artifacts\n" {
			t.Fatalf("downloaded %q", got)
		}
	})

	t.Run("several blocks", func(t *testing.T) {
		big := random(20<<20 + 123)
		write(build, "big/rand.bin", big)
		if ok, _, log := up(build, map[string]string{"name": "big", "path": "big/rand.bin", "compression-level": "0"}); !ok {
			t.Fatalf("upload:\n%s", log)
		}
		if ok, log := down(test, map[string]string{"name": "big", "path": "got-big"}); !ok {
			t.Fatalf("download:\n%s", log)
		}
		if !bytes.Equal(read(test, "got-big/rand.bin"), big) {
			t.Fatal("the downloaded file differs")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		write(build, "empty/zero.txt", nil)
		if ok, _, log := up(build, map[string]string{"name": "empty", "path": "empty/zero.txt"}); !ok {
			t.Fatalf("upload:\n%s", log)
		}
		if ok, log := down(test, map[string]string{"name": "empty", "path": "got-empty"}); !ok {
			t.Fatalf("download:\n%s", log)
		}
		if got := read(test, "got-empty/zero.txt"); len(got) != 0 {
			t.Fatalf("downloaded %q", got)
		}
	})

	t.Run("merge multiple", func(t *testing.T) {
		for i := range 3 {
			n := strconv.Itoa(i)
			write(build, "part"+n+"/p"+n+".txt", []byte("part "+n+"\n"))
			if ok, _, log := up(build, map[string]string{"name": "part-" + n, "path": "part" + n + "/"}); !ok {
				t.Fatalf("upload %d:\n%s", i, log)
			}
		}
		if ok, log := down(test, map[string]string{"pattern": "part-*", "merge-multiple": "true", "path": "merged"}); !ok {
			t.Fatalf("download:\n%s", log)
		}
		for i := range 3 {
			n := strconv.Itoa(i)
			if got := read(test, "merged/p"+n+".txt"); string(got) != "part "+n+"\n" {
				t.Fatalf("p%d.txt: %q", i, got)
			}
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		write(build, "one.txt", []byte("hello artifacts v2\n"))
		ok, _, log := up(build, map[string]string{"name": "single", "path": "one.txt"})
		if ok || !strings.Contains(log, "409") {
			t.Fatalf("a second upload of the name without overwrite must fail with a 409:\n%s", log)
		}
		if ok, _, log := up(build, map[string]string{"name": "single", "path": "one.txt", "overwrite": "true"}); !ok {
			t.Fatalf("overwrite:\n%s", log)
		}
		if ok, log := down(test, map[string]string{"name": "single", "path": "got-single-2"}); !ok {
			t.Fatalf("download:\n%s", log)
		}
		if got := read(test, "got-single-2/one.txt"); string(got) != "hello artifacts v2\n" {
			t.Fatalf("downloaded %q", got)
		}
	})

	t.Run("archive false", func(t *testing.T) {
		if _, ok := upload.defaults["archive"]; !ok {
			t.Skip("upload-artifact before v7 has no archive input")
		}
		write(build, "raw/report.html", []byte("<h1>raw</h1>\n"))
		if ok, _, log := up(build, map[string]string{"path": "raw/report.html", "archive": "false"}); !ok {
			t.Fatalf("upload:\n%s", log)
		}
		if ok, log := down(test, map[string]string{"name": "report.html", "path": "got-raw"}); !ok {
			t.Fatalf("download:\n%s", log)
		}
		if got := read(test, "got-raw/report.html"); string(got) != "<h1>raw</h1>\n" {
			t.Fatalf("downloaded %q", got)
		}
	})

	t.Run("token of another channel", func(t *testing.T) {
		// test's token on build's channel: the channel accepts only build's.
		stray := &chJob{Job: test.Job, url: build.url, token: test.token}
		ok, log := down(stray, map[string]string{"name": "single", "path": "stray"})
		if ok || !strings.Contains(log, "401") {
			t.Fatalf("a token used on another job's channel must be refused with a 401:\n%s", log)
		}
	})

	t.Run("another run", func(t *testing.T) {
		if ok, log := down(intruder, map[string]string{"name": "single", "path": "x"}); ok {
			t.Fatalf("a job of another run downloaded the artifact:\n%s", log)
		}
		ok, log := down(intruder, map[string]string{"path": "all"})
		if !ok {
			t.Fatalf("download all:\n%s", log)
		}
		if entries, _ := os.ReadDir(filepath.Join(ws(intruder), "all")); len(entries) != 0 {
			t.Fatalf("a job of another run got %d artifacts", len(entries))
		}
	})
}
