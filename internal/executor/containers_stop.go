// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
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
// so it runs after a failure, a cancellation and a timeout alike; its commands
// run on a budget of their own (they do not end with the job's cancellation,
// and a stalled daemon does not hold them forever).
func (j *job) stopContainers(s *stepRun, _ <-chan struct{}) stepResult {
	if j.cs == nil {
		return resSucceeded
	}
	errs := j.cleanupContainers(s)
	for _, e := range errs {
		j.errorf(s.num, "%s", e.Error())
	}
	if len(errs) > 0 {
		return resFailed
	}
	return resSucceeded
}

// fallbackCleanup is the cleanup of the job when "Stop containers" did not (or
// not completely) run: a panic, a runner shutdown that skips the queued post
// steps, a budget that ran out. It does only what is left; with nothing left
// it runs no docker command. Its messages go to the diagnostics of the runner,
// the job log being closed.
func (j *job) fallbackCleanup() {
	if j.cs == nil || !j.cs.pending() {
		return
	}
	for _, e := range j.cleanupContainers(nil) {
		j.cfg.Log.Warn().Err(e).Msg("fallback cleanup of the job containers")
	}
}

// pending reports whether something created by the job is still to be removed.
func (cs *containerSet) pending() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.networkTry != "" && !cs.netRemoved || len(cs.dirs) > 0 {
		return true
	}
	for _, c := range cs.all() {
		if c.createTried && !c.removed {
			return true
		}
	}
	return false
}

// cleanupContainers removes what the job created: every container, then the
// network, then the credentials directories left behind. It attempts all of
// them whatever fails, within CleanupBudget, each command within
// CleanupCommand, and returns the errors. Logs of a service are best effort and
// never stand in the way of its removal. s, when set, gets the runner's lines.
// It is idempotent: what is gone is not tried again.
func (j *job) cleanupContainers(s *stepRun) (errs []error) {
	cs := j.cs
	cs.cleanupMu.Lock()
	defer cs.cleanupMu.Unlock()
	t := j.timeouts()
	budget, budgetOut, stop := bound(nil, t.CleanupBudget)
	defer stop()
	out := func(line string) {
		if s != nil {
			j.out(s.num, line)
		}
	}
	run := func(c dockerCmd) (int, error) {
		select {
		case <-budget:
			if budgetOut() {
				return 0, ghErrorf("The cleanup of the containers ran out of time (%s)", t.CleanupBudget)
			}
		default:
		}
		_, code, err := j.dockerDo(c, dockerOpts{s: s, cancel: budget, all: true, timeout: t.CleanupCommand})
		return code, err
	}
	for _, c := range cs.all() {
		if !c.createTried || c.removed {
			continue
		}
		target := c.id
		if target == "" {
			// The create is not known to have finished: the name was ours.
			target = c.name
		}
		if c.id != "" && !c.isJob && !c.failedInit {
			out("Print service container logs: " + c.name)
			code, err := run(j.newDockerCmd("logs", raw("--details", c.id)))
			switch {
			case err != nil:
				// Best effort, as the runner's warning for a failed logs: it
				// neither fails the step nor stops the removal.
				j.warnTo(s, "Docker logs failed: "+err.Error())
			case code != 0:
				j.warnTo(s, fmt.Sprintf("Docker logs fail with exit code %d", code))
			}
		}
		out("Stop and remove container: " + c.name)
		code, err := run(j.newDockerCmd("rm", raw("--force", target)))
		switch {
		case err != nil:
			errs = append(errs, err)
		case code != 0:
			j.warnTo(s, fmt.Sprintf("Docker rm fail with exit code %d", code))
			if c.id == "" {
				c.removed = true // a name that docker does not know is nothing to remove
			}
		default:
			c.removed = true
		}
	}
	cs.mu.Lock()
	network, gone := cs.networkTry, cs.netRemoved
	cs.mu.Unlock()
	if network != "" && !gone {
		out("Remove container network: " + network)
		code, err := run(j.newDockerCmd("network", raw("rm", network)))
		switch {
		case err != nil:
			errs = append(errs, err)
		case code != 0:
			j.warnTo(s, fmt.Sprintf("Docker network rm failed with exit code %d", code))
			cs.mu.Lock()
			cs.netRemoved = true // the runner warns and moves on; docker knows no such network (or refuses)
			cs.mu.Unlock()
		default:
			cs.mu.Lock()
			cs.netRemoved = true
			cs.mu.Unlock()
		}
	}
	cs.mu.Lock()
	dirs := cs.dirs
	cs.dirs = nil
	cs.mu.Unlock()
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			cs.addDir(d)
			errs = append(errs, ghErrorf("Failed to remove directory containing Docker client credentials: %v", err))
		}
	}
	return errs
}

// warnTo logs a warning to the step, or to the diagnostics without one.
func (j *job) warnTo(s *stepRun, msg string) {
	if s != nil {
		j.tagged(s.num, "##[warning]", msg)
		return
	}
	j.cfg.Log.Warn().Msg(msg)
}
