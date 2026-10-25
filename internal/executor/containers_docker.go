// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Helpers for the docker commands of the container steps. The runner builds a
// command line as a string, echoes it (ExecutionContext.Command, "##[command]")
// and lets .NET split it into arguments. Here the echoed line and the
// arguments are built together (dockerPart), so that nothing a workflow wrote
// can end up as an argument of its own, except through `options:` which the
// runner splits too (ParseArguments).

// quotedOpt is `flag "value"` in the echoed line and two arguments.
func quotedOpt(flag, value string) dockerPart {
	return dockerPart{line: flag + ` "` + value + `"`, args: []string{flag, value}}
}

// eqOpt is `flag="value"` (docker inspect --format="...").
func eqOpt(flag, value string) dockerPart {
	return dockerPart{line: flag + `="` + value + `"`, args: []string{flag + "=" + value}}
}

// Accepted risk (review of the pull request, GO-004-R1/R2): a workflow can set
// DOCKER_*, proxy, PATH or SSH_AUTH_SOCK variables for its container, and the
// ones listed here travel as NAME=value on the arguments of the docker command,
// which the docker CLI reads. The CLI runs in the job's own sandbox, which is
// the containment boundary: the job can already run docker with any
// environment and read its own process arguments, and the C# runner has the
// same exposure. Values still go through the masker in the logs.
//
// cliSensitive are the variables the docker CLI itself reads (its daemon, its
// configuration, its proxy and TLS settings, the loader): handing one of them
// to the process of docker as the value of a container variable would change
// what the CLI does, so these are the only values that stay in the arguments.
func cliSensitive(name string) bool {
	u := strings.ToUpper(name)
	switch u {
	case "HOME", "TMPDIR", "USER", "LOGNAME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"GODEBUG", "GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GOTRACEBACK", "KUBECONFIG", ProcessTrackingID:
		return true
	}
	for _, pre := range []string{"DOCKER_", "XDG_", "LD_", "SSL_CERT", "BUILDKIT_", "COMPOSE_", "CONTAINERD_", "REGISTRY_"} {
		if strings.HasPrefix(u, pre) {
			return true
		}
	}
	return false
}

// envForCLI is how one container variable reaches `docker create`/`exec`:
// "-e NAME" with the value in the environment of the docker process, so that
// it is on no command line (visible to every user of the sandbox in /proc) —
// except for the variables the CLI reads itself, unless their value is the one
// the process has anyway. An empty value is set explicitly, so that the CLI
// cannot inherit one from the executor. echo is the text the runner logs.
func (j *job) envForCLI(key, value, echo string) dockerPart {
	if cliSensitive(key) {
		if v, ok := envLookup(j.baseEnv, key); ok && v == value {
			return dockerPart{line: echo, args: []string{"-e", key}}
		}
		return dockerPart{line: echo, args: []string{"-e", key + "=" + value}}
	}
	return dockerPart{line: echo, args: []string{"-e", key}, env: []string{key + "=" + value}}
}

func envLookup(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// checkEnvNames refuses the names `docker -e NAME` would take for NAME=value:
// an empty one, or one holding "=" or NUL. Such a key would let a workflow set
// a variable of the docker CLI itself ("HTTP_PROXY=http://x/?y") past the exact
// name check of cliSensitive. The runner passes them on and docker decides; a
// name like that is never valid in the environment of a process.
func checkEnvNames(keys []string) error {
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return ghErrorf("Invalid environment variable name '%s': a name cannot be empty or contain '=' or NUL", strings.ReplaceAll(k, "\x00", `\0`))
		}
	}
	return nil
}

// envKV is DockerUtil.CreateEscapedOption("-e", key, value), or ("-e", key)
// for an empty value, as the runner echoes it.
func (j *job) envKV(key, value string) dockerPart {
	if value == "" {
		return j.envForCLI(key, "", "-e "+escapeDockerArg(key))
	}
	return j.envForCLI(key, value, "-e "+escapeDockerArg(key+"="+value))
}

// mountOpt is the -v option of DockerCommandManager.DockerCreate (:164-185).
func mountOpt(m mount) dockerPart {
	esc := func(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }
	var p dockerPart
	if m.source == "" {
		p = dockerPart{line: `-v "` + esc(m.target) + `"`, args: []string{"-v", m.target}}
	} else {
		p = dockerPart{line: `-v "` + esc(m.source) + `":"` + esc(m.target) + `"`, args: []string{"-v", m.source + ":" + m.target}}
	}
	if m.readOnly {
		p.line += ":ro"
		p.args[1] += ":ro"
	}
	return p
}

// optionsParts is the user's `options:` text: echoed as written, split into
// arguments like .NET splits a command line. An empty text still leaves its
// place in the line (the runner joins an empty string: two spaces).
func optionsParts(options string) dockerPart {
	return dockerPart{line: options, args: ParseArguments(options)}
}

// dockerArgString is the arguments of the command as ProcessExitCodeException
// words them: the echoed line without the path of docker.
func (j *job) dockerArgString(c dockerCmd) string {
	return strings.TrimPrefix(c.line, j.dockerPath()+" ")
}

