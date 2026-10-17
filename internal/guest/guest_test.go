// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestParseCmdline(t *testing.T) {
	got := ParseCmdline("console=ttyS0 ro  ermite.connect=vsock:1024 x=\"a b\" ermite.gw=10.0.0.1 ermite.gw=10.0.0.9 -- init\n")
	want := map[string]string{"console": "ttyS0", "ro": "", "ermite.connect": "vsock:1024", "x": "a b", "ermite.gw": "10.0.0.9", "--": "", "init": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if len(ParseCmdline("")) != 0 {
		t.Fatal("empty command line")
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]Target{
		"unix:/run/ermite/exec.sock": {"unix", "/run/ermite/exec.sock"},
		"vsock:1024":                 {"vsock", "1024"},
	} {
		got, err := ParseTarget(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "unix", "unix:", "vsock:0", "vsock:abc", "tcp:1.2.3.4:5", "vsock:99999999999"} {
		if _, err := ParseTarget(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// host plays the host process on a unix socket and runs one session.
func host(t *testing.T, script string, cancelAfter time.Duration, tweak func(*ExecConfig)) (out, errOut string, exit *execpb.Exit, hello *execpb.Hello, code int, execErr error) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	type res struct {
		code int
		err  error
	}
	done := make(chan res, 1)
	go func() {
		cfg := ExecConfig{Target: Target{"unix", sock}, Sandbox: "container", Version: "test", Shell: "/bin/sh", RunTimeout: 5 * time.Second}
		if tweak != nil {
			tweak(&cfg)
		}
		c, err := Exec(context.Background(), cfg)
		done <- res{c, err}
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var cancel chan *execpb.Cancel
	if cancelAfter > 0 {
		cancel = make(chan *execpb.Cancel, 1)
		time.AfterFunc(cancelAfter, func() { cancel <- &execpb.Cancel{Reason: "test"} })
	}
	var so, se strings.Builder
	hello, exit, err = execstream.Drive(conn, &execpb.Run{Script: script, Env: map[string]string{"STUB_VAR": "from-host"}}, cancel, func(o *execpb.Output) {
		if o.Stream == execpb.StreamKind_STREAM_KIND_STDERR {
			se.Write(o.Data)
		} else {
			so.Write(o.Data)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	r := <-done
	return so.String(), se.String(), exit, hello, r.code, r.err
}

func TestExecStreamsOutputAndExitCode(t *testing.T) {
	out, errOut, exit, hello, code, err := host(t, `echo out-line; echo err-line >&2; echo "$STUB_VAR"; exit 3`, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "out-line\nfrom-host\n" || errOut != "err-line\n" {
		t.Fatalf("out=%q err=%q", out, errOut)
	}
	if exit.Code != 3 || code != 3 || exit.Error != "" {
		t.Fatalf("exit=%v code=%d", exit, code)
	}
	if hello.Sandbox != "container" || hello.Version != "test" || hello.Hostname == "" {
		t.Fatalf("hello=%v", hello)
	}
}

func TestExecDefaultScriptRunsDiagnostics(t *testing.T) {
	// The default script is the CI-1 stub: uname, id, os-release, docker info.
	// docker may be absent here: only the layout of the output is checked.
	out, errOut, exit, _, _, _ := host(t, "", 0, nil)
	if !strings.Contains(out, "Linux") || !strings.Contains(out, "uid=") {
		t.Fatalf("uname/id missing: out=%q err=%q", out, errOut)
	}
	if exit == nil {
		t.Fatal("no exit frame")
	}
	if !strings.Contains(DefaultScript, "docker info") || !strings.Contains(DefaultScript, "os-release") {
		t.Fatal("default script must run what the plan says")
	}
}

func TestExecLargeOutputIsChunkedAndComplete(t *testing.T) {
	out, _, exit, _, _, _ := host(t, `head -c 300000 /dev/zero | tr '\0' 'x'`, 0, nil)
	if len(out) != 300000 || exit.Code != 0 {
		t.Fatalf("len=%d exit=%v", len(out), exit)
	}
}

func TestExecCancelSendsSIGINT(t *testing.T) {
	start := time.Now()
	out, _, exit, _, _, _ := host(t, `trap 'echo got-int; exit 130' INT; echo ready; while :; do sleep 0.05; done`, 500*time.Millisecond, nil)
	if !strings.Contains(out, "got-int") || exit.Code != 130 {
		t.Fatalf("out=%q exit=%v", out, exit)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation too slow")
	}
}

// sigRecorder replaces the signal function: it records (signal, time since
// start) and still delivers the signal.
type sigRecorder struct {
	mu    sync.Mutex
	start time.Time
	got   []string
	times []time.Duration
}

func (r *sigRecorder) fn(pgid int, sig syscall.Signal) {
	r.mu.Lock()
	r.got = append(r.got, sig.String())
	r.times = append(r.times, time.Since(r.start))
	r.mu.Unlock()
	_ = syscall.Kill(-pgid, sig)
}

// C1: a script that ignores SIGINT and SIGTERM is stopped by the escalation:
// SIGINT at once, SIGTERM after TermAfter, SIGKILL KillAfter later.
func TestExecCancelEscalatesToKill(t *testing.T) {
	rec := &sigRecorder{start: time.Now()}
	termAfter, killAfter := 400*time.Millisecond, 300*time.Millisecond
	_, _, exit, _, _, _ := host(t, `trap '' INT TERM; echo ready; while :; do sleep 0.05; done`, 300*time.Millisecond, func(c *ExecConfig) {
		c.TermAfter, c.KillAfter, c.signal = termAfter, killAfter, rec.fn
	})
	if exit.Code != 128+int32(syscall.SIGKILL) {
		t.Fatalf("a script ignoring INT and TERM must end by SIGKILL, exit=%v", exit)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	// SIGINT, SIGTERM, SIGKILL (from the escalation), then the final group kill
	// issued after the shell was reaped.
	if len(rec.got) < 3 || rec.got[0] != "interrupt" || rec.got[1] != "terminated" || rec.got[2] != "killed" {
		t.Fatalf("signals: %v", rec.got)
	}
	cancelAt := rec.times[0]
	if d := rec.times[1] - cancelAt; d < termAfter-50*time.Millisecond || d > termAfter+400*time.Millisecond {
		t.Errorf("SIGTERM %v after SIGINT, want about %v", d, termAfter)
	}
	if d := rec.times[2] - rec.times[1]; d < killAfter-50*time.Millisecond || d > killAfter+400*time.Millisecond {
		t.Errorf("SIGKILL %v after SIGTERM, want about %v", d, killAfter)
	}
}

// C1 + C9: a script that exits on SIGINT stops the escalation: the timers are
// stopped when Wait returns, so no SIGTERM/SIGKILL follows, and nothing is
// signalled after the shell was reaped except the final group sweep.
func TestExecEscalationStopsWhenScriptExits(t *testing.T) {
	rec := &sigRecorder{start: time.Now()}
	_, _, exit, _, _, _ := host(t, `trap 'exit 130' INT; while :; do sleep 0.05; done`, 200*time.Millisecond, func(c *ExecConfig) {
		c.TermAfter, c.KillAfter, c.signal = 300*time.Millisecond, 300*time.Millisecond, rec.fn
	})
	if exit.Code != 130 {
		t.Fatalf("exit=%v", exit)
	}
	time.Sleep(1200 * time.Millisecond) // well past TermAfter+KillAfter
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 2 || rec.got[0] != "interrupt" || rec.got[1] != "killed" {
		t.Fatalf("want SIGINT then the final sweep only, got %v", rec.got)
	}
}

// C9: after the shell was reaped the escalator never signals again, even if its
// timers had fired concurrently.
func TestEscalatorNeverSignalsAfterFinish(t *testing.T) {
	var n atomic.Int32
	e := &escalator{cfg: ExecConfig{TermAfter: time.Millisecond, KillAfter: time.Millisecond, signal: func(int, syscall.Signal) { n.Add(1) }}, pgid: 1 << 30}
	e.cancel()
	e.finish()
	after := n.Load()
	time.Sleep(50 * time.Millisecond)
	e.send(syscall.SIGTERM)
	e.cancel()
	if n.Load() != after {
		t.Fatalf("signalled %d times after finish", n.Load()-after)
	}
}

// C2: a background child that keeps stdout open must not hang the executor: it
// reports the script's exit promptly and the child is killed.
func TestExecBackgroundChildDoesNotHoldTheSession(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	start := time.Now()
	out, _, exit, _, _, _ := host(t, `sleep 30 & echo $! > `+marker+`; echo parent-done`, 0, func(c *ExecConfig) { c.WaitDelay = 300 * time.Millisecond })
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the session was held by a background child for %v", time.Since(start))
	}
	if exit == nil || exit.Code != 0 || !strings.Contains(out, "parent-done") {
		t.Fatalf("exit=%v out=%q", exit, out)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err != nil || strings.Contains(string(st), ") Z") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("background child %d survived the job", pid)
}

// C2: output written by the script before the shell exits is complete even when
// the stream is large and a background child holds the pipe.
func TestExecOutputCompleteWithBackgroundChild(t *testing.T) {
	out, _, exit, _, _, _ := host(t, `sleep 30 & head -c 200000 /dev/zero | tr '\0' 'y'`, 0, func(c *ExecConfig) { c.WaitDelay = 300 * time.Millisecond })
	if exit.Code != 0 || len(out) != 200000 {
		t.Fatalf("len=%d exit=%v", len(out), exit)
	}
}

func TestExecKilledBySignalReports128PlusSignal(t *testing.T) {
	_, _, exit, _, _, _ := host(t, `kill -TERM $$`, 0, nil)
	if exit.Code != 128+int32(syscall.SIGTERM) {
		t.Fatalf("exit=%v", exit)
	}
}

func TestExecMissingShell(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, _ := ln.Accept()
		defer func() { _ = c.Close() }()
		_, exit, _ := execstream.Drive(c, &execpb.Run{Script: "true"}, nil, nil)
		if exit == nil || exit.Code != 127 || exit.Error == "" {
			t.Errorf("want 127 with an error message, got %v", exit)
		}
	}()
	code, _ := Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, Shell: "/nonexistent/sh", RunTimeout: 3 * time.Second})
	if code != 127 {
		t.Fatalf("code=%d", code)
	}
}

func TestExecHostGoneKillsTheScript(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan int, 1)
	go func() {
		c, _ := Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, Shell: "/bin/sh", RunTimeout: 3 * time.Second})
		done <- c
	}()
	conn, _ := ln.Accept()
	g := execstream.NewConn(conn)
	_, _ = g.Recv() // hello
	_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Run{Run: &execpb.Run{Script: "sleep 60"}}})
	time.Sleep(200 * time.Millisecond)
	_ = conn.Close() // the host died
	select {
	case code := <-done:
		if code == 0 {
			t.Fatalf("code=%d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the executor kept running after the host went away")
	}
}

func TestExecConnectTimeout(t *testing.T) {
	start := time.Now()
	code, err := Exec(context.Background(), ExecConfig{Target: Target{"unix", filepath.Join(t.TempDir(), "nobody")}, ConnectTimeout: 300 * time.Millisecond})
	if code == 0 || err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("code=%d err=%v after %v", code, err, time.Since(start))
	}
}

func TestExecRunTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	go func() { c, _ := ln.Accept(); time.Sleep(2 * time.Second); _ = c.Close() }()
	code, err := Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, RunTimeout: 200 * time.Millisecond})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "run frame") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestInitInContainerSkipsVMSetup(t *testing.T) {
	// In a container (not a VM's PID 1) Init must not mount anything: with a
	// command line that says "vm" but a pid that is not 1 it behaves as a
	// container, runs the payload and returns the executor's code.
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _, _ = execstream.Drive(c, &execpb.Run{Script: "exit 0"}, nil, nil)
	}()
	code := Init(context.Background(), InitConfig{
		Cmdline:    "ermite.sandbox=vm ermite.connect=unix:" + sock,
		Getpid:     func() int { return 4242 },
		ExecArgs:   []string{"/bin/sh", "-c", "exit 5"},
		SkipDocker: true,
	})
	// ExecArgs is a stand-in for the executor: its exit code is Init's.
	if code != 5 {
		t.Fatalf("code=%d", code)
	}
}

