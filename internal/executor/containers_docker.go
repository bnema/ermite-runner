// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
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

// envKV is DockerUtil.CreateEscapedOption("-e", key, value), or ("-e", key)
// for an empty value (docker then reads it from its own environment).
func envKV(key, value string) dockerPart {
	if value == "" {
		return envOpt(key)
	}
	kv := key + "=" + value
	return dockerPart{line: "-e " + escapeDockerArg(kv), args: []string{"-e", kv}}
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

// dockerList is the overload of ExecuteDockerCommandAsync that returns the
// output (DockerCommandManager.cs:462-492): non-empty lines of stdout are kept
// and logged, non-empty lines of stderr are logged, and a non-zero exit is an
// error.
func (j *job) dockerList(s *stepRun, c dockerCmd, cancel <-chan struct{}) ([]string, error) {
	j.out(s.num, "##[command]"+c.line)
	var lines []string
	res, err := runProcess(procSpec{
		Argv: c.argv, Dir: dirOrCwd(j.githubValue("workspace")), Env: j.processEnv(newOrdered()), Credential: j.cfg.Credential,
		OnLine: func(l string) {
			if l != "" {
				lines = append(lines, l)
				j.write(s.num, l)
			}
		},
		OnErrLine: func(l string) {
			if l != "" {
				j.write(s.num, l)
			}
		},
		Cancel: cancel, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter, WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
	})
	if err != nil {
		return nil, err
	}
	if res.Cancelled {
		return lines, errCancelled
	}
	if res.ExitCode != 0 {
		return lines, ghErrorf("Exit code %d returned from process: file name '%s', arguments '%s'.", res.ExitCode, j.dockerPath(), j.dockerArgString(c))
	}
	return lines, nil
}

// dockerCode is the overload that logs every line and returns the exit code.
func (j *job) dockerCode(s *stepRun, c dockerCmd, cancel <-chan struct{}) (int, error) {
	return j.runDockerCmd(s, c, dirOrCwd(j.githubValue("workspace")), nil, cancel)
}
