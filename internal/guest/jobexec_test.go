// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"net"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// runJobSession starts Exec on a unix socket and drives it with a job.
func runJobSession(t *testing.T, yaml string, cancelOn string, tweak func(*ExecConfig)) (steps []string, logs []*execpb.Log, res *execpb.JobResult, exit *execpb.Exit) {
	t.Helper()
	return runJobSessionSpec(t, yaml, cancelOn, tweak, nil)
}

// runJobSessionSpec is runJobSession with a change to the JobSpec sent.
func runJobSessionSpec(t *testing.T, yaml string, cancelOn string, tweak func(*ExecConfig), spec func(*execpb.JobSpec)) (steps []string, logs []*execpb.Log, res *execpb.JobResult, exit *execpb.Exit) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_TOOL_CACHE", filepath.Join(home, "toolcache"))
	t.Setenv("ERMITE_CONNECT", "unix:"+sock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cfg := ExecConfig{Target: Target{"unix", sock}, Sandbox: "container", Version: "test", RunTimeout: 5 * time.Second,
			TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond, WaitDelay: 300 * time.Millisecond}
		if tweak != nil {
			tweak(&cfg)
		}
		_, _ = Exec(context.Background(), cfg)
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// With cancelOn, the job is cancelled when it logs that line (the script
	// prints it once it is ready to be interrupted): no timer to guess.
	var cancel chan *execpb.Cancel
	if cancelOn != "" {
		cancel = make(chan *execpb.Cancel, 1)
	}
	var cancelOnce sync.Once
	var mu sync.Mutex
	js := &execpb.JobSpec{
		WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: yaml, JobKey: "j", Repo: "hello", RunnerName: "r", RunnerVersion: "v",
		Contexts: `{"github":{"repository":"octo/hello","event_name":"push","job":"j"}}`, Matrix: "{}",
	}
	if spec != nil {
		spec(js)
	}
	_, exit, err = execstream.DriveJob(conn, &execpb.Run{Job: js}, cancel, execstream.JobHandler{
		StepStarted: func(s *execpb.StepStarted) {
			mu.Lock()
			steps = append(steps, s.Name)
			mu.Unlock()
		},
		Log: func(l *execpb.Log) {
			logs = append(logs, l)
			if cancelOn != "" && l.Text == cancelOn {
				cancelOnce.Do(func() { cancel <- &execpb.Cancel{Reason: "test", Kind: execpb.CancelKind_CANCEL_KIND_USER} })
			}
		},
		JobResult: func(r *execpb.JobResult) { res = r },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	return
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := lookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
}

func TestExecRunsAJob(t *testing.T) {
	requireBash(t)
	steps, logs, res, exit := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    outputs:\n      o: ${{ steps.s.outputs.v }}\n    steps:\n      - id: s\n        run: |\n          echo hello-from-job\n          echo v=42 >> \"$GITHUB_OUTPUT\"\n", "", nil)
	if res == nil || res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || res.Outputs["o"] != "42" || exit.Code != 0 {
		t.Fatalf("res=%v exit=%v", res, exit)
	}
	if strings.Join(steps, "|") != "Set up job|Run echo hello-from-job|Complete job" {
		t.Fatalf("steps %v", steps)
	}
	found := false
	for _, l := range logs {
		if l.Text == "hello-from-job" && l.Step == 2 && l.UnixNano > 0 && !l.Continued {
			found = true
		}
	}
	if !found {
		t.Fatalf("log line missing: %d lines", len(logs))
	}
}

func TestExecJobFailureExitsNonZero(t *testing.T) {
	requireBash(t)
	_, _, res, exit := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: exit 4\n", "", nil)
	if res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_FAILURE || exit.Code == 0 {
		t.Fatalf("res=%v exit=%v", res, exit)
	}
}

func TestExecJobLongLinesAreContinued(t *testing.T) {
	requireBash(t)
	_, logs, res, _ := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: head -c 150000 /dev/zero | tr '\\0' y; echo\n", "", nil)
	if res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS {
		t.Fatalf("%v", res)
	}
	var total int
	var pieces int
	for _, l := range logs {
		if strings.HasPrefix(l.Text, "yyyy") {
			pieces++
			total += len(l.Text)
			if len(l.Text) > execstream.MaxLogLine || (pieces > 1) != l.Continued {
				t.Fatalf("piece %d: len %d continued %v", pieces, len(l.Text), l.Continued)
			}
		}
	}
	if pieces != 3 || total != 150000 {
		t.Fatalf("pieces=%d total=%d", pieces, total)
	}
}

func TestExecJobCancel(t *testing.T) {
	requireBash(t)
	start := time.Now()
	_, _, res, _ := runJobSession(t, "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: |\n          echo ready\n          sleep 30\n", "ready", nil)
	if res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_CANCELLED || res.RunnerShutdown {
		t.Fatalf("%v", res)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("cancel too slow")
	}
}

func TestExecJobHostGoneIsAShutdown(t *testing.T) {
	requireBash(t)
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TOOL_CACHE", filepath.Join(t.TempDir(), "toolcache"))
	done := make(chan int, 1)
	go func() {
		c, _ := Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, RunTimeout: 5 * time.Second,
			TermAfter: 100 * time.Millisecond, KillAfter: 100 * time.Millisecond, WaitDelay: 200 * time.Millisecond})
		done <- c
	}()
	conn, _ := ln.Accept()
	c := execstream.NewConn(conn)
	if _, err := c.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = c.Send(&execpb.Frame{Body: &execpb.Frame_Run{Run: &execpb.Run{Job: &execpb.JobSpec{
		WorkflowYaml: "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: echo running; sleep 60\n", JobKey: "j", Repo: "hello",
		Contexts: `{"github":{"repository":"o/hello"}}`,
	}}}})
	// Close the stream once the step is running (its first line arrived).
	for {
		f, err := c.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if l := f.GetLog(); l != nil && l.Text == "running" {
			break
		}
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the executor kept running without a host")
	}
}

