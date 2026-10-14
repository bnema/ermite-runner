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
	// AllowRootSteps lets the steps run as root when StepUser does not exist.
	// For test images only; set by ERMITE_RUNNER_ALLOW_ROOT_STEPS=1 in the
	// executor's environment.
	AllowRootSteps bool
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
	run, vals, err := waitForRun(ctx, conn, cfg.RunTimeout)
	if err != nil {
		return 1, err
	}
	if run.GetJob() != nil {
		return sendExit(conn, runJob(ctx, conn, cfg, run.GetJob(), vals), nil)
	}
	return runStub(ctx, conn, cfg, run)
}

// jobValues are the secrets and variables of the job that the host sent in
// NamedValue frames before Run. They stay in memory.
type jobValues struct{ secrets, vars map[string]string }

// maxJobValues bounds what a host can make the executor keep before Run.
const maxJobValues = 64 << 20

// waitForRun waits for the Run frame of the host, bounded: a host that never
// answers must not pin the sandbox. The NamedValue frames that come before it
// are collected.
func waitForRun(ctx context.Context, conn *execstream.Conn, timeout time.Duration) (*execpb.Run, jobValues, error) {
	type recv struct {
		run  *execpb.Run
		vals jobValues
		err  error
	}
	first := make(chan recv, 1)
	go func() {
		vals := jobValues{secrets: map[string]string{}, vars: map[string]string{}}
		size := 0
		for {
			f, err := conn.Recv()
			if err != nil {
				first <- recv{err: fmt.Errorf("waiting for run: %w", err)}
				return
			}
			switch b := f.Body.(type) {
			case *execpb.Frame_Run:
				first <- recv{run: b.Run, vals: vals}
				return
			case *execpb.Frame_Secret:
				size += len(b.Secret.GetName()) + len(b.Secret.GetValue())
				vals.secrets[b.Secret.GetName()] = b.Secret.GetValue()
			case *execpb.Frame_Var:
				size += len(b.Var.GetName()) + len(b.Var.GetValue())
				vals.vars[b.Var.GetName()] = b.Var.GetValue()
			default:
				first <- recv{err: errors.New("first frame from host is not run")}
				return
			}
			if size > maxJobValues {
				first <- recv{err: errors.New("the host sent too many secrets and variables before run")}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-first:
		return r.run, r.vals, r.err
	case <-timer.C:
		return nil, jobValues{}, errors.New("timed out waiting for the host's run frame")
	case <-ctx.Done():
		return nil, jobValues{}, ctx.Err()
	}
}

// runStub is the stub executor of sandbox-check: it runs the script of a Run
// without a job with the shell and streams its output.
func runStub(ctx context.Context, conn *execstream.Conn, cfg ExecConfig, run *execpb.Run) (int, error) {
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
	code, err := exitCode(waitErr)
	return sendExit(conn, code, err)
}

// exitCode is the exit code to report for the error of cmd.Wait: 0 for a clean
// exit (also when a background child kept the pipes open past WaitDelay: the
// group was killed by esc.finish), the code of the process (128+signal when it
// was killed, like a shell), or 1 with the error for anything else.
func exitCode(waitErr error) (int, error) {
	var ee *exec.ExitError
	switch {
	case waitErr == nil, errors.Is(waitErr, exec.ErrWaitDelay):
		return 0, nil
	case errors.As(waitErr, &ee):
		code := ee.ExitCode()
		if code < 0 {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), nil
			}
			return 1, nil
		}
		return code, nil
	}
	return 1, waitErr
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
