// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bnema.dev/ermite-actions/action"
	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
)

// pathMap is ContainerInfo's path translation for a docker action: the host
// directories mounted below /github. The runner inserts each mapping first, so
// the last added is tried first (ContainerInfo.AddPathTranslateMapping).
type pathMap struct{ host, container string }

func translatePath(maps []pathMap, p string) string {
	if p == "" {
		return p
	}
	for _, m := range maps {
		if p == m.host {
			return m.container
		}
		if strings.HasPrefix(p, m.host+"/") {
			return m.container + p[len(m.host):]
		}
	}
	return p
}

// ctx is the context of the job's own operations (downloads, resolutions): it
// ends when the job is cancelled.
func (j *job) ctx() context.Context {
	if j.jobCtx == nil {
		return context.Background()
	}
	return j.jobCtx
}

// dockerEnvContext evaluates against the contexts of the step with the inputs
// context replaced: the arguments and the env of a container action see its
// inputs, with the defaults (ContainerActionHandler.cs:122-131).
func (j *job) dockerEnvContext(env *ordered, in *inputSet) template.Env {
	te := j.evalEnv(env)
	ctxs := make(map[string]expr.Value, len(te.Contexts)+1)
	for k, v := range te.Contexts {
		ctxs[k] = v
	}
	d := expr.NewDictionary()
	for _, k := range in.keys {
		v, _ := in.Get(k)
		d.Add(k, expr.String(v))
	}
	ctxs["inputs"] = d
	te.Contexts = ctxs
	return te
}

