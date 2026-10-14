// SPDX-License-Identifier: MIT

package executor

import "os"

// prepareJob is the actions part of "Set up job" (ActionManager.PrepareActionsAsync
// and JobExtension.cs:299-362): resolve and download the actions of the job,
// and build the queue of the steps: the steps that pull and build images, the
// pre stages in the order of the steps, then the steps themselves.
func (j *job) prepareJob() ([]qstep, error) {
	j.actions = newActionState()
	// The actions of a previous job are not reused (ActionManager.cs:97-100).
	_ = os.RemoveAll(j.state.Layout().Actions)
	if err := os.MkdirAll(j.state.Layout().Actions, 0o755); err != nil {
		return nil, err
	}
	j.own(j.state.Layout().Actions)
	nodes, err := newNodes(j.jb.Steps, nil, "")
	if err != nil {
		return nil, err
	}
	j.actions.nodes = nodes
	if err := j.prepare(j.ctx(), 1, nodes, 0, nil); err != nil {
		return nil, j.setupError(err)
	}
	queue := j.setupSteps()
	for _, n := range nodes {
		if j.actions.pre[n] {
			queue = append(queue, qstep{n: n, stage: stagePre, cond: n.def.preIf()})
		}
	}
	// The pre steps come before every main step: they were collected while
	// walking the steps, the mains follow.
	for _, n := range nodes {
		queue = append(queue, qstep{n: n, stage: stageMain})
	}
	return queue, nil
}

// setupError words an error of the preparation like the runner reports it:
// errors of the manifests were logged already; "Failed to resolve action
// download info." and "Failed to download archive ..." carry the inner message.
func (j *job) setupError(err error) error {
	return err
}