func TestStepEnv(t *testing.T) {
	got := stepEnv([]string{"PATH=/bin", "HOME=/root", "ERMITE_CONNECT=x", "USER=root", "RUNNER_TOOL_CACHE=/x", "FOO=1"}, StepUser{Name: "runner", Home: "/home/runner"})
	want := "PATH=/bin FOO=1 HOME=/home/runner USER=runner LOGNAME=runner"
	if strings.Join(got, " ") != want {
		t.Fatalf("%v", got)
	}
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }

func TestMissingStepUserFailsTheJobUnlessTestOptionSet(t *testing.T) {
	none := func(string) (*user.User, error) { return nil, user.UnknownUserError("runner") }
	// As root with no such user: refuse.
	if _, err := lookupStepUser(0, none, "runner", false); err == nil || !strings.Contains(err.Error(), `no user "runner"`) || !strings.Contains(err.Error(), "refusing to run them as root") {
		t.Fatalf("%v", err)
	}
	// The test option allows root.
	if su, err := lookupStepUser(0, none, "runner", true); err != nil || su.Name != "root" || su.Credential != nil {
		t.Fatalf("%+v %v", su, err)
	}
	// Not root: the steps run as the current user, whatever the name.
	if su, err := lookupStepUser(1000, none, "runner", false); err != nil || su.Credential != nil {
		t.Fatalf("%+v %v", su, err)
	}
	// As root with the user: switch to it.
	found := func(string) (*user.User, error) {
		return &user.User{Uid: "1001", Gid: "1001", Username: "runner", HomeDir: "/home/runner"}, nil
	}
	su, err := lookupStepUser(0, found, "runner", false)
	if err != nil || su.Credential == nil || su.Credential.Uid != 1001 || su.Credential.Gid != 1001 || su.Home != "/home/runner" {
		t.Fatalf("%+v %v", su, err)
	}
}

