// SPDX-License-Identifier: MIT

package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// execute is ActionRunner.RunAsync plus ScriptHandler for one run step.
func (j *job) execute(s *stepRun, stepCancel <-chan struct{}) (result stepResult) {
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

	// File commands for this step: the github context carries the paths
	// (FileCommandManager.InitializeFiles), event.json is written
	// (ExecutionContext.WriteWebhookPayload).
	j.mu.Lock()
	files, ferr := j.fc.Init()
	if ferr == nil {
		for _, c := range fileCommands {
			j.state.SetGitHub(c.ContextName, files[envVarName(c.ContextName)])
		}
		j.state.SetGitHub("action", st.ID)
		j.state.SetGitHub("action_repository", "")
		j.state.SetGitHub("action_ref", "")
		if ev := j.state.EventJSON(); ev != "" {
			dir := filepath.Join(j.state.Layout().Temp, "_github_workflow")
			if err := os.MkdirAll(dir, 0o755); err == nil {
				j.own(dir)
				p := filepath.Join(dir, "event.json")
				if err := os.WriteFile(p, []byte(ev), 0o644); err == nil {
					j.own(p)
					j.state.SetGitHub("event_path", p)
				}
			}
		}
	}
	j.mu.Unlock()
	if ferr != nil {
		return fail(ferr)
	}
	defer func() {
		// ProcessFiles runs after the handler, even when it failed (finally).
		j.fc.Process(s)
	}()

	// Inputs (ActionRunner.cs:174-185).
	script, err := j.evalString(st.Run, s.env)
	if err != nil {
		return fail(err)
	}
	shellInput, err := j.evalString(st.Shell, s.env)
	if err != nil {
		return fail(err)
	}
	var wdInput string
	var hasWD bool
	if st.WorkingDirectory != nil {
		hasWD = true
		if wdInput, err = j.evalString(st.WorkingDirectory, s.env); err != nil {
			return fail(err)
		}
	}

	// Header (ScriptHandler.PrintActionDetails, ScriptHandler.cs:27-137).
	j.out(num, "##[group]Run "+firstLine(script))
	for _, l := range strings.Split(strings.TrimRight(strings.ReplaceAll(script, "\r\n", "\n"), "\n"), "\n") {
		j.write(num, "\x1b[36;1m"+l+"\x1b[0m") // bright cyan
	}
	shell := shellInput
	if shell == "" {
		shell = j.runDefaults.shell
	}
	j.mu.Lock()
	lookup := Lookup{Path: j.lookupPath, Prepend: j.state.Prepend()}
	j.mu.Unlock()
	plan, err := PlanShell(shell, lookup)
	if err != nil {
		return fail(err) // the group stays open, like the original
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

	// Run (ScriptHandler.RunAsync, :140-358).
	wd := wdInput
	if !hasWD {
		wd = j.runDefaults.workingDirectory
	}
	j.mu.Lock()
	workspace := j.state.GitHub("workspace")
	temp := j.state.Layout().Temp
	j.mu.Unlock()
	dir := ResolveWorkingDirectory(workspace, wd)
	sc, err := WriteScript(temp, plan, script)
	if err != nil {
		return fail(err)
	}

	// Process environment: the inherited one, the step's environment, PATH with
	// GITHUB_PATH in front, then the runtime variables of the contexts.
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
	for _, k := range s.env.Keys() {
		v, _ := s.env.Get(k)
		penv.Set(k, v)
	}
	if len(prepend) > 0 {
		orig, ok := s.env.Get("PATH")
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
	var envList []string
	for _, k := range penv.Keys() {
		v, _ := penv.Get(k)
		envList = append(envList, k+"="+v)
	}

	res, err := runProcess(procSpec{
		Argv: sc.Argv, Dir: dir, Env: envList, Credential: j.cfg.Credential,
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
		return fail(errors.New(startError(plan.CommandPath, dir, err)))
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
