// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"fmt"
	"strings"

	"bnema.dev/ermite-actions/action"
)

// actionState is what the pre-job phase leaves for the run.
type actionState struct {
	nodes []*node
	// pulled and built map an image to what the setup steps produced.
	pulled map[string]string // image -> image
	built  map[string]string // action repository -> tag
	// setup are the steps that pull and build images, in the order of
	// ActionManager.PrepareActionsAsync (pulls, then builds).
	pullOrder, buildOrder []string
	buildInfo             map[string]buildInfo
	pullUsers             map[string][]*node
	buildUsers            map[string][]*node
	// pre are the pre steps of the job's steps with a pre stage, by node.
	pre map[*node]bool
	// resolved caches the download info by "name@ref" (ActionManager's legacy
	// path asks again at every level; ermite's server caches per run).
	downloaded map[string]bool
}

type buildInfo struct{ repository, dockerfile, workDir string }

func newActionState() *actionState {
	return &actionState{
		pulled: map[string]string{}, built: map[string]string{}, buildInfo: map[string]buildInfo{},
		pullUsers: map[string][]*node{}, buildUsers: map[string][]*node{}, pre: map[*node]bool{},
		downloaded: map[string]bool{},
	}
}

// lookupKey is ActionManager.GetDownloadInfoLookupKey: "Name@Ref" for a
// repository action, "" for a local one.
func lookupKey(n *node) (string, error) {
	switch n.uses.Kind {
	case action.UsesRepository:
		return n.uses.Name + "@" + n.uses.Ref, nil
	case action.UsesSelfRepository:
		return "", ghErrorf("Unable to resolve self-reference '$/'. This can occur when the server does not support this syntax, the feature flag is disabled, or the workflow context (repository/SHA) is unavailable.")
	}
	return "", nil
}

// prepare is ActionManager.PrepareActionsRecursiveLegacyAsync
// (ActionManager.cs:439-654): resolve and download the repository actions of
// nodes, read their manifests, recurse into composite actions, and note the
// images to pull or build and the pre and post stages. depth is 0 for the
// steps of the job and 1 for the ones of a local composite action prepared when
// it runs.
func (j *job) prepare(ctx context.Context, num int, nodes []*node, depth int, parent *node) error {
	if depth > compositeMaxDepth {
		return fmt.Errorf("Composite action depth exceeded max depth %d", compositeMaxDepth)
	}
	st := j.actions
	var repoNodes []*node
	for _, n := range nodes {
		switch n.uses.Kind {
		case action.UsesDocker:
			if n.uses.Image == "" {
				return fmt.Errorf("Value cannot be null or empty. (Parameter 'Image')")
			}
			if _, ok := st.pullUsers[n.uses.Image]; !ok {
				st.pullOrder = append(st.pullOrder, n.uses.Image)
			}
			st.pullUsers[n.uses.Image] = append(st.pullUsers[n.uses.Image], n)
		case action.UsesRepository, action.UsesLocal, action.UsesSelfRepository:
			repoNodes = append(repoNodes, n)
		}
	}
	if len(repoNodes) == 0 {
		return nil
	}
	infos, err := j.downloadInfo(ctx, num, repoNodes)
	if err != nil {
		return err
	}
	for _, n := range repoNodes {
		key, err := lookupKey(n)
		if err != nil {
			return err
		}
		if key == "" {
			continue
		}
		ra, ok := infos[key]
		if !ok {
			return fmt.Errorf("Missing download info for %s", key)
		}
		dir := j.actionDirOfRepo(n)
		// The directory of the archive is the one of the repository itself: the
		// action's path is below it.
		root := j.state.Layout().Actions + "/" + n.uses.Name + "/" + n.uses.Ref
		_ = dir
		if err := j.downloadAction(ctx, num, ra, ra.NameWithOwner, n.uses.Ref, root); err != nil {
			return err
		}
	}
	for _, n := range repoNodes {
		if err := j.prepareRepositoryAction(ctx, num, n, depth); err != nil {
			return err
		}
	}
	return nil
}

func (j *job) actionDirOfRepo(n *node) string { return j.actionDir(n) }