// The failure is reported like a failed "Set up job".
func TestFailSetupReportsAFailedFirstStep(t *testing.T) {
	host, guest := net.Pipe()
	got := make(chan []string, 1)
	go func() {
		var frames []string
		c := execstream.NewConn(host)
		for {
			f, err := c.Recv()
			if err != nil {
				got <- frames
				return
			}
			switch b := f.Body.(type) {
			case *execpb.Frame_StepStarted:
				frames = append(frames, "start:"+b.StepStarted.Name)
			case *execpb.Frame_Log:
				frames = append(frames, "log:"+b.Log.Text)
			case *execpb.Frame_StepCompleted:
				frames = append(frames, "done:"+b.StepCompleted.Conclusion.String())
			case *execpb.Frame_JobResult:
				frames = append(frames, "result:"+b.JobResult.Conclusion.String())
			}
		}
	}()
	if code := failSetup(execstream.NewConn(guest), "no user"); code != 1 {
		t.Fatal(code)
	}
	_ = guest.Close()
	want := "start:Set up job|log:##[error]no user|done:STEP_CONCLUSION_FAILURE|result:STEP_CONCLUSION_FAILURE"
	if g := strings.Join(<-got, "|"); g != want {
		t.Fatalf("%s", g)
	}
}

// The executor over the stream: annotations, summary and outcome frames
// arrive before the StepCompleted of their step, only when the host asked.
func TestExecSendsResultsFrames(t *testing.T) {
	requireBash(t)
	yaml := "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: |\n          echo '::warning title=T::careful'\n          echo '# sum' >> \"$GITHUB_STEP_SUMMARY\"\n          exit 1\n        continue-on-error: true\n"
	for _, on := range []bool{true, false} {
		sock := filepath.Join(t.TempDir(), "s")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Skip(err)
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("RUNNER_TOOL_CACHE", filepath.Join(home, "toolcache"))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, Sandbox: "container", Version: "test", RunTimeout: 5 * time.Second,
				TermAfter: 200 * time.Millisecond, KillAfter: 200 * time.Millisecond, WaitDelay: 300 * time.Millisecond})
		}()
		conn, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		var order []string
		var anns *execpb.StepAnnotations
		var sum *execpb.StepSummary
		var done2 *execpb.StepCompleted
		_, _, err = execstream.DriveJob(conn, &execpb.Run{Job: &execpb.JobSpec{
			WorkflowPath: ".github/workflows/ci.yml", WorkflowYaml: yaml, JobKey: "j", Repo: "hello", RunnerName: "r", RunnerVersion: "v",
			Contexts: `{"github":{"repository":"octo/hello","event_name":"push","job":"j"}}`, Matrix: "{}",
			SendAnnotations: on, SendSummaries: on,
		}}, nil, execstream.JobHandler{
			StepAnnotations: func(a *execpb.StepAnnotations) { order = append(order, "ann"); anns = a },
			StepSummary:     func(s *execpb.StepSummary) { order = append(order, "sum"); sum = s },
			StepCompleted: func(s *execpb.StepCompleted) {
				if s.Number == 2 {
					order = append(order, "done")
					done2 = s
				}
			},
		})
		_ = conn.Close()
		_ = ln.Close()
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if done2 == nil || done2.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || done2.Outcome != execpb.StepConclusion_STEP_CONCLUSION_FAILURE {
			t.Fatalf("on=%v: %+v", on, done2)
		}
		if !on {
			if len(order) != 1 {
				t.Errorf("frames without the features: %v", order)
			}
			continue
		}
		if strings.Join(order, ",") != "ann,sum,done" {
			t.Errorf("order %v", order)
		}
		// the failing exit code is also an annotation
		var msgs []string
		for _, a := range anns.Annotations {
			msgs = append(msgs, a.Level.String()+":"+a.Title+":"+a.Message)
		}
		if anns.Step != 2 || len(msgs) != 2 || msgs[0] != "ANNOTATION_LEVEL_WARNING:T:careful" || msgs[1] != "ANNOTATION_LEVEL_FAILURE::Process completed with exit code 1." {
			t.Errorf("annotations %v", msgs)
		}
		if sum == nil || sum.Step != 2 || sum.Markdown != "# sum\n" {
			t.Errorf("summary %+v", sum)
		}
	}
}

