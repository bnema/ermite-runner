// SPDX-License-Identifier: MIT

package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Cancellation timings of the official runner (Runner.Sdk/ProcessInvoker.cs:32):
// SIGINT at once, SIGTERM after 7.5 s, kill 2.5 s later.
const (
	DefaultTermAfter = 7500 * time.Millisecond
	DefaultKillAfter = 2500 * time.Millisecond
	// DefaultWaitDelay is how long the executor waits for the output pipes to
	// close after the shell exited, before it gives up on background children
	// that still hold them: ProcessInvoker.ProcessExitedHandler waits 5 s
	// (ProcessInvoker.cs:491).
	DefaultWaitDelay = 5 * time.Second
)

// lineWriter turns a byte stream into lines like StreamReader.ReadLine: "\n",
// "\r" and "\r\n" end a line, and invalid UTF-8 becomes U+FFFD. It calls fn
// for every complete line, in order, from the goroutine that writes.
type lineWriter struct {
	fn      func(line string)
	buf     []byte
	pending bool // the last byte seen was "\r": a following "\n" is part of it
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if w.pending {
			w.pending = false
			if p[0] == '\n' {
				p = p[1:]
				continue
			}
		}
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			w.buf = append(w.buf, p...)
			break
		}
		w.buf = append(w.buf, p[:i]...)
		w.emit()
		w.pending = p[i] == '\r'
		p = p[i+1:]
	}
	return n, nil
}

func (w *lineWriter) emit() {
	b := w.buf
	if !utf8.Valid(b) {
		b = bytes.ToValidUTF8(b, []byte("\ufffd"))
	}
	line := string(b)
	w.buf = w.buf[:0]
	w.fn(line)
}

// Flush emits the last line when the stream ended without a terminator.
func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit()
	}
}

// dispatchBatchDelay is how long the dispatcher lets lines pile up after the
// first one before it hands a batch over. The official runner hands over
// whatever its reader threads queued when its loop wakes up (a thread-pool
// wake-up, about a millisecond); Go wakes in microseconds, so without this
// pause the interleaving of a fast script's stdout and stderr would differ
// from the official runner's more often.
const dispatchBatchDelay = 2 * time.Millisecond

// dispatcher reproduces how ProcessInvoker delivers output: stdout and stderr
// are read by two readers into two queues, and a loop that wakes up on a
// signal hands over all queued stderr lines first and then all queued stdout
// lines (ProcessInvoker.ProcessOutput, ProcessInvoker.cs:392-436). Within a
// stream the order is kept; between the streams a batch puts stderr first,
// which is visible with `bash -x` (the corpus recording of shell-custom).
type dispatcher struct {
	fn       func(string)
	mu       sync.Mutex
	outQ     []string
	errQ     []string
	signal   chan struct{}
	closed   chan struct{}
	finished chan struct{}
	out, err *lineWriter
}

func newDispatcher(fn func(string)) *dispatcher {
	d := &dispatcher{fn: fn, signal: make(chan struct{}, 1), closed: make(chan struct{}), finished: make(chan struct{})}
	push := func(q *[]string) func(string) {
		return func(line string) {
			d.mu.Lock()
			*q = append(*q, line)
			d.mu.Unlock()
			select {
			case d.signal <- struct{}{}:
			default:
			}
		}
	}
	d.out = &lineWriter{fn: push(&d.outQ)}
	d.err = &lineWriter{fn: push(&d.errQ)}
	go d.loop()
	return d
}

func (d *dispatcher) drain() {
	d.mu.Lock()
	errs, outs := d.errQ, d.outQ
	d.errQ, d.outQ = nil, nil
	d.mu.Unlock()
	for _, l := range errs {
		d.fn(l)
	}
	for _, l := range outs {
		d.fn(l)
	}
}

func (d *dispatcher) loop() {
	defer close(d.finished)
	for {
		select {
		case <-d.signal:
			time.Sleep(dispatchBatchDelay)
			d.drain()
		case <-d.closed:
			d.drain()
			return
		}
	}
}

// Close flushes the readers and delivers what is left. Call it once the
// process and its pipes are done.
func (d *dispatcher) Close() {
	d.err.Flush()
	d.out.Flush()
	close(d.closed)
	<-d.finished
}

// procSpec describes one process to run.
type procSpec struct {
	Argv []string
	Dir  string
	Env  []string
	// Credential, when set, is the user the process runs as.
	Credential *syscall.Credential
	// OnLine receives the lines of stdout and stderr, one call at a time, in
	// the order of dispatcher.
	OnLine func(line string)
	// Cancel starts the SIGINT, SIGTERM, SIGKILL escalation when closed.
	Cancel <-chan struct{}

	TermAfter, KillAfter, WaitDelay time.Duration
	// Signal delivers a signal to a process group; tests replace it.
	Signal func(pgid int, sig syscall.Signal)
}

// procResult is how a process ended.
type procResult struct {
	ExitCode  int
	Cancelled bool // Cancel was closed while the process ran
}

