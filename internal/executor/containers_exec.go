// SPDX-License-Identifier: MIT

package executor

import (
	"strings"
)

// ContainerStepHost (Runner.Worker/Handlers/StepHost.cs:102-296): the steps of
// a job that has a container run through `docker exec` in it.

// stepContainer is the job container when steps run in it, else nil.
func (j *job) stepContainer() *container {
	if j.cs == nil {
		return nil
	}
	return j.cs.job
}

// containerReady is the check of ActionRunner.cs:157-164: the container must
// exist. After a failed "Initialize containers" an always() step gets the
// runner's error.
func (j *job) containerReady() error {
	if c := j.stepContainer(); c != nil && c.id == "" {
		return ghErrorf("Value cannot be null. (Parameter 'ContainerId')")
	}
	return nil
}

// hostEnv is the environment a handler gives its process: the step's
// variables then the runtime variables of the contexts (GITHUB_*, RUNNER_*)
// (ScriptHandler.cs:289-299). It is what `docker exec -e` passes on.
func (j *job) handlerEnv(stepEnv *ordered) *ordered {
	env := stepEnv.Clone()
	j.mu.Lock()
	rt := j.state.RuntimeEnv()
	j.mu.Unlock()
	for _, k := range rt.Keys() {
		v, _ := rt.Get(k)
		env.Set(k, v)
	}
	return env
}

// execArgv is ContainerStepHost.ExecuteAsync (StepHost.cs:196-246):
// `docker exec -i --workdir <dir> -e NAME... [-e PATH="<prepend>:<PATH>"] <id>
// <command> <arguments>`. The values of the variables are not on the command
// line: -e NAME takes them from the environment of docker, which gets them
// with the host paths translated to the container's (:248-254). args are the
// arguments of the command, already split.
func (j *job) execArgv(c *container, workDir string, env *ordered, command string, args []string) (argv []string, procEnv []string) {
	argv = []string{j.dockerPath(), "exec", "-i", "--workdir", workDir}
	cenv := newOrdered()
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		v = c.translateToContainer(v)
		// PATH is set below. The docker process is started by its absolute
		// path, so the variables of the container are not read by it, except
		// those the CLI itself reads.
		p := j.envForCLI(k, v, "")
		argv = append(argv, p.args...)
		for _, kv := range p.env {
			cenv.Set(k, strings.TrimPrefix(kv, k+"="))
		}
	}
	j.mu.Lock()
	prepend := j.state.Prepend()
	j.mu.Unlock()
	if len(prepend) > 0 {
		rev := make([]string, len(prepend))
		for i, p := range prepend {
			rev[len(prepend)-1-i] = p
		}
		full := strings.Join(rev, ":")
		if c.runtimePath != "" {
			full += ":" + c.runtimePath
		}
		// The value is in the environment of docker.exec only: the process is
		// started by absolute path and `docker exec` runs nothing of its own.
		argv = append(argv, "-e", "PATH")
		cenv.Set("PATH", full)
	}
	argv = append(argv, c.id, command)
	argv = append(argv, args...)
	return argv, j.dockerProcessEnv(cenv)
}

// containerScript prepares a run step for the container: the script is
// written on the host in RUNNER_TEMP, which the container has at /__w/_temp,
// and the shell gets that path.
func (j *job) containerScript(c *container, plan ShellPlan, in scriptIn, s *stepRun) (argv []string, err error) {
	sc, err := WriteScriptFor(j.tempRoot, plan, in.script, c.translateToContainer)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	workspace := j.state.GitHub("workspace")
	j.mu.Unlock()
	wd := c.resolvePath(ResolveWorkingDirectory(workspace, in.workingDirectory))
	var rest []string
	if len(sc.Argv) > 1 {
		rest = sc.Argv[1:]
	}
	a, penv := j.execArgv(c, wd, j.handlerEnv(s.env), plan.CommandPath, rest)
	s.rawEnvList = penv
	return a, nil
}
