// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"path/filepath"
	"strings"

	"bnema.dev/ermite-actions/workflow"
)

// execute is ActionRunner.RunAsync plus ScriptHandler for one run step.
func (j *job) execute(s *stepRun, stepCancel <-chan struct{}) stepResult {
	num, st := s.num, s.st
	if msg := notSupported(st); msg != "" {
		if st.Uses != nil {
			j.out(num, "##[group]Run "+st.Uses.Value)
			j.out(num, "##[endgroup]")
		}
		j.errorf(num, "%s", msg)
		return resFailed
	}
	fail := func(err error) stepResult { j.errorf(num, "%s", err.Error()); return resFailed }

	if err := j.prepareFileCommands(st); err != nil {
		return fail(err)
	}
	// ProcessFiles runs after the handler, even when it failed (a finally block).
	defer j.fc.Process(s)

	in, err := j.scriptInputs(s)
	if err != nil {
		return fail(err)
	}
	plan, err := j.printHeader(s, in)
	if err != nil {
		return fail(err) // the group stays open, like the original
	}
	dir, argv, err := j.scriptCommand(plan, in)
	if err != nil {
		return fail(err)
	}
	return j.startProcess(s, plan, dir, argv, stepCancel)
}

// scriptIn is what a run step evaluates before it prints its header.
type scriptIn struct {
	script, shell, workingDirectory string
	hasWorkingDirectory             bool
}

// scriptInputs evaluates the run, shell and working-directory keys of the step
// (ActionRunner.cs:174-185). The shell and the directory fall back to
// defaults.run (ScriptHandler.cs:155-180).
func (j *job) scriptInputs(s *stepRun) (scriptIn, error) {
	st := s.st
	var in scriptIn
	var err error
	if in.script, err = j.evalString(st.Run, s.env); err != nil {
		return in, err
	}
	if in.shell, err = j.evalString(st.Shell, s.env); err != nil {
		return in, err
	}
	if in.shell == "" {
		in.shell = j.runDefaults.shell
	}
	if st.WorkingDirectory != nil {
		in.hasWorkingDirectory = true
		if in.workingDirectory, err = j.evalString(st.WorkingDirectory, s.env); err != nil {
			return in, err
		}
	}
	if !in.hasWorkingDirectory {
		in.workingDirectory = j.runDefaults.workingDirectory
	}
	return in, nil
}

// prepareFileCommands creates the files of the step, puts their paths in the
// github context (FileCommandManager.InitializeFiles) and writes event.json
// (ExecutionContext.WriteWebhookPayload).
func (j *job) prepareFileCommands(st *workflow.Step) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	files, err := j.fc.Init()
	if err != nil {
		return err
	}
	for _, c := range fileCommands {
		j.state.SetGitHub(c.ContextName, files[envVarName(c.ContextName)])
	}
	j.state.SetGitHub("action_repository", "")
	j.state.SetGitHub("action_ref", "")
	if ev := j.state.EventJSON(); ev != "" {
		if p, err := j.writeEvent(ev); err == nil {
			j.state.SetGitHub("event_path", p)
		}
	}
	return nil
}

// printHeader prints the group of the step as ScriptHandler.PrintActionDetails
// does (ScriptHandler.cs:27-137): the first line of the script, the script in
// bright cyan, the shell, the env block of the step, and the end of the group.
func (j *job) printHeader(s *stepRun, in scriptIn) (ShellPlan, error) {
	num := s.num
	j.out(num, "##[group]Run "+firstLine(in.script))
	for _, l := range strings.Split(strings.TrimRight(strings.ReplaceAll(in.script, "\r\n", "\n"), "\n"), "\n") {
		j.write(num, "\x1b[36;1m"+l+"\x1b[0m")
	}
	j.mu.Lock()
	lookup := Lookup{Path: j.lookupPath, Prepend: j.state.Prepend()}
	j.mu.Unlock()
	plan, err := PlanShell(in.shell, lookup)
	if err != nil {
		return plan, err
	}
	j.out(num, "shell: "+plan.ShellLine())
	if s.env.Len() > 0 {
		j.write(num, "env:")
		for _, k := range s.env.Keys() {
			v, _ := s.env.Get(k)
			j.out(num, fmt.Sprintf("  %s: %s", k, v))
		}
	}
	j.write(num, "##[endgroup]")
	return plan, nil
}

