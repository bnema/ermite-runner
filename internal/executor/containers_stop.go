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
		l, err := j.dockerList(s, cmd, cancel)
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
		j.diagWarn(e, "fallback cleanup of the job containers")
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
	cs.mu.Lock()
	if cs.deadline.IsZero() {
		cs.deadline = time.Now().Add(t.CleanupBudget)
	}
	remaining := time.Until(cs.deadline)
	cs.mu.Unlock()
	budget, budgetOut, stop := bound(nil, remaining)
	defer stop()
	defer func() { errs = dedupeErrors(errs) }()
	out := func(line string) {
		if s != nil {
			j.out(s.num, line)
		}
	}
	run := func(c dockerCmd) (int, error) {
		_, code, err := j.dockerDoCleanup(c, s, budget, budgetOut, t, t.CleanupCommand)
		return code, err
	}
	// rm runs `docker rm --force target`. It reports whether the target is
	// gone: removed, or docker says it does not exist. Any other failure leaves
	// it pending, for the fallback of the job to try again.
	rm := func(kind string, c dockerCmd) (gone bool) {
		lines, code, err := j.dockerDoCleanup(c, s, budget, budgetOut, t, t.CleanupCommand)
		switch {
		case err != nil:
			errs = append(errs, err)
			return false
		case code == 0:
			return true
		case absent(lines):
			return true
		}
		j.warnTo(s, fmt.Sprintf("Docker %s fail with exit code %d", kind, code))
		return false
	}
	// A container whose create did not finish is not known by its id, and its
	// name may not be ours (options: can set another --name): it is found by
	// the label every container of the job carries (DockerCreate adds it).
	var unresolved []*container
	for _, c := range cs.all() {
		if c.createTried && !c.removed && c.id == "" {
			unresolved = append(unresolved, c)
		}
	}
	if len(unresolved) > 0 {
		ok := true
		ids, code, err := j.dockerDoCleanup(j.newDockerCmd("ps", raw("--all", "--quiet", "--no-trunc"), quotedOpt("--filter", "label="+j.dockerLabel())), nil, budget, budgetOut, t, t.CleanupCommand)
		switch {
		case err != nil:
			errs, ok = append(errs, err), false
		case code != 0:
			j.warnTo(s, fmt.Sprintf("Docker ps fail with exit code %d", code))
			ok = false
		}
		known := map[string]bool{}
		for _, c := range cs.all() {
			if c.id != "" {
				known[c.id] = true // those are removed below, after their logs
			}
		}
		for _, id := range ids {
			if containerIDRe.MatchString(id) && !known[id] && !rm("rm", j.newDockerCmd("rm", raw("--force", id))) {
				ok = false
			}
		}
		if ok { // listed, and everything listed is gone: nothing of the job is left
			for _, c := range unresolved {
				c.removed = true
			}
		}
	}
	for _, c := range cs.all() {
		if !c.createTried || c.removed || c.id == "" {
			continue
		}
		if !c.isJob && !c.failedInit {
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
		if rm("rm", j.newDockerCmd("rm", raw("--force", c.id))) {
			c.removed = true
		}
	}
	cs.mu.Lock()
	network, gone := cs.networkTry, cs.netRemoved
	cs.mu.Unlock()
	if network != "" && !gone {
		out("Remove container network: " + network)
		if rm("network rm", j.newDockerCmd("network", raw("rm", network))) {
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
	j.diagWarn(nil, msg)
}

// absent reports whether docker's answer to a removal says the object does not
// exist: that is the state the cleanup wants.
func absent(lines []string) bool {
	for _, l := range lines {
		l = strings.ToLower(l)
		if strings.Contains(l, "no such container") || strings.Contains(l, "no such network") ||
			strings.Contains(l, "no such object") || strings.Contains(l, "network") && strings.Contains(l, "not found") {
			return true
		}
	}
	return false
}

// dockerDoCleanup runs a command of the cleanup: bounded by its own limit and
// by the budget of the whole cleanup, its output (stdout and stderr) returned,
// logged to s if any. The budget is checked here, before every command: once
// it is spent nothing is started (a command started with a closed cancel would
// still cost a signal escalation) and the object stays pending.
func (j *job) dockerDoCleanup(c dockerCmd, s *stepRun, budget <-chan struct{}, budgetOut func() bool, t ContainerTimeouts, limit time.Duration) ([]string, int, error) {
	spent := func() error {
		return ghErrorf("The cleanup of the containers ran out of time (%s): not run: docker %s", t.CleanupBudget, j.dockerArgString(c))
	}
	select {
	case <-budget:
		return nil, 0, spent()
	default:
	}
	lines, code, err := j.dockerDo(c, dockerOpts{s: s, cancel: budget, all: true, timeout: limit, errToOut: true})
	if err == errCancelled && budgetOut() {
		return lines, code, ghErrorf("The cleanup of the containers ran out of time (%s): stopped: docker %s", t.CleanupBudget, j.dockerArgString(c))
	}
	return lines, code, err
}

// dedupeErrors keeps the first of the errors that read the same.
func dedupeErrors(errs []error) []error {
	seen := map[string]bool{}
	var out []error
	for _, e := range errs {
		if !seen[e.Error()] {
			seen[e.Error()] = true
			out = append(out, e)
		}
	}
	return out
}