// runProcess starts the process in its own process group, streams its output
// and waits. It is ProcessInvoker.ExecuteAsync for one process: stdin is
// closed, and a closed Cancel starts the escalation. After the shell exits the
// group is not touched (background children survive, like on a hosted runner
// until the orphan cleanup of "Complete job").
func runProcess(spec procSpec) (procResult, error) {
	if spec.TermAfter == 0 {
		spec.TermAfter = DefaultTermAfter
	}
	if spec.KillAfter == 0 {
		spec.KillAfter = DefaultKillAfter
	}
	if spec.WaitDelay == 0 {
		spec.WaitDelay = DefaultWaitDelay
	}
	if spec.Signal == nil {
		spec.Signal = func(pgid int, sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: spec.Credential}
	disp := newDispatcher(spec.OnLine)
	cmd.Stdout, cmd.Stderr = disp.out, disp.err
	cmd.WaitDelay = spec.WaitDelay
	if err := cmd.Start(); err != nil {
		return procResult{}, err
	}
	pid := cmd.Process.Pid
	setOOMScoreAdj(pid)

	var mu sync.Mutex
	done := false
	cancelled := false
	var timers []*time.Timer
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-spec.Cancel:
		case <-stopWatch:
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		cancelled = true
		spec.Signal(pid, syscall.SIGINT)
		send := func(sig syscall.Signal) func() {
			return func() {
				mu.Lock()
				defer mu.Unlock()
				if !done { // the group id could be reused once the shell is reaped
					spec.Signal(pid, sig)
				}
			}
		}
		timers = append(timers,
			time.AfterFunc(spec.TermAfter, send(syscall.SIGTERM)),
			time.AfterFunc(spec.TermAfter+spec.KillAfter, send(syscall.SIGKILL)))
	}()

	err := cmd.Wait()
	mu.Lock()
	done = true
	for _, t := range timers {
		t.Stop()
	}
	was := cancelled
	mu.Unlock()
	close(stopWatch)
	<-watchDone
	disp.Close()

	res := procResult{Cancelled: was}
	var ee *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode < 0 { // killed by a signal: 128+signal, as .NET reports it
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.ExitCode = 128 + int(ws.Signal())
			} else {
				res.ExitCode = 1
			}
		}
	default:
		return res, err
	}
	return res, nil
}

// setOOMScoreAdj makes the step process a likelier OOM victim than the
// executor (ProcessInvoker.DecreaseProcessPriority, ProcessInvoker.cs:575).
func setOOMScoreAdj(pid int) {
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("500"), 0o644)
}

// startError turns the error of a failed start into the text of .NET's
// Win32Exception, which the official runner logs (the corpus recording
// "working directory that does not exist" has it).
func startError(shellPath, dir string, err error) string {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return fmt.Sprintf("An error occurred trying to start process '%s' with working directory '%s'. No such file or directory", shellPath, dir)
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return fmt.Sprintf("An error occurred trying to start process '%s' with working directory '%s'. Not a directory", shellPath, dir)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Sprintf("An error occurred trying to start process '%s' with working directory '%s'. Permission denied", shellPath, dir)
	}
	return fmt.Sprintf("An error occurred trying to start process '%s' with working directory '%s'. %v", shellPath, dir, err)
}

// ProcessTrackingID is the variable that marks every process of a job so that
// "Complete job" can find the orphans (Constants.ProcessTrackingId).
const ProcessTrackingID = "RUNNER_TRACKING_ID"

// procEntry is a process seen in /proc.
type procEntry struct {
	PID  int
	Name string // comm, as .NET's ProcessName
}

func (p procEntry) key() string { return strconv.Itoa(p.PID) + "_" + p.Name }

// snapshotProcesses lists the processes (JobExtension.SnapshotProcesses).
func snapshotProcesses() []procEntry {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []procEntry
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		out = append(out, procEntry{pid, strings.TrimSuffix(string(comm), "\n")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// processTrackingValue reads RUNNER_TRACKING_ID from the environment of a
// process; "" when it cannot be read.
func processTrackingValue(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return ""
	}
	prefix := []byte(ProcessTrackingID + "=")
	for _, kv := range bytes.Split(b, []byte{0}) {
		if bytes.HasPrefix(kv, prefix) {
			return string(kv[len(prefix):])
		}
	}
	return ""
}

// killOrphans kills the processes that carry the tracking id and were not
// there before the job (JobExtension.FinalizeJob, JobExtension.cs:132-180).
// logf receives the "Terminate orphan process" lines.
func killOrphans(id string, before map[string]bool, logf func(string)) {
	self := os.Getpid()
	for _, p := range snapshotProcesses() {
		if p.PID == self || before[p.key()] {
			continue
		}
		if !strings.EqualFold(processTrackingValue(p.PID), id) {
			continue
		}
		logf(fmt.Sprintf("Terminate orphan process: pid (%d) (%s)", p.PID, p.Name))
		_ = syscall.Kill(p.PID, syscall.SIGKILL)
	}
}

var _ io.Writer = (*lineWriter)(nil)