// exitError is ProcessExitCodeException: docker ran and said no.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Time limits of the docker commands of the container steps. The runner has
// none (its commands end with the job); a sandbox whose daemon stalls must not
// hold the job until its timeout, nor the cleanup forever.
type ContainerTimeouts struct {
	// Command bounds one docker command (default 2 minutes); Pull bounds a
	// docker pull (30 minutes).
	Command, Pull time.Duration
	// CleanupCommand bounds one command of "Stop containers" (30 seconds) and
	// CleanupBudget the whole cleanup (3 minutes), whatever happens to the job.
	CleanupCommand, CleanupBudget time.Duration
}

func (j *job) timeouts() ContainerTimeouts {
	t := j.cfg.Actions.Containers
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&t.Command, 2*time.Minute)
	def(&t.Pull, 30*time.Minute)
	def(&t.CleanupCommand, 30*time.Second)
	def(&t.CleanupBudget, 3*time.Minute)
	return t
}

// cmdTimeoutError is a docker command stopped by its time limit.
type cmdTimeoutError struct {
	cmd string
	d   time.Duration
}

func (e *cmdTimeoutError) Error() string {
	return fmt.Sprintf("The docker command timed out after %s: %s", e.d, e.cmd)
}

// bound returns a channel that closes when cancel does (nil never) or after d,
// and a function that tells whether it was the time, with the one that
// releases the timer. With d <= 0 the channel is closed on return: a command
// that checks it right away must not race the timer.
func bound(cancel <-chan struct{}, d time.Duration) (ch <-chan struct{}, timedOut func() bool, stop func()) {
	out := make(chan struct{})
	var once sync.Once
	var fired atomic.Bool
	closeOut := func() { once.Do(func() { close(out) }) }
	if d <= 0 {
		fired.Store(true)
		closeOut()
	}
	t := time.AfterFunc(d, func() { fired.Store(true); closeOut() })
	done := make(chan struct{})
	if cancel != nil {
		go func() {
			select {
			case <-cancel:
				closeOut()
			case <-done:
			}
		}()
	}
	return out, fired.Load, func() { t.Stop(); close(done) }
}

// dockerOpts are the options of dockerDo.
type dockerOpts struct {
	// s is the step whose log gets the command and its output; nil runs the
	// command silently (the fallback cleanup).
	s *stepRun
	// cancel stops the command (the escalation of runProcess); always set.
	cancel <-chan struct{}
	// timeout bounds the command; 0 means the command limit.
	timeout time.Duration
	// quiet keeps the echoed command and the output out of the log.
	quiet bool
	// all logs the empty lines too (the overloads that return a code).
	all bool
	// errToOut also collects the lines of stderr.
	errToOut bool
}

// do runs the command and returns the non-empty lines of its stdout (and
// stderr with errToOut), its exit code and an error for what is not an exit
// code: a start failure, a cancellation, a time limit.
func (j *job) dockerDo(c dockerCmd, o dockerOpts) (lines []string, code int, err error) {
	log := o.s != nil && !o.quiet
	if log {
		j.out(o.s.num, "##[command]"+c.line)
	}
	d := o.timeout
	if d <= 0 {
		d = j.timeouts().Command
	}
	cancel, timedOut, stop := bound(o.cancel, d)
	defer stop()
	var mu sync.Mutex
	run := runProcess
	if j.cfg.runProcess != nil {
		run = j.cfg.runProcess
	}
	res, perr := run(procSpec{
		Argv: c.argv, Dir: dirOrCwd(j.githubValue("workspace")), Env: append(j.processEnv(newOrdered()), c.env...), Credential: j.cfg.Credential,
		OnLine: func(l string) {
			mu.Lock()
			if l != "" {
				lines = append(lines, l)
			}
			mu.Unlock()
			if log && (o.all || l != "") {
				j.write(o.s.num, l)
			}
		},
		OnErrLine: func(l string) {
			if o.errToOut && l != "" {
				mu.Lock()
				lines = append(lines, l)
				mu.Unlock()
			}
			if log && (o.all || l != "") {
				j.write(o.s.num, l)
			}
		},
		Cancel: cancel, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter, WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
	})
	mu.Lock()
	defer mu.Unlock()
	switch {
	case perr != nil:
		return lines, 0, perr
	case res.Cancelled && timedOut():
		return lines, res.ExitCode, &cmdTimeoutError{cmd: j.dockerArgString(c), d: d}
	case res.Cancelled:
		return lines, res.ExitCode, errCancelled
	}
	return lines, res.ExitCode, nil
}

// dockerList is the overload of ExecuteDockerCommandAsync that returns the
// output (DockerCommandManager.cs:462-492): non-empty lines of stdout are kept
// and logged, non-empty lines of stderr are logged, and a non-zero exit is an
// error.
func (j *job) dockerList(s *stepRun, c dockerCmd, cancel <-chan struct{}) ([]string, error) {
	lines, code, err := j.dockerDo(c, dockerOpts{s: s, cancel: cancel})
	if err != nil {
		return lines, err
	}
	if code != 0 {
		return lines, &exitError{code: code, msg: fmt.Sprintf("Exit code %d returned from process: file name '%s', arguments '%s'.", code, j.dockerPath(), j.dockerArgString(c))}
	}
	return lines, nil
}

// dockerCode is the overload that logs every line and returns the exit code.
func (j *job) dockerCode(s *stepRun, c dockerCmd, cancel <-chan struct{}) (int, error) {
	_, code, err := j.dockerDo(c, dockerOpts{s: s, cancel: cancel, all: true})
	return code, err
}
