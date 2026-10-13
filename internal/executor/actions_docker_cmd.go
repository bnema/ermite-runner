// SPDX-License-Identifier: MIT

package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// dockerPath is the docker CLI (DockerCommandManager.Initialize: which docker).
func (j *job) dockerPath() string {
	if p := j.cfg.Actions.DockerPath; p != "" {
		return p
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	return "/usr/bin/docker"
}

// dockerLabel is DockerInstanceLabel: six hex characters that identify the
// runner (the first characters of the SHA-256 of its .runner file). The
// sandbox has one runner per job; the job's tracking id stands in.
func (j *job) dockerLabel() string {
	h := sha256.Sum256([]byte(j.trackID))
	return hex.EncodeToString(h[:])[:6]
}

// escapeDockerArg is DockerUtil.EscapeString: the string wrapped in quotes with
// the backslashes before a quote doubled (and at the end), as the runner writes
// the arguments of its docker commands (it echoes them, so the log has them).
var (
	quoteEscape       = regexp.MustCompile(`(\\*)"`)
	endOfStringEscape = regexp.MustCompile(`(\\+)$`)
)

func escapeDockerArg(v string) string {
	if v == "" {
		return ""
	}
	v = quoteEscape.ReplaceAllString(v, `$1$1\"`)
	v = endOfStringEscape.ReplaceAllString(v, `$1$1`)
	return `"` + v + `"`
}

// dockerCmd is a docker command: the line the runner echoes and the arguments
// that are run.
type dockerCmd struct {
	line string
	argv []string
}

// newDockerCmd builds "docker <command> <options>" the way the runner does
// (ExecuteDockerCommandAsync: command and options joined by a space and
// trimmed) and keeps the argv apart, so that nothing is re-split.
func (j *job) newDockerCmd(command string, parts ...dockerPart) dockerCmd {
	lines := []string{}
	argv := []string{j.dockerPath(), command}
	for _, p := range parts {
		lines = append(lines, p.line)
		argv = append(argv, p.args...)
	}
	line := strings.TrimSpace(command + " " + strings.Join(lines, " "))
	return dockerCmd{line: j.dockerPath() + " " + line, argv: argv}
}

// dockerPart is one piece of a docker command: its text in the echoed line and
// its arguments.
type dockerPart struct {
	line string
	args []string
}

func raw(args ...string) dockerPart { return dockerPart{line: strings.Join(args, " "), args: args} }

// envOpt is CreateEscapedOption("-e", key): the value is read from the
// environment of the docker process.
func envOpt(key string) dockerPart {
	return dockerPart{line: "-e " + escapeDockerArg(key), args: []string{"-e", key}}
}

func entrypointOpt(ep string) dockerPart {
	return dockerPart{line: `--entrypoint "` + ep + `"`, args: []string{"--entrypoint", ep}}
}

func volumeOpt(src, dst string) dockerPart {
	return dockerPart{
		line: `-v "` + strings.ReplaceAll(src, `"`, `\"`) + `":"` + strings.ReplaceAll(dst, `"`, `\"`) + `"`,
		args: []string{"-v", src + ":" + dst},
	}
}

// sanitizeName is NameValidation.Sanitize: letters, digits (not first) and "_".
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && b.Len() > 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func newImageTag(label string) string {
	return label + ":" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// splitArgs splits a command line like .NET does for the arguments of a
// process: spaces separate, double quotes group, a backslash before a quote
// escapes it (ArgUtil.SplitArguments is not needed: ProcessInvoker passes the
// string to the platform, which splits it with these rules).
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	in, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					in = !in
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
			}
			started = true
		case c == '"':
			in = !in
			started = true
		case (c == ' ' || c == '\t') && !in:
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}

// runDockerCmd echoes the command (##[command], ExecutionContext.Command) and
// runs it, sending every line of its output to the log as is: the output of
// docker is not interpreted as workflow commands (ExecuteDockerCommandAsync
// writes it with context.Output). It returns the exit code.
func (j *job) runDockerCmd(s *stepRun, c dockerCmd, dir string, env []string, cancel <-chan struct{}) (int, error) {
	j.out(s.num, "##[command]"+c.line)
	if env == nil {
		env = j.processEnv(newOrdered())
	}
	res, err := runProcess(procSpec{
		Argv: c.argv, Dir: dir, Env: env, Credential: j.cfg.Credential,
		OnLine: func(line string) { j.write(s.num, line) },
		Cancel: cancel, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter, WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
	})
	if err != nil {
		return 0, err
	}
	if res.Cancelled {
		return res.ExitCode, errCancelled
	}
	return res.ExitCode, nil
}

// errCancelled is a docker command stopped by the cancellation of the step.
var errCancelled = fmt.Errorf("The operation was canceled.")

func dirOrCwd(d string) string {
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}