// The permissions the server sent reach "Set up job" through the Run frame as
// the guest reads them: absent keeps the workflow's, present but empty is
// "Metadata only", populated is printed as sent. The frame is marshalled and
// parsed on the way, where a nil and an empty map would look alike.
func TestExecJobPrintsTheServersTokenPermissions(t *testing.T) {
	requireBash(t)
	yaml := "on: push\npermissions: write-all\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: echo hi\n"
	group := func(logs []*execpb.Log) string {
		var lines []string
		in := false
		for _, l := range logs {
			switch {
			case l.Text == "##[group]GITHUB_TOKEN Permissions":
				in = true
			case l.Text == "##[endgroup]":
				in = false
			case in:
				lines = append(lines, l.Text)
			}
		}
		return strings.Join(lines, "\n")
	}
	for _, tc := range []struct {
		name string
		set  func(*execpb.JobSpec)
		want string
	}{
		{"absent: from the workflow", nil, "Actions: write\nArtifactMetadata: write\nAttestations: write\nChecks: write\nCodeQuality: write\nContents: write\n" +
			"CopilotRequests: write\nDeployments: write\nDiscussions: write\nDrives: write\nIssues: write\nMetadata: read\nModels: read\nPackages: write\n" +
			"Pages: write\nPullRequests: write\nRepositoryProjects: write\nSecurityEvents: write\nStatuses: write\nVulnerabilityAlerts: read"},
		{"present and empty: metadata only", func(s *execpb.JobSpec) { s.TokenPermissions = &execpb.TokenPermissions{} }, "Metadata: read"},
		{"populated: as sent", func(s *execpb.JobSpec) {
			s.TokenPermissions = &execpb.TokenPermissions{Levels: map[string]string{"contents": "read", "issues": "none"}}
		}, "Contents: read\nMetadata: read"},
	} {
		_, logs, res, _ := runJobSessionSpec(t, yaml, "", nil, tc.set)
		if res == nil || res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS {
			t.Fatalf("%s: %+v", tc.name, res)
		}
		if got := group(logs); got != tc.want {
			t.Errorf("%s:\n%s\nwant:\n%s", tc.name, got, tc.want)
		}
	}
}

func TestTokenPermissionsNilVersusEmpty(t *testing.T) {
	for name, tc := range map[string]struct {
		spec    *execpb.JobSpec
		wantNil bool
		want    int
	}{
		"absent":    {&execpb.JobSpec{}, true, 0},
		"empty":     {&execpb.JobSpec{TokenPermissions: &execpb.TokenPermissions{}}, false, 0},
		"populated": {&execpb.JobSpec{TokenPermissions: &execpb.TokenPermissions{Levels: map[string]string{"contents": "read"}}}, false, 1},
	} {
		// After the trip a frame makes.
		raw, err := proto.Marshal(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		var got execpb.JobSpec
		if err := proto.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		m := tokenPermissions(&got)
		if (m == nil) != tc.wantNil || len(m) != tc.want {
			t.Errorf("%s: %#v", name, m)
		}
	}
}

// JobSpec.id_token_url and id_token_request_token reach the run steps as
// ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN, the token
// masked in every log line.
func TestExecJobGetsIDTokenRequest(t *testing.T) {
	requireBash(t)
	const url, token = "http://ermite.example:8088/-/oidc/token?api-version=2.0", "ermiteoidc_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	yaml := "on: push\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: echo \"oidc [$ACTIONS_ID_TOKEN_REQUEST_URL] [$ACTIONS_ID_TOKEN_REQUEST_TOKEN]\"\n"
	_, logs, res, _ := runJobSessionSpec(t, yaml, "", nil, func(s *execpb.JobSpec) { s.IdTokenUrl, s.IdTokenRequestToken = url, token })
	if res == nil || res.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l.Text, token) {
			t.Errorf("the token is in the log: %q", l.Text)
		}
		found = found || l.Text == "oidc ["+url+"] [***]"
	}
	if !found {
		t.Error("the step did not print the variables")
	}
}