func TestInitWithoutTargetFails(t *testing.T) {
	if code := Init(context.Background(), InitConfig{Cmdline: "ermite.sandbox=container", Getpid: func() int { return 77 }, ExecArgs: []string{"/bin/true"}, SkipDocker: true}); code == 0 {
		t.Fatal("init without a connect target must fail")
	}
}

func TestWriteResolvConfNeedsGateway(t *testing.T) {
	if err := writeResolvConf(""); err == nil {
		t.Fatal("empty gateway accepted")
	}
}

// C11: init forwards SIGTERM to the executor and finishes with the executor's
// own exit code, instead of dying with its child still running.
func TestInitForwardsSIGTERMToTheExecutor(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan int, 1)
	marker := filepath.Join(t.TempDir(), "got-term")
	go func() {
		done <- Init(context.Background(), InitConfig{
			Cmdline:    "ermite.sandbox=container ermite.connect=unix:/nonexistent",
			Getpid:     func() int { return 4242 },
			SkipDocker: true,
			Signals:    sigs,
			ExecArgs:   []string{"/bin/sh", "-c", `trap 'echo term > ` + marker + `; exit 9' TERM; while :; do sleep 0.05; done`},
		})
	}()
	time.Sleep(500 * time.Millisecond)
	sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 9 {
			t.Fatalf("init must return the executor's exit code, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("init did not finish after SIGTERM")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the executor never received SIGTERM")
	}
}

