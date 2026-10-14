// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// createCmd is DockerCommandManager.DockerCreate (DockerCommandManager.cs:
// 119-215), in the order of the runner's options.
func (j *job) createCmd(c *container) dockerCmd {
	parts := []dockerPart{raw("--name", c.name), raw("--label", j.dockerLabel())}
	if c.workDir != "" {
		parts = append(parts, raw("--workdir", c.workDir))
	}
	if c.network != "" {
		parts = append(parts, raw("--network", c.network))
	}
	if c.alias != "" {
		parts = append(parts, raw("--network-alias", c.alias))
	}
	for _, p := range c.ports {
		parts = append(parts, raw("-p", p))
	}
	parts = append(parts, optionsParts(c.options))
	for _, k := range c.env.Keys() {
		v, _ := c.env.Get(k)
		parts = append(parts, envKV(k, v))
	}
	parts = append(parts, raw("-e", "GITHUB_ACTIONS=true"))
	if _, ok := c.env.Get("CI"); !ok {
		parts = append(parts, raw("-e", "CI=true"))
	}
	for _, m := range c.mounts {
		parts = append(parts, mountOpt(m))
	}
	if c.entrypoint != "" {
		parts = append(parts, entrypointOpt(c.entrypoint))
	}
	parts = append(parts, raw(c.image), dockerPart{line: c.entryArgs, args: ParseArguments(c.entryArgs)})
	return j.newDockerCmd("create", parts...)
}

// mountWellKnownDirectories is ContainerOperationProvider.MountWellKnownDirectories
// (:242-283): the job container sees the work directory, the externals, the
// temp, actions and tool cache directories, and a home and a workflow
// directory of its own.
func (j *job) mountWellKnownDirectories(c *container) error {
	lay := j.state.Layout()
	workspace := j.githubValue("workspace")
	tr := c.translateToContainer
	c.mounts = append(c.mounts,
		mount{source: lay.WorkRoot, target: tr(lay.WorkRoot)},
		mount{source: j.externalsDir(), target: tr(j.externalsDir()), readOnly: true},
		mount{source: lay.Temp, target: tr(lay.Temp)},
		mount{source: lay.Actions, target: tr(lay.Actions)},
		mount{source: lay.ToolCache, target: tr(lay.ToolCache)})
	for _, d := range []struct{ dir, target string }{
		{"_github_home", "/github/home"}, {"_github_workflow", "/github/workflow"},
	} {
		host := filepath.Join(lay.Temp, d.dir)
		if err := os.MkdirAll(host, 0o755); err != nil {
			return err
		}
		j.own(host)
		c.mounts = append(c.mounts, mount{source: host, target: d.target})
		c.maps = append([]pathMap{{host, d.target}}, c.maps...) // AddPathTranslateMapping inserts first
		if d.dir == "_github_home" {
			c.env.Set("HOME", tr(host))
		}
	}
	c.workDir = tr(workspace)
	c.entrypoint = "tail"
	c.entryArgs = `"-f" "/dev/null"`
	return nil
}

