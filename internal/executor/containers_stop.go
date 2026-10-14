// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

// healthBackoff is BackoffTimerHelper.GetExponentialBackoff(retry, 2 s, 32 s,
// 2 s) (BackoffTimerHelper.cs:27-36).
func healthBackoff(attempt int) time.Duration {
	random := 1600 + rand.Float64()*800 // Random.Next(1600, 2400) ms
	add := (math.Pow(2, float64(attempt)) - 1) * random
	ms := math.Min(2000+add, 32000)
	return time.Duration(ms * float64(time.Millisecond))
}

// containerHealth is ContainerHealthcheck (ContainerOperationProvider.cs:
// 372-392): a container without a HEALTHCHECK is healthy; while docker says
// "starting" the check waits with an exponential back off.
func (j *job) containerHealth(s *stepRun, c *container, cancel <-chan struct{}) (bool, error) {
	inspect := func() (string, error) {
		cmd := j.newDockerCmd("inspect",
			eqOpt("--format", "{{if .Config.Healthcheck}}{{print .State.Health.Status}}{{end}}"), raw(c.id))
		l, err := j.dockerList(s, cmd, nil)
		if err != nil || len(l) == 0 {
			return "", err
		}
		return l[0], nil
	}
	h, err := inspect()
	if err != nil {
		return false, err
	}
	if h == "" {
		return true, nil
	}
	for retry := 0; strings.EqualFold(h, "starting"); retry++ {
		d := healthBackoff(retry)
		if f := j.cfg.Actions.Backoff; f != nil {
			d = f()
		}
		// TimeSpan.Seconds is the seconds component, not the total.
		j.out(s.num, fmt.Sprintf("%s service is starting, waiting %d seconds before checking again.", c.alias, int(d.Seconds())%60))
		if !j.sleepOrCancel(d, cancel) {
			return false, errCancelled
		}
		if h, err = inspect(); err != nil {
			return false, err
		}
	}
	return strings.EqualFold(h, "healthy"), nil
}

// healthchecks is RunContainersHealthcheck (:105-139).
func (j *job) healthchecks(s *stepRun, all []*container, cancel <-chan struct{}) error {
	num := s.num
	j.out(num, "##[group]Waiting for all services to be ready")
	var bad []*container
	for _, c := range all {
		if c.isJob {
			continue
		}
		ok, err := j.containerHealth(s, c, cancel)
		if err != nil {
			return err
		}
		if !ok {
			bad = append(bad, c)
			continue
		}
		j.out(num, c.alias+" service is healthy.")
	}
	j.out(num, "##[endgroup]")
	if len(bad) == 0 {
		return nil
	}
	for _, c := range bad {
		j.out(num, "##[group]Service container "+c.alias+" failed.")
		if _, err := j.dockerCode(s, j.newDockerCmd("logs", raw("--details", c.id)), cancel); err != nil {
			return err
		}
		j.errorf(num, "Failed to initialize container %s", c.image)
		c.failedInit = true
		j.out(num, "##[endgroup]")
	}
	return ghErrorf("One or more containers failed to start.")
}

// stopContainers is "Stop containers" (StopContainersAsync, :105-124 and
// StopContainerAsync, :328-351). It is registered with the condition always(),
// so it runs after a failure, a cancellation and a timeout alike.
func (j *job) stopContainers(s *stepRun, cancel <-chan struct{}) stepResult {
	num := s.num
	cs := j.cs
	for _, c := range cs.all() {
		if c.id == "" {
			continue
		}
		if !c.isJob && !c.failedInit {
			j.out(num, "Print service container logs: "+c.name)
			code, err := j.dockerCode(s, j.newDockerCmd("logs", raw("--details", c.id)), cancel)
			if err != nil {
				return j.containerResult(s, err)
			}
			if code != 0 {
				j.tagged(num, "##[warning]", fmt.Sprintf("Docker logs fail with exit code %d", code))
			}
		}
		j.out(num, "Stop and remove container: "+c.name)
		code, err := j.dockerCode(s, j.newDockerCmd("rm", raw("--force", c.id)), cancel)
		if err != nil {
			return j.containerResult(s, err)
		}
		if code != 0 {
			j.tagged(num, "##[warning]", fmt.Sprintf("Docker rm fail with exit code %d", code))
		}
	}
	// The network of the first container; without one the runner throws
	// (ArgUtil.NotNull, ContainerOperationProvider.cs:272).
	if len(cs.all()) > 0 && cs.all()[0].network == "" {
		return j.containerResult(s, ghErrorf("Value cannot be null. (Parameter 'network')"))
	}
	network := cs.all()[0].network
	j.out(num, "Remove container network: "+network)
	code, err := j.dockerCode(s, j.newDockerCmd("network", raw("rm", network)), cancel)
	if err != nil {
		return j.containerResult(s, err)
	}
	if code != 0 {
		j.tagged(num, "##[warning]", fmt.Sprintf("Docker network rm failed with exit code %d", code))
	}
	return resSucceeded
}
