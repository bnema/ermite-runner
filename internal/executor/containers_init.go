// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// containerSet is the containers of a job: the job container, if any, then the
// services (JobExtension.cs:322-326: the order of the list the provider gets).
type containerSet struct {
	job      *container
	services []*container
	network  string

	// What the cleanup needs, recorded BEFORE the command that creates it, so
	// that a command whose outcome is not known (a timeout, a cancellation, a
	// lost connection) can still be cleaned: the names are chosen here, not by
	// docker. mu guards them; cleanupMu serialises the cleanups ("Stop
	// containers" and the fallback of the job).
	mu         sync.Mutex
	cleanupMu  sync.Mutex
	networkTry string   // network name, set before docker network create
	netRemoved bool     // the network is gone
	dirs       []string // docker --config directories that could not be removed
}

func (cs *containerSet) tryNetwork(name string) { cs.mu.Lock(); cs.networkTry = name; cs.mu.Unlock() }

func (cs *containerSet) addDir(d string) { cs.mu.Lock(); cs.dirs = append(cs.dirs, d); cs.mu.Unlock() }

func (cs *containerSet) all() []*container {
	var l []*container
	if cs.job != nil {
		l = append(l, cs.job)
	}
	return append(l, cs.services...)
}

// minDockerAPI is requiredDockerEngineAPIVersion (ContainerOperationProvider.cs
// :591): Docker-CE 17.12.
var minDockerAPI = [2]int{1, 35}

var (
	apiVersionRe = regexp.MustCompile(`(?i)\d+\.\d+(\.\d+)?`)
	portLineRe   = regexp.MustCompile(`^(\d+)/(\w+) -> (.+):(\d+)$`)
)

// initContainersStep is the "Initialize containers" step
// (JobExtension.cs:328-331), run if success().
func (j *job) initContainersStep() qstep {
	return qstep{name: "Initialize containers", setup: func(s *stepRun, cancel <-chan struct{}) stepResult {
		return j.initContainers(s, cancel)
	}}
}

// result turns the error of a step body into the result StepsRunner gives it
// (StepsRunner.cs:314-340): a cancellation is logged and cancels the step,
// anything else is logged with its message and fails it.
func (j *job) containerResult(s *stepRun, err error) stepResult {
	if err == nil {
		return resSucceeded
	}
	if err == errCancelled {
		return j.cancelledStep(s)
	}
	j.errorf(s.num, "%s", err.Error())
	return resFailed
}

// initContainers is ContainerOperationProvider.StartContainersAsync
// (:40-103). The step that stops the containers is registered first, so that
// it runs even when this one fails (:56-60).
func (j *job) initContainers(s *stepRun, cancel <-chan struct{}) stepResult {
	j.debug(s.num, "Register post job cleanup for stopping/deleting containers.")
	j.mu.Lock()
	j.postStack = append(j.postStack, qstep{name: "Stop containers", cond: "always()", setup: j.stopContainers})
	j.mu.Unlock()
	return j.containerResult(s, j.startContainers(s, cancel))
}

func (j *job) startContainers(s *stepRun, cancel <-chan struct{}) error {
	num, cs := s.num, j.cs
	// AssertCompatibleOS (:559-571) refuses to run in a container because the
	// paths it mounts would be those of the container's host. The docker
	// daemon of a job runs next to the executor, in the same sandbox, so the
	// paths are right and the check is left out.
	if err := j.checkDockerVersion(s, cancel); err != nil {
		return err
	}
	j.out(num, "##[group]Clean up resources from previous jobs")
	label := j.dockerLabel()
	ps := j.newDockerCmd("ps", raw("--all", "--quiet", "--no-trunc"), quotedOpt("--filter", "label="+label))
	stale, err := j.dockerList(s, ps, cancel)
	if err != nil {
		return err
	}
	for _, id := range stale {
		code, err := j.dockerCode(s, j.newDockerCmd("rm", raw("--force", id)), cancel)
		if err != nil {
			return err
		}
		if code != 0 {
			j.tagged(num, "##[warning]", fmt.Sprintf("Delete stale containers failed, docker rm fail with exit code %d for container %s", code, id))
		}
	}
	prune := j.newDockerCmd("network", raw("prune", "--force"), quotedOpt("--filter", "label="+label))
	code, err := j.dockerCode(s, prune, cancel)
	if err != nil {
		return err
	}
	if code != 0 {
		j.tagged(num, "##[warning]", fmt.Sprintf("Delete stale container networks failed, docker network prune fail with exit code %d", code))
	}
	j.out(num, "##[endgroup]")

	j.out(num, "##[group]Create local container network")
	network := "github_network_" + strings.ReplaceAll(newImageTag(""), ":", "")
	cs.tryNetwork(network)
	code, err = j.dockerCode(s, j.newDockerCmd("network", raw("create", "--label", label, network)), cancel)
	if err != nil {
		return err
	}
	if code != 0 {
		return ghErrorf("Docker network create failed with exit code %d", code)
	}
	j.mu.Lock()
	j.state.SetJobContainer("network", network)
	j.mu.Unlock()
	cs.network = network
	j.out(num, "##[endgroup]")

	for _, c := range cs.all() {
		c.network = network
		if err := j.startContainer(s, c, cancel); err != nil {
			return err
		}
	}
	return j.healthchecks(s, cs.all(), cancel)
}

