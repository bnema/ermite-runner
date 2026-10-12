// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

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
func host(t *testing.T, script string, ctxFn func(context.Context) context.Context) (out, errOut string, exit *execpb.Exit, hello *execpb.Hello, code int, execErr error) {
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
		c, err := Exec(context.Background(), ExecConfig{Target: Target{"unix", sock}, Sandbox: "container", Version: "test", Shell: "/bin/sh", RunTimeout: 5 * time.Second})
		done <- res{c, err}
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	if ctxFn != nil {
		ctx = ctxFn(ctx)
	}
	var so, se strings.Builder
	hello, exit, err = execstream.Drive(ctx, conn, &execpb.Run{Script: script, Env: map[string]string{"STUB_VAR": "from-host"}}, func(o *execpb.Output) {
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
	out, errOut, exit, hello, code, err := host(t, `echo out-line; echo err-line >&2; echo "$STUB_VAR"; exit 3`, nil)
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
	out, errOut, exit, _, _, _ := host(t, "", nil)
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
	out, _, exit, _, _, _ := host(t, `head -c 300000 /dev/zero | tr '\0' 'x'`, nil)
	if len(out) != 300000 || exit.Code != 0 {
		t.Fatalf("len=%d exit=%v", len(out), exit)
	}
}

func TestExecCancelSendsSIGINT(t *testing.T) {
	start := time.Now()
	out, _, exit, _, _, _ := host(t, `trap 'echo got-int; exit 130' INT; echo ready; while :; do sleep 0.05; done`, func(ctx context.Context) context.Context {
		c, cancel := context.WithCancel(ctx)
		time.AfterFunc(500*time.Millisecond, cancel)
		return c
	})
	if !strings.Contains(out, "got-int") || exit.Code != 130 {
		t.Fatalf("out=%q exit=%v", out, exit)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation too slow")
	}
}

func TestExecKilledBySignalReports128PlusSignal(t *testing.T) {
	_, _, exit, _, _, _ := host(t, `kill -TERM $$`, nil)
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
		_, exit, _ := execstream.Drive(context.Background(), c, &execpb.Run{Script: "true"}, nil)
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
		_, _, _ = execstream.Drive(context.Background(), c, &execpb.Run{Script: "exit 0"}, nil)
	}()
	code := Init(context.Background(), InitConfig{
		Cmdline:       "ermite.sandbox=vm ermite.connect=unix:" + sock,
		Getpid:        func() int { return 4242 },
		ExecArgs:      []string{"/bin/sh", "-c", "exit 5"},
		DockerTimeout: time.Millisecond,
		SkipDocker:    true,
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