// scriptCommand writes the script file and returns the working directory and
// the command line (ScriptHandler.RunAsync, :140-285).
func (j *job) scriptCommand(plan ShellPlan, in scriptIn) (dir string, argv []string, err error) {
	j.mu.Lock()
	workspace := j.state.GitHub("workspace")
	j.mu.Unlock()
	sc, err := WriteScript(j.tempRoot, plan, in.script)
	if err != nil {
		return "", nil, err
	}
	return ResolveWorkingDirectory(workspace, in.workingDirectory), sc.Argv, nil
}

// processEnv builds the environment of the process: the inherited one, the
// step's environment, PATH with GITHUB_PATH in front, then the runtime variables
// of the contexts, GITHUB_ACTIONS, CI and the tracking id.
func (j *job) processEnv(stepEnv *ordered) []string {
	j.mu.Lock()
	runtime := j.state.RuntimeEnv()
	prepend := j.state.Prepend()
	j.mu.Unlock()
	penv := newOrdered()
	for _, kv := range j.baseEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			penv.Set(k, v)
		}
	}
	for _, k := range stepEnv.Keys() {
		v, _ := stepEnv.Get(k)
		penv.Set(k, v)
	}
	if len(prepend) > 0 {
		orig, ok := stepEnv.Get("PATH")
		if !ok {
			orig = j.lookupPath
		}
		penv.Set("PATH", PrependedPath(prepend, orig))
	}
	for _, k := range runtime.Keys() {
		v, _ := runtime.Get(k)
		penv.Set(k, v)
	}
	penv.Set("GITHUB_ACTIONS", "true") // ProcessInvoker.cs:283
	if _, ok := penv.Get("CI"); !ok {
		penv.Set("CI", "true")
	}
	penv.Set(ProcessTrackingID, j.trackID)
	var list []string
	for _, k := range penv.Keys() {
		v, _ := penv.Get(k)
		list = append(list, k+"="+v)
	}
	return list
}

// startProcess runs the script and turns how it ended into the result of the
// step.
func (j *job) startProcess(s *stepRun, plan ShellPlan, dir string, argv []string, stepCancel <-chan struct{}) stepResult {
	num := s.num
	res, err := runProcess(procSpec{
		Argv: argv, Dir: dir, Env: j.processEnv(s.env), Credential: j.cfg.Credential,
		OnLine: func(line string) {
			if strings.Contains(line, "##[start-action") || strings.Contains(line, "##[end-action") {
				line = strings.ReplaceAll(strings.ReplaceAll(line, "##[start-action", `##[\start-action`), "##[end-action", `##[\end-action`)
			}
			if (strings.Contains(line, "##[") || strings.Contains(line, "::")) && s.cmds.TryProcess(line) {
				return
			}
			j.write(num, line)
		},
		Cancel: stepCancel, TermAfter: j.cfg.TermAfter, KillAfter: j.cfg.KillAfter,
		WaitDelay: j.cfg.WaitDelay, Signal: j.cfg.Signal,
	})
	if err != nil {
		j.errorf(num, "%s", startError(plan.CommandPath, dir, err))
		return resFailed
	}
	if res.Cancelled {
		// The process was stopped on cancellation: ExecuteAsync throws
		// (ProcessInvoker.cs:369) and StepsRunner logs the exception
		// (StepsRunner.cs:333-337).
		j.errorf(num, "The operation was canceled.")
		return resCanceled
	}
	if res.ExitCode != 0 {
		j.errorf(num, "Process completed with exit code %d.", res.ExitCode)
		return resFailed
	}
	return resSucceeded
}

// writeEvent writes the webhook payload to RUNNER_TEMP/_github_workflow/event.json
// (ExecutionContext.WriteWebhookPayload) and returns its path. The directory
// is the user's: the file is replaced, never followed (see FileCommands).
func (j *job) writeEvent(payload string) (string, error) {
	const dir, name = "_github_workflow", "event.json"
	if err := j.tempRoot.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if c := j.cfg.Credential; c != nil {
		_ = j.tempRoot.Lchown(dir, int(c.Uid), int(c.Gid))
	}
	sub, err := j.tempRoot.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = sub.Close() }()
	var owner *[2]int
	if c := j.cfg.Credential; c != nil {
		owner = &[2]int{int(c.Uid), int(c.Gid)}
	}
	if err := createExcl(sub, name, []byte(payload), 0o644, owner); err != nil {
		return "", err
	}
	return filepath.Join(j.state.Layout().Temp, dir, name), nil
}