// fakeDocker serves the Docker API on a temp unix socket from the moment it is
// called; ping decides the /_ping answer.
func fakeDocker(t *testing.T, ping http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", ping)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func sleeper(t *testing.T) int {
	t.Helper()
	pid, err := spawn([]string{"/bin/sh", "-c", "sleep 60"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL); _, _ = wait4NoHang(pid) })
	return pid
}

var testLog = zerowrap.FromCtx(context.Background())

func TestWaitDockerReadyWaitsForPing(t *testing.T) {
	var calls atomic.Int32
	sock := fakeDocker(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 4 { // dockerd is up but still starting
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("OK"))
	})
	start := time.Now()
	if err := waitDockerReady(context.Background(), testLog, sock, sleeper(t), 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 4 || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("returned before the API answered OK (calls=%d)", calls.Load())
	}
}

func TestWaitDockerReadyLateSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	go func() {
		time.Sleep(400 * time.Millisecond)
		l, err := net.Listen("unix", sock)
		if err != nil {
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })}
		go func() { _ = srv.Serve(l) }()
		time.AfterFunc(5*time.Second, func() { _ = srv.Close() })
	}()
	start := time.Now()
	if err := waitDockerReady(context.Background(), testLog, sock, sleeper(t), 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("returned before the socket existed")
	}
}