// startContainer is StartContainerAsync (ContainerOperationProvider.cs:
// 123-240).
func (j *job) startContainer(s *stepRun, c *container, cancel <-chan struct{}) error {
	num := s.num
	group := "Starting job container"
	if !c.isJob {
		group = "Starting " + c.alias + " service container"
	}
	j.out(num, "##[group]"+group)
	for _, m := range c.userMounts() {
		if m.source == "/" {
			j.tagged(num, "##[warning]", fmt.Sprintf("Volume mount %s is going to mount '/' into the container which may cause file ownership change in the entire file system and cause Actions Runner to lose permission to access the disk.", m.user))
		}
	}
	cfg, err := j.registryLogin(s, c, cancel)
	if err != nil {
		if cfg != "" {
			_ = os.RemoveAll(cfg)
		}
		return err
	}
	code, err := j.pullWithRetry(s, c.image, cfg, cancel)
	if cfg != "" {
		_ = os.RemoveAll(cfg)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return ghErrorf("Docker pull failed with exit code %d", code)
	}
	if c.isJob {
		if err := j.mountWellKnownDirectories(c); err != nil {
			return err
		}
	}
	out, err := j.dockerList(s, j.createCmd(c), nil)
	if err != nil {
		return err
	}
	if len(out) == 0 || out[0] == "" {
		return ghErrorf("Value cannot be null. (Parameter 'container.ContainerId')")
	}
	c.id = out[0]
	code, err = j.dockerCode(s, j.newDockerCmd("start", raw(c.id)), cancel)
	if err != nil {
		return err
	}
	if code != 0 {
		return ghErrorf("Docker start fail with exit code %d", code)
	}
	j.checkRunning(s, c)
	if !c.isJob {
		ports, err := j.dockerList(s, j.newDockerCmd("port", raw(c.id)), nil)
		if err != nil {
			return err
		}
		c.portMappings = parseDockerPort(ports)
		j.mu.Lock()
		j.state.SetJobService(c.alias, c.id, c.network, c.portMappings)
		j.mu.Unlock()
	} else {
		env, err := j.dockerList(s, j.newDockerCmd("inspect", quotedOpt("--format", "{{range .Config.Env}}{{println .}}{{end}}"), raw(c.id)), nil)
		if err != nil {
			return err
		}
		c.runtimePath = parsePathFromConfigEnv(env)
		j.mu.Lock()
		j.state.SetJobContainer("id", c.id)
		j.mu.Unlock()
	}
	j.out(num, "##[endgroup]")
	return nil
}

// pullWithRetry is the pull loop of StartContainerAsync (:158-178): three
// tries, a random back off of 1 to 10 s between them.
func (j *job) pullWithRetry(s *stepRun, image, cfg string, cancel <-chan struct{}) (int, error) {
	code := 0
	for try := 1; try <= 3; try++ {
		var err error
		code, err = j.dockerCode(s, j.pullCmd(image, cfg), cancel)
		if err != nil {
			return code, err
		}
		if code == 0 {
			return 0, nil
		}
		if try < 3 {
			d := j.containerBackoff()
			j.tagged(s.num, "##[warning]", fmt.Sprintf("Docker pull failed with exit code %d, back off %s seconds before retry.", code, seconds(d)))
			if !j.sleepOrCancel(d, cancel) {
				return code, errCancelled
			}
		}
	}
	return code, nil
}

// checkRunning is the best-effort check of :193-213.
func (j *job) checkRunning(s *stepRun, c *container) {
	fmtOpt := quotedOpt("--format", "{{.ID}} {{.Status}}")
	out, err := j.dockerList(s, j.newDockerCmd("ps", raw("--all"), raw("--filter", "id="+c.id), raw("--filter", "status=running"), raw("--no-trunc"), fmtOpt), nil)
	if err != nil {
		return
	}
	for _, l := range out {
		if l != "" {
			if strings.HasPrefix(l, c.id) {
				return
			}
			break
		}
	}
	_, _ = j.dockerList(s, j.newDockerCmd("ps", raw("--all"), raw("--filter", "id="+c.id), raw("--no-trunc"), fmtOpt), nil)
	if code, err := j.dockerCode(s, j.newDockerCmd("logs", raw("--details", c.id)), nil); err == nil && code != 0 {
		j.tagged(s.num, "##[warning]", fmt.Sprintf("Docker logs fail with exit code %d", code))
	}
	j.tagged(s.num, "##[warning]", fmt.Sprintf("Docker container %s is not in running state.", c.id))
}

// parseDockerPort is DockerUtil.ParseDockerPort (DockerUtil.cs:14-37).
func parseDockerPort(lines []string) []portMapping {
	var out []portMapping
	for _, l := range lines {
		if m := portLineRe.FindStringSubmatch(l); m != nil {
			out = append(out, portMapping{host: m[4], container: m[1], proto: m[2]})
		}
	}
	return out
}

// parsePathFromConfigEnv is DockerUtil.ParsePathFromConfigEnv (:39-52).
func parsePathFromConfigEnv(lines []string) string {
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok && k == "PATH" {
			return v
		}
	}
	return ""
}
