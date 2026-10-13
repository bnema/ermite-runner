// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bnema.dev/ermite-actions/action"
)

// loadManifest reads action.yml or action.yaml (the first is preferred) from
// dir, like ActionManager.LoadAction (ActionManager.cs:722-731) and
// ActionManifestManagerLegacy.Load (:53-160): every error of the manifest is
// logged as an error of the step, then the load fails with "Failed to load
// <path>". shown is the path the messages use: relative to the actions
// directory for downloaded actions, the whole path for local ones
// (ActionManifestManagerLegacy.cs:61-66). It returns (nil, "", nil) without a
// manifest.
func (j *job) loadManifest(num int, dir string) (*action.Action, string, error) {
	var file string
	for _, name := range []string{"action.yml", "action.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			file = filepath.Join(dir, name)
			break
		}
	}
	if file == "" {
		return nil, "", nil
	}
	shown := file
	if base := j.state.Layout().Actions; strings.HasPrefix(file, base+string(filepath.Separator)) {
		shown = file[len(base)+1:]
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", err
	}
	act, errs := action.Parse(shown, data)
	if len(errs) > 0 {
		for _, e := range errs {
			j.errorf(num, "%s", e.Error())
		}
		return nil, "", ghErrorf("%s", action.LoadFailed(shown))
	}
	return act, shown, nil
}

func kindOfRuns(r *action.Runs) kind {
	switch r.Kind {
	case action.RunsNode:
		return kindNode
	case action.RunsDocker:
		return kindDocker
	case action.RunsComposite:
		return kindComposite
	}
	return kindScript
}

// loadDef loads the action in dir (ActionManager.LoadAction): its manifest, or
// a Dockerfile when there is none (Dockerfile before dockerfile). what names
// the action in the error for a directory without any.
func (j *job) loadDef(num int, dir string) (*actionDef, error) {
	act, _, err := j.loadManifest(num, dir)
	if err != nil {
		return nil, err
	}
	if act != nil {
		d := &actionDef{kind: kindOfRuns(act.Runs), dir: dir, act: act}
		switch act.Runs.Kind {
		case action.RunsDocker:
			d.image = act.Runs.Image
		case action.RunsPlugin:
			return nil, fmt.Errorf("Unsupported action type: Plugin")
		}
		return d, nil
	}
	for _, name := range []string{"Dockerfile", "dockerfile"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return &actionDef{kind: kindDocker, dir: dir, dockerfile: filepath.Join(dir, name)}, nil
		}
	}
	return nil, errNoAction
}

// errNoAction is the sentinel of a directory without manifest and Dockerfile.
var errNoAction = fmt.Errorf("no action")

// actionDir is where the action of a node lives: the workspace for a local
// action, the download directory for a repository one (ActionManager.cs:665-694).
func (j *job) actionDir(n *node) string {
	switch n.uses.Kind {
	case action.UsesLocal:
		return filepath.Join(j.state.GitHub("workspace"), filepath.FromSlash(strings.ReplaceAll(n.uses.Path, `\`, "/")))
	case action.UsesRepository:
		d := filepath.Join(j.state.Layout().Actions, filepath.FromSlash(n.uses.Name), n.uses.Ref)
		if n.uses.Path != "" {
			d = filepath.Join(d, filepath.FromSlash(n.uses.Path))
		}
		return d
	}
	return ""
}

// refString is "Name[/Path]@Ref" (ActionRunner.cs:119-121, Handler.cs:130-148).
func refString(u action.Uses) string {
	switch u.Kind {
	case action.UsesRepository:
		p := u.Name
		if u.Path != "" {
			p += "/" + u.Path
		}
		return p + "@" + u.Ref
	case action.UsesLocal:
		return u.Path
	case action.UsesDocker:
		return "docker://" + u.Image
	}
	return ""
}