func TestWaitDockerReadyTimesOut(t *testing.T) {
	sock := fakeDocker(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) })
	start := time.Now()
	err := waitDockerReady(context.Background(), testLog, sock, sleeper(t), 500*time.Millisecond, nil)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("want a timeout error, got %v after %s", err, time.Since(start))
	}
	var exited *dockerdExitError
	var abort *abortBootError
	if errors.As(err, &exited) || errors.As(err, &abort) {
		t.Fatalf("a timeout is neither a dockerd exit nor an abort: %v", err)
	}
}

// A daemon that accepts the connection and never answers must not delay the
// deadline, nor a signal.
func TestWaitDockerReadyStalledServer(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sock := fakeDocker(t, func(http.ResponseWriter, *http.Request) { <-release })

	start := time.Now()
	err := waitDockerReady(context.Background(), testLog, sock, sleeper(t), 500*time.Millisecond, nil)
	var abort *abortBootError
	if err == nil || errors.As(err, &abort) || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("want the deadline error within ~0.5 s, got %v after %s", err, time.Since(start))
	}

	sigs := make(chan os.Signal, 1)
	time.AfterFunc(200*time.Millisecond, func() { sigs <- syscall.SIGTERM })
	start = time.Now()
	err = waitDockerReady(context.Background(), testLog, sock, sleeper(t), 30*time.Second, sigs)
	if !errors.As(err, &abort) || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("want an abort right after the signal, got %v after %s", err, time.Since(start))
	}
}