// checkDockerVersion is AssertCompatibleOS' version check (:573-598) with
// DockerCommandManager.DockerVersion (:47-90), including its use of the
// server's text for the client version (:71, the runner's own slip).
func (j *job) checkDockerVersion(s *stepRun, cancel <-chan struct{}) error {
	num := s.num
	j.out(num, "##[group]Checking docker version")
	ver := func(which string) (string, error) {
		l, err := j.dockerList(s, j.newDockerCmd("version", raw("--format", "'{{."+which+".APIVersion}}'")), cancel)
		if err != nil {
			return "", err
		}
		if len(l) == 0 {
			return "", ghErrorf("Value cannot be null. (Parameter 'Docker.%s.Version')", which)
		}
		return l[0], nil
	}
	server, err := ver("Server")
	if err != nil {
		return err
	}
	j.out(num, "Docker daemon API version: "+server)
	client, err := ver("Client")
	if err != nil {
		return err
	}
	j.out(num, "Docker client API version: "+client)
	j.out(num, "##[endgroup]")
	v, ok := parseAPIVersion(server)
	if !ok {
		return ghErrorf("Value cannot be null. (Parameter 'ServerVersion')")
	}
	if v[0] < minDockerAPI[0] || (v[0] == minDockerAPI[0] && v[1] < minDockerAPI[1]) {
		return ghErrorf("Min required docker engine API server version is '%d.%d', your docker ('%s') server version is '%d.%d'",
			minDockerAPI[0], minDockerAPI[1], j.dockerPath(), v[0], v[1])
	}
	return nil
}

func parseAPIVersion(s string) ([2]int, bool) {
	m := apiVersionRe.FindString(s)
	parts := strings.Split(m, ".")
	if len(parts) < 2 {
		return [2]int{}, false
	}
	a, e1 := strconv.Atoi(parts[0])
	b, e2 := strconv.Atoi(parts[1])
	return [2]int{a, b}, e1 == nil && e2 == nil
}

// containerBackoff is BackoffTimerHelper.GetRandomBackoff(1 s, 10 s).
func (j *job) containerBackoff() time.Duration {
	if f := j.cfg.Actions.Backoff; f != nil {
		return f()
	}
	return time.Duration(1000+rand.IntN(9000)) * time.Millisecond
}

// seconds is the way the runner prints a backoff, TimeSpan.TotalSeconds.
func seconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }

// pullArgs builds `docker [--config dir] pull image`.
func (j *job) pullCmd(image, config string) dockerCmd {
	if config == "" {
		return j.newDockerCmd("pull", raw(image))
	}
	p := j.dockerPath()
	return dockerCmd{line: p + " --config " + config + " pull " + image, argv: []string{p, "--config", config, "pull", image}}
}

// removeConfigDir is ContainerRegistryLogout (:457-470): the credentials
// directory goes, and a failure is the runner's error. A directory that
// cannot be removed is kept for the cleanup of the job, which tries again.
func (j *job) removeConfigDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		j.cs.addDir(dir)
		return ghErrorf("Failed to remove directory containing Docker client credentials: %v", err)
	}
	return nil
}

// registryLogin is ContainerRegistryLogin (:394-455): the credentials go to a
// private docker config directory, the password through stdin. It returns the
// directory ("" without credentials); the caller removes it with
// removeConfigDir, whatever happens.
func (j *job) registryLogin(s *stepRun, c *container, cancel <-chan struct{}) (string, error) {
	if c.credUser == "" || c.credPass == "" {
		return "", nil
	}
	// The password is a secret for the logs of this job from here on.
	j.addMask(c.credPass)
	dir := filepath.Join(j.state.Layout().Temp, ".docker_"+strings.ReplaceAll(newImageTag(""), ":", ""))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", ghErrorf("Failed to create directory to store registry client credentials: %v", err)
	}
	j.own(dir)
	args := []string{j.dockerPath(), "--config", dir, "login"}
	line := "--config " + dir + " login"
	if c.registryServer != "" {
		args, line = append(args, c.registryServer), line+" "+c.registryServer
	}
	args, line = append(args, "-u", c.credUser, "--password-stdin"), line+" -u "+c.credUser+" --password-stdin"
	for try := 1; ; try++ {
		j.out(s.num, "##[command]"+j.dockerPath()+" "+line)
		bc, timedOut, stop := bound(cancel, j.timeouts().Command)
		res, err := runProcess(procSpec{
			Argv: args, Dir: dirOrCwd(j.githubValue("workspace")), Env: j.processEnv(newOrdered()), Credential: j.cfg.Credential,
			OnLine: func(string) {}, Stdin: c.credPass + "\n",
			Cancel: bc, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter, WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
		})
		stop()
		if err != nil {
			return dir, err
		}
		if res.Cancelled {
			if timedOut() {
				return dir, &cmdTimeoutError{cmd: "login", d: j.timeouts().Command}
			}
			return dir, errCancelled
		}
		if res.ExitCode == 0 {
			return dir, nil
		}
		if try == 3 {
			return dir, ghErrorf("Docker login for '%s' failed with exit code %d", c.registryServer, res.ExitCode)
		}
		d := j.containerBackoff()
		j.tagged(s.num, "##[warning]", fmt.Sprintf("Docker login for '%s' failed with exit code %d, back off %s seconds before retry.", c.registryServer, res.ExitCode, seconds(d)))
		if !j.sleepOrCancel(d, cancel) {
			return dir, errCancelled
		}
	}
}
