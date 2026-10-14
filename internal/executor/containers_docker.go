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

// dockerList is the overload of ExecuteDockerCommandAsync that returns the
// output (DockerCommandManager.cs:462-492): non-empty lines of stdout are kept
// and logged, non-empty lines of stderr are logged, and a non-zero exit is an
// error.
func (j *job) dockerList(s *stepRun, c dockerCmd, cancel <-chan struct{}) ([]string, error) {
	j.out(s.num, "##[command]"+c.line)
	var lines []string
	res, err := runProcess(procSpec{
		Argv: c.argv, Dir: dirOrCwd(j.githubValue("workspace")), Env: append(j.processEnv(newOrdered()), c.env...), Credential: j.cfg.Credential,
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