func TestWaitDockerReadyDockerdExits(t *testing.T) {
	pid, err := spawn([]string{"/bin/sh", "-c", "exit 3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = waitDockerReady(context.Background(), testLog, filepath.Join(t.TempDir(), "none.sock"), pid, 30*time.Second, nil)
	var exited *dockerdExitError
	if !errors.As(err, &exited) || exited.status.ExitStatus() != 3 {
		t.Fatalf("want dockerd exit 3, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not notice the exit promptly")
	}
}

// Orphans that init adopts while it waits must be reaped at once, not left as
// zombies until the wait ends.
func TestWaitDockerReadyReapsAdoptedOrphans(t *testing.T) {
	// A child that exits immediately stands for an adopted orphan.
	orphan, err := spawn([]string{"/bin/sh", "-c", "exit 0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	zombie := func() bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(orphan) + "/stat")
		return err == nil && strings.Contains(string(b), ") Z ")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !zombie() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !zombie() {
		t.Fatal("setup: the orphan did not become a zombie")
	}
	sigs := make(chan os.Signal, 1)
	time.AfterFunc(600*time.Millisecond, func() { sigs <- syscall.SIGTERM })
	done := make(chan error, 1)
	go func() {
		done <- waitDockerReady(context.Background(), testLog, filepath.Join(t.TempDir(), "none.sock"), sleeper(t), 30*time.Second, sigs)
	}()
	// The zombie must vanish while the wait is still running.
	deadline = time.Now().Add(400 * time.Millisecond)
	for zombie() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if zombie() {
		t.Fatal("the adopted orphan stayed a zombie during the wait")
	}
	<-done
}

// A shutdown signal during the wait aborts the boot (exit 128+signal) instead
// of starting the executor; so does a cancelled context.
func TestWaitDockerReadyAbortsOnSignalAndContext(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "none.sock")
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGINT
	var abort *abortBootError
	err := waitDockerReady(context.Background(), testLog, sock, sleeper(t), 30*time.Second, sigs)
	if !errors.As(err, &abort) || abort.code != 128+int(syscall.SIGINT) {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = waitDockerReady(ctx, testLog, sock, sleeper(t), 30*time.Second, nil)
	if !errors.As(err, &abort) || abort.code != 128+int(syscall.SIGTERM) {
		t.Fatalf("err=%v", err)
	}
}

func TestStopDockerdReapsAndZeroIsNoop(t *testing.T) {
	stopDockerd(0)
	pid := sleeper(t)
	stopDockerd(pid)
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
		t.Fatal("dockerd is still there")
	}
}

func TestDockerPingRejectsNonOK(t *testing.T) {
	sock := fakeDocker(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	if dockerPing(context.Background(), client) {
		t.Fatal("a 200 without OK must not count as ready")
	}
}

func initWithFakeDockerd(t *testing.T, sigs <-chan os.Signal, timeout time.Duration, dockerd string) (code int, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "executor-ran")
	code = Init(context.Background(), InitConfig{
		Cmdline:            "ermite.sandbox=container ermite.connect=unix:/nonexistent",
		Getpid:             func() int { return 4242 },
		Signals:            sigs,
		DockerdArgs:        []string{"/bin/sh", "-c", dockerd},
		DockerSocket:       filepath.Join(t.TempDir(), "none.sock"),
		DockerReadyTimeout: timeout,
		ExecArgs:           []string{"/bin/sh", "-c", "touch " + marker + "; exit 7"},
	})
	return code, marker
}

// A shutdown signal while waiting for dockerd: no executor, dockerd stopped,
// exit 128+signal.
func TestInitSignalDuringDockerWaitDoesNotStartTheExecutor(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM
	code, marker := initWithFakeDockerd(t, sigs, 30*time.Second, "sleep 60")
	if code != 128+int(syscall.SIGTERM) {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the executor was started")
	}
}

// The deadline and a dying dockerd still start the executor.
func TestInitStartsTheExecutorWhenDockerNeverGetsReady(t *testing.T) {
	for name, tc := range map[string]struct {
		dockerd string
		timeout time.Duration
	}{"timeout": {"sleep 60", 400 * time.Millisecond}, "dockerd exits": {"exit 1", 30 * time.Second}} {
		t.Run(name, func(t *testing.T) {
			code, marker := initWithFakeDockerd(t, nil, tc.timeout, tc.dockerd)
			if code != 7 {
				t.Fatalf("want the executor's code 7, got %d", code)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("the executor did not run")
			}
		})
	}
}

// A dockerd that ignores SIGTERM goes through the SIGKILL path and is reaped
// (no zombie left).
func TestStopDockerdKillsAndReapsATermIgnoringDaemon(t *testing.T) {
	pid, err := spawn([]string{"/bin/sh", "-c", "trap '' TERM; while :; do sleep 0.1; done"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	time.Sleep(200 * time.Millisecond) // let the shell install its trap
	start := time.Now()
	stopDockerd(pid)
	if d := time.Since(start); d < 5*time.Second || d > 8*time.Second {
		t.Fatalf("expected the SIGKILL path after the 5 s grace, took %s", d)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
		t.Fatal("dockerd was not reaped (zombie or alive)")
	}
}

// A step runs with GitHub's umask 022 even when the executor was started with
// 0, as the kernel starts a VM's init: its files and directories are not
// world-writable.
func TestExecStepsGetUmask022(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	out, _, _, _, code, err := host(t, `umask`, 0, nil)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out) != "0022" {
		t.Fatalf("umask = %q, want 0022", out)
	}
}
