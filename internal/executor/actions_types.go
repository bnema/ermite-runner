// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bnema.dev/ermite-actions/action"
	"bnema.dev/ermite-actions/workflow"
)

// ResolvedAction is where to download one action (ermite.ResolvedAction, see
// the host). The Token is a secret: the executor masks it.
type ResolvedAction struct {
	Ref, NameWithOwner, Path, ResolvedSHA, TarballURL, Token, Error string
	// AuthOrigin is the only origin (scheme://host[:port]) the Token is sent to;
	// the host sets it. Without it the token is not sent.
	AuthOrigin string
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
	// RequireNode24 is the server flag actions.runner.requirenode24 (phase 3
	// of the migration): every node20 action runs on node24 whatever
	// FORCE_JAVASCRIPT_ACTIONS_TO_NODE24 and ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION
	// say, without the "being deprecated" notice or the conflict warning.
	// github.com does so today (recordings act-node-*). The zero value is the
	// runner's phase 1; the guest sets both flags (see DefaultActionsConfig).
	RequireNode24 bool
	// Containers are the time limits of the docker commands of job containers
	// and services; the zero value means the defaults.
	Containers ContainerTimeouts
	// Backoff, when set, replaces the random 10 to 30 s wait between download
	// attempts (tests).
	Backoff func() time.Duration
}

// DefaultActionsConfig is the configuration of a job in the sandbox: GitHub's
// current node migration phase, in which node12, node16 and node20 actions run
// on node24 and the job ends with the "forced to run on Node.js 24" warning.
func DefaultActionsConfig(r Resolver) ActionsConfig {
	return ActionsConfig{Resolver: r, Node24ByDefault: true, RequireNode24: true}
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

// kind is what an action runs.
type kind int

const (
	kindScript kind = iota
	kindNode
	kindDocker
	kindComposite
)

// actionDef is a loaded action (ActionManager.LoadAction's Definition).
type actionDef struct {
	kind kind
	// dir is the directory of the action ("" for docker://).
	dir string
	act *action.Action // nil for docker:// and for a Dockerfile without manifest
	// image is the "runs.image" of a container action, or the image of a
	// docker:// step; "" for Dockerfile-only actions (see dockerfile).
	image string
	// dockerfile is the path of the Dockerfile of a Dockerfile-only action.
	dockerfile string
}

func (d *actionDef) runs() *action.Runs {
	if d.act == nil {
		return nil
	}
	return d.act.Runs
}

// preIf and postIf are InitCondition and CleanupCondition: always() unless the
// manifest says otherwise (ActionExecutionData, ActionManager.cs:1884-1900).
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

// node is one step of the job or of a composite action, with what the runner
// keeps per action step: its intra-action state, and for a composite its
// children and its pre and post steps (CompositeActionExecutionData).
type node struct {
	st     *workflow.Step
	uses   action.Uses
	parent *node
	// scope is the name of the steps context of the step's children: the
	// fully qualified context name (ExecutionContext.GetFullyQualifiedContextName).
	scope string
	// stateKey names the intra-action state of the action (job.saveState): the
	// pre, main and post stages of an action, and its inner steps, share it
	// (the runner keys it by the id of the step, FileCommandManager.cs:305-318).
	stateKey string
	// children are the nodes of a composite action's steps.
	children []*node
	// preSteps and postSteps are the embedded steps with a pre or a post, in
	// declaration order (the runner's post steps are a stack: they run last
	// to first).
	preSteps, postSteps []*node
	// image is the image the setup steps pulled or built for the step.
	image string
	def   *actionDef
	// postRegistered is Root.StepsWithPostRegistered / EmbeddedStepsWithPostRegistered.
	postRegistered bool
	postCond       string
}

// nodeSeq numbers the nodes, for their state keys.
var nodeSeq atomic.Int64

// newNodes makes the nodes of steps. The workflow reader has checked every
// uses value.
func newNodes(steps []*workflow.Step, parent *node, scope string) ([]*node, error) {
	out := make([]*node, 0, len(steps))
	for _, st := range steps {
		n := &node{st: st, parent: parent, stateKey: "node-" + strconv.FormatInt(nodeSeq.Add(1), 10)}
		n.scope = st.ID
		if scope != "" {
			n.scope = scope + "." + st.ID
		}
		if st.Uses != nil {
			u, err := action.ParseUses(st.Uses.Value)
			if err != nil {
				return nil, err
			}
			n.uses = u
		}
		out = append(out, n)
	}
	return out, nil
}