// quoteArg is the quoting of one argument of the entrypoint
// (ContainerActionHandler.cs:139-148): in double quotes, a quote preceded by a
// backslash; an empty argument is "".
func quoteArg(a string) string {
	if a == "" {
		return `""`
	}
	return `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
}

// dockerBuildCmd is docker build (DockerCommandManager.DockerBuild).
func (j *job) dockerBuildCmd(tag, dockerfile, context string) dockerCmd {
	return j.newDockerCmd("build",
		dockerPart{line: "-t " + tag, args: []string{"-t", tag}},
		dockerPart{line: `-f "` + dockerfile + `"`, args: []string{"-f", dockerfile}},
		dockerPart{line: `"` + context + `"`, args: []string{context}})
}

// buildImage builds an image from a Dockerfile, in the step.
func (j *job) buildImage(s *stepRun, tag, dockerfile, workDir string, cancel <-chan struct{}) error {
	c := j.dockerBuildCmd(tag, dockerfile, filepath.Dir(dockerfile))
	code, err := j.runDockerCmd(s, c, dirOrCwd(workDir), nil, cancel)
	if err != nil {
		return err
	}
	if code != 0 {
		return ghErrorf("Docker build failed with exit code %d", code)
	}
	return nil
}

// runDocker is ContainerActionHandler.RunAsync (ContainerActionHandler.cs:
// 34-277) in a sandbox that has the docker CLI: the same mounts, variables and
// options as the runner's docker run.
func (j *job) runDocker(s *stepRun, def *actionDef, in *inputSet, env *ordered, cancel <-chan struct{}) stepResult {
	num, n := s.num, s.n
	fail := func(err error) stepResult { j.errorf(num, "%s", err.Error()); return resFailed }
	for _, k := range in.keys {
		v, _ := in.Get(k)
		env.Set(inputEnvName(k), v)
	}
	j.mu.Lock()
	workspace := j.state.GitHub("workspace")
	temp := j.state.Layout().Temp
	j.mu.Unlock()
	image := n.image
	if image == "" {
		var build string
		if def.act == nil && def.dockerfile != "" {
			build = def.dockerfile
		} else if def.act == nil {
			image = def.image
		} else {
			im, err := action.ParseImage(def.image)
			if err != nil {
				return fail(err)
			}
			if im.Kind == action.ImageRegistry {
				image = im.Ref
			} else {
				build = filepath.Join(def.dir, filepath.FromSlash(im.Path))
			}
		}
		if build != "" {
			if _, err := os.Stat(build); err != nil {
				return fail(ghErrorf("File not found: '%s'", build))
			}
			j.out(num, "##[group]Building docker image")
			j.out(num, fmt.Sprintf("Dockerfile for action: '%s'.", build))
			image = newImageTag(j.dockerLabel())
			err := j.buildImage(s, image, build, workspace, cancel)
			j.out(num, "##[endgroup]")
			if err != nil {
				if err == errCancelled {
					return j.cancelledStep(num)
				}
				return fail(err)
			}
		}
	}
	var entry string
	r := def.runs()
	switch s.stage {
	case stageMain:
		if r != nil && r.Entrypoint != "" {
			entry = r.Entrypoint
		} else if v, ok := in.Get("entryPoint"); ok {
			entry = v
		}
	case stagePre:
		if r != nil {
			entry = r.PreEntrypoint
		}
	case stagePost:
		if r != nil {
			entry = r.PostEntrypoint
		}
	}
	te := j.dockerEnvContext(env, in)
	argLine, argList, err := j.dockerArgs(r, in, te)
	if err != nil {
		j.errorf(num, "%s", templateNotValid(err, true))
		return resFailed
	}
	if r != nil && r.Env != nil {
		v, err := template.Evaluate(context.Background(), r.Env, te)
		if err != nil {
			j.errorf(num, "%s", templateNotValid(err, true))
			return resFailed
		}
		if d, ok := v.(*expr.Dictionary); ok {
			for _, p := range d.Pairs() {
				if _, has := env.Get(p.Key); !has {
					env.Set(p.Key, expr.CoerceString(p.Value))
				}
			}
		}
	}
	// The directories the container sees (ContainerActionHandler.cs:171-198).
	home := filepath.Join(temp, "_github_home")
	workflowDir := filepath.Join(temp, "_github_workflow")
	fcDir := filepath.Join(temp, "_runner_file_commands")
	for _, d := range []string{home, workflowDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fail(err)
		}
		j.own(d)
	}
	env.Set("HOME", home)
	maps := []pathMap{ // last added first
		{workspace, "/github/workspace"}, {fcDir, "/github/file_commands"}, {workflowDir, "/github/workflow"},
		{home, "/github/home"}, {temp, "/github/runner_temp"},
	}
	// The runtime variables of the contexts (GITHUB_*, RUNNER_*).
	j.mu.Lock()
	runtime := j.state.RuntimeEnv()
	j.mu.Unlock()
	for _, k := range runtime.Keys() {
		v, _ := runtime.Get(k)
		env.Set(k, v)
	}
	cenv := newOrdered()
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		cenv.Set(k, translatePath(maps, v))
	}
	parts := []dockerPart{
		raw("--name", sanitizeName(image)+"_"+shortID()),
		raw("--label", j.dockerLabel()),
		raw("--workdir", "/github/workspace"),
		raw("--rm"),
	}
	for _, k := range cenv.Keys() {
		parts = append(parts, envOpt(k))
	}
	parts = append(parts, raw("-e", "GITHUB_ACTIONS=true"))
	if _, ok := cenv.Get("CI"); !ok {
		parts = append(parts, raw("-e", "CI=true"))
	}
	if entry != "" {
		parts = append(parts, entrypointOpt(entry))
	}
	parts = append(parts,
		volumeOpt("/var/run/docker.sock", "/var/run/docker.sock"),
		volumeOpt(temp, "/github/runner_temp"),
		volumeOpt(home, "/github/home"),
		volumeOpt(workflowDir, "/github/workflow"),
		volumeOpt(fcDir, "/github/file_commands"),
		volumeOpt(workspace, "/github/workspace"),
		raw(image),
		dockerPart{line: argLine, args: argList})
	cmd := j.newDockerCmd("run", parts...)
	j.out(num, "##[command]"+cmd.line)
	// The docker process gets the container variables on top of its own
	// environment: "-e NAME" reads them from there.
	// (ProcessInvoker gets ContainerEnvironmentVariables: the translated values,
	// and nothing from the contexts on top.)
	s.rawEnvList = j.dockerProcessEnv(cenv)
	return j.runProc(s, cmd.argv[0], workspace, cmd.argv, "Docker Action run completed with exit code %d", cancel)
}

// dockerArgs is the "args" of the entrypoint: the manifest's list, evaluated and
// quoted, or the "args" input as it is (ContainerActionHandler.cs:133-161).
func (j *job) dockerArgs(r *action.Runs, in *inputSet, te template.Env) (line string, list []string, err error) {
	if r != nil && r.Args != nil {
		v, err := template.Evaluate(context.Background(), r.Args, te)
		if err != nil {
			return "", nil, err
		}
		a, _ := v.(*expr.Array)
		if a != nil {
			for _, it := range a.Items() {
				s := expr.CoerceString(it)
				line += " " + quoteArg(s)
				list = append(list, s)
			}
		}
		return line, list, nil
	}
	raw, _ := in.Get("args")
	return raw, splitArgs(raw), nil
}

// cancelledStep logs the cancellation of a step that was running docker.
func (j *job) cancelledStep(num int) stepResult {
	j.errorf(num, "The operation was canceled.")
	return resCanceled
}

func shortID() string { return strings.ReplaceAll(newImageTag(""), ":", "")[:6] }

// dockerProcessEnv is the environment of the docker CLI: the one of the runner
// and the translated container variables (DockerCommandManager.DockerRun passes
// ContainerEnvironmentVariables as the environment of the process).
func (j *job) dockerProcessEnv(cenv *ordered) []string {
	penv := newOrdered()
	for _, kv := range j.baseEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			penv.Set(k, v)
		}
	}
	for _, k := range cenv.Keys() {
		v, _ := cenv.Get(k)
		penv.Set(k, v)
	}
	penv.Set(ProcessTrackingID, j.trackID)
	var list []string
	for _, k := range penv.Keys() {
		v, _ := penv.Get(k)
		list = append(list, k+"="+v)
	}
	return list
}
