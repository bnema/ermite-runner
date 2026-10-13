// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"net/http"
	"strings"

	"bnema.dev/ermite-actions/action"
	"bnema.dev/ermite-actions/workflow"
)

// ResolvedAction is where to download one action (ermite.ResolvedAction, see
// the host). The Token is a secret: the executor masks it.
type ResolvedAction struct {
	Ref, NameWithOwner, Path, ResolvedSHA, TarballURL, Token, Error string
}

// Resolver resolves "owner/repo[/path]@ref" values of uses with the server. It
// is the host's call (the job token stays out of the sandbox); the result has
// one entry per value, in order.
type Resolver func(ctx context.Context, uses []string) ([]ResolvedAction, error)

// ActionsConfig is what the executor needs to run actions.
type ActionsConfig struct {
	// Resolver is required to run actions from repositories; without it a job
	// that uses one fails "Set up job".
	Resolver Resolver
	// HTTPClient downloads the archives; nil means a client with the default
	// transport. It must not follow a redirect to another host with the
	// credentials (net/http does not).
	HTTPClient *http.Client
	// ExternalsDir is where Node.js lives (<dir>/node20/bin/node); empty means
	// /home/runner/externals.
	ExternalsDir string
	// DockerPath is the docker CLI; empty means the one found in PATH.
	DockerPath string
	// Node24ByDefault is the server flag actions.runner.usenode24bydefault
	// (HandlerFactory.cs, NodeUtil.DetermineActionsNodeVersion): ermite has no
	// feature flags, so the default is pinned here.
	Node24ByDefault bool
}

const defaultExternalsDir = "/home/runner/externals"

// stage is ActionRunStage.
type stage int

const (
	stageMain stage = iota
	stagePre
	stagePost
)

// compositeMaxDepth is Constants.CompositeActionsMaxDepth
// (Runner.Common/Constants.cs:46).
const compositeMaxDepth = 9

// actionDef is a loaded action: its directory and manifest (ActionManager.
// LoadAction's Definition). A docker:// step has no manifest and no directory.
type actionDef struct {
	uses action.Uses
	// ref is "Name[/Path]@Ref" for repository actions, the path for local ones.
	ref string
	dir string
	act *action.Action // nil for docker:// and Dockerfile-only actions
	// image is what a container action runs: "docker://x" steps and docker://
	// manifests give the image to pull, Dockerfile ones the path of the file.
	image      string
	dockerfile bool
	// children are the loaded actions of a composite's steps, by step.
	children map[*workflow.Step]*actionDef
}

func (d *actionDef) runs() *action.Runs {
	if d.act == nil {
		return nil
	}
	return d.act.Runs
}

// hasPre and hasPost are ActionExecutionData.HasPre/HasPost.
func (d *actionDef) hasPre() bool {
	r := d.runs()
	switch {
	case r == nil:
		return false
	case r.Kind == action.RunsComposite:
		return d.anyChild(func(c *actionDef) bool { return c.hasPre() && !c.local() })
	}
	return r.HasPre()
}

func (d *actionDef) hasPost() bool {
	r := d.runs()
	switch {
	case r == nil:
		return false
	case r.Kind == action.RunsComposite:
		return d.anyChild(func(c *actionDef) bool { return c.local() || c.hasPost() })
	}
	return r.HasPost()
}

func (d *actionDef) anyChild(f func(*actionDef) bool) bool {
	for _, c := range d.children {
		if c != nil && f(c) {
			return true
		}
	}
	return false
}

func (d *actionDef) local() bool { return d.uses.Kind == action.UsesLocal }

// preIf and postIf are InitCondition and CleanupCondition: always() unless the
// manifest says otherwise (action.Runs keeps them as written).
func (d *actionDef) preIf() string  { return condOrAlways(d.runs(), true) }
func (d *actionDef) postIf() string { return condOrAlways(d.runs(), false) }

func condOrAlways(r *action.Runs, pre bool) string {
	if r == nil || r.Kind == action.RunsComposite {
		return "always()"
	}
	c := r.PostIf
	if pre {
		c = r.PreIf
	}
	if strings.TrimSpace(c) == "" {
		return "always()"
	}
	return c
}