// downloadInfo is ActionManager.GetDownloadInfoAsync (ActionManager.cs:1000-1135):
// it asks the server, through the host, about the distinct repository actions
// of nodes. The server's answer for one action may be an error in GitHub's
// words, which is the message of the failure.
func (j *job) downloadInfo(ctx context.Context, num int, nodes []*node) (map[string]ResolvedAction, error) {
	j.out(num, "Getting action download info")
	var uses, keys []string
	seen := map[string]bool{}
	for _, n := range nodes {
		key, err := lookupKey(n)
		if err != nil {
			return nil, err
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
		uses = append(uses, refString(n.uses))
	}
	out := map[string]ResolvedAction{}
	if len(uses) == 0 {
		return out, nil
	}
	if j.cfg.Actions.Resolver == nil {
		return nil, ghErrorf("Failed to resolve action download info.")
	}
	res, err := j.cfg.Actions.Resolver(ctx, uses)
	if err != nil {
		return nil, ghErrorf("%s", err.Error())
	}
	if len(res) != len(uses) {
		return nil, ghErrorf("Failed to resolve action download info.")
	}
	for i, ra := range res {
		if ra.Error != "" {
			return nil, ghErrorf("%s", ra.Error)
		}
		// The token is a secret from here on (ActionManager.cs:1130).
		j.addTokenSecret(ra.Token)
		if ra.NameWithOwner == "" {
			ra.NameWithOwner = strings.SplitN(keys[i], "@", 2)[0]
		}
		out[keys[i]] = ra
	}
	return out, nil
}

// prepareRepositoryAction is ActionManager.PrepareRepositoryActionAsync
// (:1411-1550) and the code after it in the loop of the caller: read the
// manifest, note an image to pull or build, recurse into a composite, and
// register pre and post stages.
func (j *job) prepareRepositoryAction(ctx context.Context, num int, n *node, depth int) error {
	local := n.uses.Kind == action.UsesLocal
	if local {
		if depth > 0 {
			// "if we're in a composite action and haven't loaded the local action
			// yet we assume it has a post step" (ActionManager.cs:643-654).
			n.parentPost()
		}
		return nil
	}
	dir := j.actionDir(n)
	def, err := j.loadDef(num, dir)
	if err == errNoAction {
		return fmt.Errorf("Can't find 'action.yml', 'action.yaml' or 'Dockerfile' for action '%s'.", refString(n.uses))
	}
	if err != nil {
		return err
	}
	n.def = def
	repo := n.uses.Name + "@" + n.uses.Ref
	if n.uses.Path != "" {
		repo = n.uses.Name + "/" + n.uses.Path + "@" + n.uses.Ref
	}
	root := j.state.Layout().Actions + "/" + n.uses.Name + "/" + n.uses.Ref
	st := j.actions
	switch def.kind {
	case kindDocker:
		img, build, err := j.dockerImageOf(def)
		if err != nil {
			return err
		}
		if build != "" {
			if _, ok := st.buildUsers[repo]; !ok {
				st.buildOrder = append(st.buildOrder, repo)
			}
			st.buildUsers[repo] = append(st.buildUsers[repo], n)
			st.buildInfo[repo] = buildInfo{repository: repo, dockerfile: build, workDir: root}
		} else {
			if _, ok := st.pullUsers[img]; !ok {
				st.pullOrder = append(st.pullOrder, img)
			}
			st.pullUsers[img] = append(st.pullUsers[img], n)
		}
	case kindComposite:
		if err := j.prepareComposite(ctx, num, n, depth); err != nil {
			return err
		}
	}
	j.registerStages(n, depth)
	return nil
}

// dockerImageOf says what a container action needs: an image to pull, or a
// Dockerfile to build (absolute path).
func (j *job) dockerImageOf(def *actionDef) (pull, build string, err error) {
	if def.act == nil {
		return "", def.dockerfile, nil
	}
	im, err := action.ParseImage(def.image)
	if err != nil {
		return "", "", err
	}
	if im.Kind == action.ImageDockerfile {
		return "", def.dir + "/" + im.Path, nil
	}
	return im.Ref, "", nil
}

// prepareComposite creates the nodes of a composite action's steps and prepares
// them one level down.
func (j *job) prepareComposite(ctx context.Context, num int, n *node, depth int) error {
	steps := n.def.act.Runs.Steps
	kids, err := newNodes(steps, n, n.scope)
	if err != nil {
		return err
	}
	n.children = kids
	return j.prepare(ctx, num, kids, depth+1, n)
}

// registerStages notes the pre and post stages of a prepared action that is not
// local (ActionManager.cs:600-640): for the steps of the job a pre step, for
// the steps of a composite action a pre and post step of the composite.
func (j *job) registerStages(n *node, depth int) {
	pre, post := n.hasPre(), n.hasPost()
	if depth < 1 {
		if pre {
			j.actions.pre[n] = true
		}
		return
	}
	if p := n.parent; p != nil {
		if pre {
			p.preSteps = append(p.preSteps, n)
		}
		if post {
			p.postSteps = append(p.postSteps, n)
		}
	}
}

// parentPost is the assumption for a local action inside a composite action.
func (n *node) parentPost() {
	if p := n.parent; p != nil {
		p.postSteps = append(p.postSteps, n)
	}
}

// hasPre and hasPost are ActionExecutionData.HasPre and HasPost.
func (n *node) hasPre() bool {
	d := n.def
	if d == nil {
		return false
	}
	if d.kind == kindComposite {
		return len(n.preSteps) > 0
	}
	if r := d.runs(); r != nil {
		return r.HasPre()
	}
	return false
}

func (n *node) hasPost() bool {
	d := n.def
	if d == nil {
		return false
	}
	if d.kind == kindComposite {
		return len(n.postSteps) > 0
	}
	if r := d.runs(); r != nil {
		return r.HasPost()
	}
	return false
}
