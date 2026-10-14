// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// alpineCheck is the command of ContainerStepHost.DetermineNodeRuntimeVersion
// (StepHost.cs:176): the distribution is read from the release files.
const alpineCheck = `cat /etc/*release | grep ^ID`

// nodeInContainer is ContainerStepHost.DetermineNodeRuntimeVersion
// (StepHost.cs:154-196, container hooks off): "<version>_alpine" when the
// image is Alpine (musl, where the glibc builds do not run), else the version
// itself. The output of the probe is not logged, only the command is.
func (j *job) nodeInContainer(s *stepRun, c *container, version string, cancel <-chan struct{}) (string, error) {
	cmd := j.newDockerCmd("exec", dockerPart{line: ""}, raw(c.id),
		dockerPart{line: `sh -c "` + alpineCheck + `"`, args: []string{"sh", "-c", alpineCheck}})
	j.out(s.num, "##[command]"+cmd.line)
	var out []string
	res, err := runProcess(procSpec{
		Argv: cmd.argv, Dir: dirOrCwd(j.githubValue("workspace")), Env: j.processEnv(newOrdered()), Credential: j.cfg.Credential,
		OnLine: func(l string) {
			if l != "" {
				out = append(out, l)
			}
		},
		OnErrLine: func(l string) {
			if l != "" {
				out = append(out, l)
			}
		},
		Cancel: cancel, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter, WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
	})
	if err != nil {
		return "", err
	}
	if res.Cancelled {
		return "", errCancelled
	}
	if res.ExitCode == 0 {
		for _, l := range out {
			j.debug(s.num, l)
			if strings.Contains(strings.ToLower(l), "alpine") {
				return j.alpineNode(s, version)
			}
		}
	}
	j.debug(s.num, "Running JavaScript Action with default external tool: "+version)
	return version, nil
}

// alpineNode is CheckPlatformForAlpineContainer (StepHost.cs:248-279): the
// alpine builds exist for x64 only.
func (j *job) alpineNode(s *stepRun, version string) (string, error) {
	if runtime.GOARCH != "amd64" {
		arch := map[string]string{"arm64": "Arm64", "arm": "Arm", "386": "X86"}[runtime.GOARCH]
		if arch == "" {
			arch = runtime.GOARCH
		}
		return "", fmt.Errorf("JavaScript Actions in Alpine containers are only supported on x64 Linux runners. Detected Linux %s", arch)
	}
	v := version + "_alpine"
	j.debug(s.num, "Container distribution is alpine. Running JavaScript Action with external tool: "+v)
	return v, nil
}

// runNodeInContainer is NodeScriptActionHandler.RunAsync (:127-190) with a
// ContainerStepHost: the node of the externals mounted at /__e runs the script
// at its container path, in the container's workspace.
func (j *job) runNodeInContainer(s *stepRun, c *container, script string, env *ordered, cancel <-chan struct{}) stepResult {
	num := s.num
	fail := func(err error) stepResult { j.errorf(num, "%s", err.Error()); return resFailed }
	if err := j.containerReady(); err != nil {
		return fail(err)
	}
	version, err := j.nodeInContainer(s, c, s.nodeVersion, cancel)
	if err != nil {
		return j.containerResult(s, err)
	}
	file := c.resolvePath(filepath.Join(j.externalsDir(), version, "bin", "node"))
	j.mu.Lock()
	wd := j.state.GitHub("workspace")
	j.mu.Unlock()
	henv := j.handlerEnv(env)
	if err := checkEnvNames(henv.Keys()); err != nil {
		return fail(err)
	}
	argv, penv := j.execArgv(c, c.resolvePath(wd), henv, file, []string{c.resolvePath(`"` + script + `"`)})
	s.rawEnvList = penv
	return j.runProc(s, file, j.state.Layout().WorkRoot, argv, "Node Action run completed with exit code %d", cancel)
}
