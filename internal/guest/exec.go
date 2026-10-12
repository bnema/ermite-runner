// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// DefaultScript is what the stub executor runs when the host sends no script.
const DefaultScript = "uname -a; id; cat /etc/os-release; docker info"

// ExecConfig configures the executor.
type ExecConfig struct {
	Target  Target
	Sandbox string // "vm" or "container", reported in Hello
	Version string
	// Shell runs the script as `Shell -c script`; default /bin/sh.
	Shell string
	// ConnectTimeout bounds the connection attempts (default 30 s).
	ConnectTimeout time.Duration
	// RunTimeout bounds the wait for the host's Run frame (default 60 s).
	RunTimeout time.Duration
}

// Exec is `ermite-runner exec`, the CI-1 stub executor: it connects back to the
// host, receives one Run, runs the script with the shell, streams stdout and
// stderr as Output frames, and ends with an Exit frame. It returns the exit code
// it reported. A Cancel frame sends SIGINT to the script's process group.
func Exec(ctx context.Context, cfg ExecConfig) (int, error) {
	if cfg.Shell == "" {
		cfg.Shell = "/bin/sh"
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 30 * time.Second
	}
	if cfg.RunTimeout == 0 {
		cfg.RunTimeout = 60 * time.Second
	}
	raw, err := cfg.Target.Dial(ctx, cfg.ConnectTimeout)
	if err != nil {
		return 1, err
	}
	defer func() { _ = raw.Close() }()
	return serve(ctx, raw, cfg)
}

func serve(ctx context.Context, rw io.ReadWriteCloser, cfg ExecConfig) (int, error) {
	conn := execstream.NewConn(rw)
	host, _ := os.Hostname()
	if err := conn.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{
		Version: cfg.Version, Sandbox: cfg.Sandbox, Hostname: host,
	}}}); err != nil {
		return 1, fmt.Errorf("send hello: %w", err)
	}

	// Wait for Run, bounded: a host that never answers must not pin the sandbox.
	type recv struct {
		f   *execpb.Frame
		err error
	}
	first := make(chan recv, 1)
	go func() { f, err := conn.Recv(); first <- recv{f, err} }()
	var run *execpb.Run
	select {
	case r := <-first:
		if r.err != nil {
			return 1, fmt.Errorf("waiting for run: %w", r.err)
		}
		if run = r.f.GetRun(); run == nil {
			return 1, errors.New("first frame from host is not run")
		}
	case <-time.After(cfg.RunTimeout):
		return 1, errors.New("timed out waiting for the host's run frame")
	case <-ctx.Done():
		return 1, ctx.Err()
	}

	script := run.GetScript()
	if script == "" {
		script = DefaultScript
	}
	cmd := exec.Command(cfg.Shell, "-c", script)
	cmd.Env = os.Environ()
	for k, v := range run.GetEnv() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return sendExit(conn, 1, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return sendExit(conn, 1, err)
	}
	if err := cmd.Start(); err != nil {
		return sendExit(conn, 127, fmt.Errorf("start %s: %w", cfg.Shell, err))
	}

	// Cancel frames arrive while the script runs.
	go func() {
		for {
			f, err := conn.Recv()
			if err != nil {
				// Host went away: stop the script rather than run unobserved.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				return
			}
			if f.GetCancel() != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			}
		}
	}()

	var wg sync.WaitGroup
	pump := func(r io.Reader, kind execpb.StreamKind) {
		defer wg.Done()
		buf := make([]byte, execstream.ChunkSize)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_ = conn.Send(&execpb.Frame{Body: &execpb.Frame_Output{Output: &execpb.Output{
					Stream: kind, Data: append([]byte(nil), buf[:n]...), UnixNano: time.Now().UnixNano(),
				}}})
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, execpb.StreamKind_STREAM_KIND_STDOUT)
	go pump(stderr, execpb.StreamKind_STREAM_KIND_STDERR)
	wg.Wait() // pipes must be drained before Wait closes them
	waitErr := cmd.Wait()
	code := 0
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &ee):
		code = ee.ExitCode()
		if code < 0 { // killed by a signal: 128+signal like a shell
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			} else {
				code = 1
			}
		}
	default:
		return sendExit(conn, 1, waitErr)
	}
	return sendExit(conn, code, nil)
}

func sendExit(c *execstream.Conn, code int, cause error) (int, error) {
	e := &execpb.Exit{Code: int32(code)}
	if cause != nil {
		e.Error = cause.Error()
	}
	if err := c.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: e}}); err != nil {
		return code, fmt.Errorf("send exit: %w", err)
	}
	return code, nil
}
