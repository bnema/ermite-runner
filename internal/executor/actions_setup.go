// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"path/filepath"
	"time"
)

const (
	pullAttempts  = 3
	buildAttempts = 3
)

// setupSteps are the steps that pull and build the images of the actions, as
// JobExtensionRunner steps before the first one of the job
// (ActionManager.PrepareActionsAsync, ActionManager.cs:134-166): the pulls in
// the order the images were met, then the builds. They run if success().
func (j *job) setupSteps() []qstep {
	st := j.actions
	var out []qstep
	for _, img := range st.pullOrder {
		img, users := img, st.pullUsers[img]
		out = append(out, qstep{name: "Pull " + img, setup: func(s *stepRun, cancel <-chan struct{}) stepResult {
			return j.pullImage(s, img, users, cancel)
		}})
	}
	for _, repo := range st.buildOrder {
		users, info := st.buildUsers[repo], st.buildInfo[repo]
		out = append(out, qstep{name: "Build " + info.repository, setup: func(s *stepRun, cancel <-chan struct{}) stepResult {
			return j.buildActionImage(s, info, users, cancel)
		}})
	}
	return out
}

// pullImage is ActionManager.PullActionContainerAsync (ActionManager.cs:803-846).
func (j *job) pullImage(s *stepRun, image string, users []*node, cancel <-chan struct{}) stepResult {
	num := s.num
	j.out(num, fmt.Sprintf("##[group]Pull down action image '%s'", image))
	code := 0
	for try := 1; try <= pullAttempts; try++ {
		c := j.newDockerCmd("pull", raw(image))
		var err error
		code, err = j.runDockerCmd(s, c, dirOrCwd(j.state.GitHub("workspace")), nil, cancel)
		if err == errCancelled {
			j.out(num, "##[endgroup]")
			return j.cancelledStep(s)
		}
		if err != nil {
			j.out(num, "##[endgroup]")
			j.errorf(num, "%s", err.Error())
			return resFailed
		}
		if code == 0 {
			break
		}
		if try < pullAttempts {
			d := j.pullBackoff()
			j.tagged(num, "##[warning]", fmt.Sprintf("Docker pull failed with exit code %d, back off %v seconds before retry.", code, d.Seconds()))
			if !j.sleepOrCancel(d, cancel) {
				j.out(num, "##[endgroup]")
				return j.cancelledStep(s)
			}
		}
	}
	j.out(num, "##[endgroup]")
	if code != 0 {
		j.errorf(num, "Docker pull failed with exit code %d", code)
		return resFailed
	}
	for _, n := range users {
		n.image = image
	}
	return resSucceeded
}

// buildActionImage is ActionManager.BuildActionContainerAsync (:848-910).
func (j *job) buildActionImage(s *stepRun, info buildInfo, users []*node, cancel <-chan struct{}) stepResult {
	num := s.num
	j.out(num, fmt.Sprintf("##[group]Build container for action use: '%s'.", info.dockerfile))
	tag := newImageTag(j.dockerLabel())
	code := 0
	for try := 1; try <= buildAttempts; try++ {
		// Directory.GetParent(Dockerfile).FullName: the parent after Path.GetFullPath,
		// which folds "." and ".." lexically, as filepath.Dir does; the Dockerfile
		// itself is passed as written.
		c := j.dockerBuildCmd(tag, info.dockerfile, filepath.Dir(info.dockerfile))
		var err error
		code, err = j.runDockerCmd(s, c, dirOrCwd(info.workDir), nil, cancel)
		if err == errCancelled {
			j.out(num, "##[endgroup]")
			return j.cancelledStep(s)
		}
		if err != nil {
			j.out(num, "##[endgroup]")
			j.errorf(num, "%s", err.Error())
			return resFailed
		}
		if code == 0 {
			break
		}
		if try < buildAttempts {
			d := j.pullBackoff()
			j.tagged(num, "##[warning]", fmt.Sprintf("Docker build failed with exit code %d, back off %v seconds before retry.", code, d.Seconds()))
			if !j.sleepOrCancel(d, cancel) {
				j.out(num, "##[endgroup]")
				return j.cancelledStep(s)
			}
		}
	}
	j.out(num, "##[endgroup]")
	if code != 0 {
		j.errorf(num, "Docker build failed with exit code %d", code)
		return resFailed
	}
	for _, n := range users {
		n.image = tag
	}
	return resSucceeded
}

// pullBackoff is a random wait of 1 to 10 seconds.
func (j *job) pullBackoff() time.Duration {
	if f := j.cfg.Actions.Backoff; f != nil {
		return f()
	}
	return time.Duration(1+time.Now().UnixNano()%10) * time.Second
}

func (j *job) sleepOrCancel(d time.Duration, cancel <-chan struct{}) bool {
	select {
	case <-time.After(d):
		return true
	case <-cancel:
		return false
	}
}
