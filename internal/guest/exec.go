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

// Cancellation timings, like the official runner's ProcessInvoker: SIGINT, then
// SIGTERM after 7.5 s, then SIGKILL 2.5 s later.
const (
	DefaultTermAfter = 7500 * time.Millisecond
	DefaultKillAfter = 2500 * time.Millisecond
	// DefaultWaitDelay bounds how long the executor waits for the output pipes
	// to close after the script's shell exited (background children may hold
	// them open).
	DefaultWaitDelay = 500 * time.Millisecond
)

// ExecConfig configures the executor.
type ExecConfig struct {
	Target  Target
	Sandbox string // "vm" or "container", reported in Hello
	Version string
	// Shell runs the script as `Shell -c script`; default /bin/sh.
	Shell string
	// StepUser is the account the steps of a job run as when the executor is
	// root; default "runner".
	StepUser string
	// ConnectTimeout bounds the connection attempts (default 30 s).
	ConnectTimeout time.Duration
	// RunTimeout bounds the wait for the host's Run frame (default 60 s).
	RunTimeout time.Duration
	// TermAfter and KillAfter are the escalation delays after a Cancel (or after
	// the executor's own context ends): SIGINT at once, SIGTERM after TermAfter,
	// SIGKILL KillAfter later. WaitDelay is the pipe-drain grace after the shell
	// exits. Zero means the defaults above.
	TermAfter, KillAfter, WaitDelay time.Duration

	// signal sends a signal to a process group; tests replace it to observe the
	// escalation. Default: syscall.Kill(-pgid, sig).
	signal func(pgid int, sig syscall.Signal)
}

func (c *ExecConfig) defaults() {
	if c.Shell == "" {
		c.Shell = "/bin/sh"
	}
	if c.StepUser == "" {
		c.StepUser = "runner"
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 30 * time.Second
	}
	if c.RunTimeout == 0 {
		c.RunTimeout = 60 * time.Second
	}
	if c.TermAfter == 0 {
		c.TermAfter = DefaultTermAfter
	}
	if c.KillAfter == 0 {
		c.KillAfter = DefaultKillAfter
	}
	if c.WaitDelay == 0 {
		c.WaitDelay = DefaultWaitDelay
	}
	if c.signal == nil {
		c.signal = func(pgid int, sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }
	}
}

// Exec is `ermite-runner exec`: it connects back to the host and receives one
// Run. A Run with a job is executed by package executor (see runJob); a Run
// without one is the stub of sandbox-check, which runs the script with the shell, streams stdout and
// stderr as Output frames, and ends with an Exit frame. It returns the exit code
// it reported. A Cancel frame, or the end of ctx, stops the script's process
// group with escalating signals (see ExecConfig).
func Exec(ctx context.Context, cfg ExecConfig) (int, error) {
	cfg.defaults()
	raw, err := cfg.Target.Dial(ctx, cfg.ConnectTimeout)
	if err != nil {
		return 1, err
	}
	defer func() { _ = raw.Close() }()
	return serve(ctx, raw, cfg)
}

// frameWriter turns writes into Output frames of at most ChunkSize bytes. It is
// the command's Stdout/Stderr, so no goroutine of ours sits between the pipe
// and the stream.
type frameWriter struct {
	conn *execstream.Conn
	kind execpb.StreamKind
}

func (w frameWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > execstream.ChunkSize {
			chunk = chunk[:execstream.ChunkSize]
		}
		err := w.conn.Send(&execpb.Frame{Body: &execpb.Frame_Output{Output: &execpb.Output{
			Stream: w.kind, Data: append([]byte(nil), chunk...), UnixNano: time.Now().UnixNano(),
		}}})
		if err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// escalator signals a process group and escalates after a cancel. Once finish
// has been called (the shell was reaped) it never signals again: the group id
// could have been reused by then.
type escalator struct {
	cfg  ExecConfig
	pgid int

	mu       sync.Mutex
	finished bool
	started  bool
	timers   []*time.Timer
}

func (e *escalator) send(sig syscall.Signal) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.finished {
		e.cfg.signal(e.pgid, sig)
	}
}

// cancel starts the escalation; only the first call does anything.
func (e *escalator) cancel() {
	e.mu.Lock()
	if e.finished || e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.cfg.signal(e.pgid, syscall.SIGINT)
	e.timers = append(e.timers,
		time.AfterFunc(e.cfg.TermAfter, func() { e.send(syscall.SIGTERM) }),
		time.AfterFunc(e.cfg.TermAfter+e.cfg.KillAfter, func() { e.send(syscall.SIGKILL) }),
	)
	e.mu.Unlock()
}

// finish is called right after the shell was reaped: it kills what is left of
// the group (background children), stops the timers and forbids further signals.
func (e *escalator) finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return
	}
	e.cfg.signal(e.pgid, syscall.SIGKILL)
	e.finished = true
	for _, t := range e.timers {
		t.Stop()
	}
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

	if run.GetJob() != nil {
		code := runJob(ctx, conn, cfg, run.GetJob())
		return sendExit(conn, code, nil)
	}

	// Without a job this is the stub executor of sandbox-check.
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
	// Frame writers as stdout/stderr: exec copies the pipes for us, and
	// WaitDelay bounds the wait when a background child keeps a pipe open.
	cmd.Stdout = frameWriter{conn, execpb.StreamKind_STREAM_KIND_STDOUT}
	cmd.Stderr = frameWriter{conn, execpb.StreamKind_STREAM_KIND_STDERR}
	cmd.WaitDelay = cfg.WaitDelay
	if err := cmd.Start(); err != nil {
		return sendExit(conn, 127, fmt.Errorf("start %s: %w", cfg.Shell, err))
	}
	esc := &escalator{cfg: cfg, pgid: cmd.Process.Pid}

	// Frames from the host while the script runs: a Cancel starts the escalation;
	// a lost host kills the script rather than let it run unobserved.
	go func() {
		for {
			f, err := conn.Recv()
			if err != nil {
				esc.send(syscall.SIGKILL)
				return
			}
			if f.GetCancel() != nil {
				esc.cancel()
			}
		}
	}()
	// The executor's own context (SIGTERM forwarded by init) cancels the same way.
	stopCtx := context.AfterFunc(ctx, esc.cancel)
	defer stopCtx()

	waitErr := cmd.Wait()
	esc.finish()

	code := 0
	var ee *exec.ExitError
	switch {
	case waitErr == nil, errors.Is(waitErr, exec.ErrWaitDelay):
		// ErrWaitDelay: the shell exited cleanly but a background child kept the
		// pipes open; the group was killed above.
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
