// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Shells and script files of `run:` steps: a port of ScriptHandlerHelpers.cs
// and of the parts of ScriptHandler.cs that build the command line
// (Runner.Worker/Handlers). Only the Linux behaviour is ported.

// defaultArguments is ScriptHandlerHelpers._defaultArguments
// (ScriptHandlerHelpers.cs:13). Keys are compared case-insensitively.
var defaultArguments = map[string]string{
	"cmd":        `/D /E:ON /V:OFF /S /C "CALL "{0}""`,
	"pwsh":       `-command ". '{0}'"`,
	"powershell": `-command ". '{0}'"`,
	"bash":       "--noprofile --norc -e -o pipefail {0}",
	"sh":         "-e {0}",
	"python":     "{0}",
}

// scriptExtensions is ScriptHandlerHelpers._extensions (:23).
var scriptExtensions = map[string]string{
	"cmd":        ".cmd",
	"pwsh":       ".ps1",
	"powershell": ".ps1",
	"bash":       ".sh",
	"sh":         ".sh",
	"python":     ".py",
}

// ScriptArgumentsFormat is GetScriptArgumentsFormat (:33): the argument format
// of a built-in shell, "" for any other command.
func ScriptArgumentsFormat(shellType string) string {
	return defaultArguments[strings.ToLower(shellType)]
}

// ScriptFileExtension is GetScriptFileExtension (:42): ".sh", ".py", ... or "".
func ScriptFileExtension(shellType string) string {
	return scriptExtensions[strings.ToLower(shellType)]
}

// FixUpScriptContents is ScriptHandlerHelpers.FixUpScriptContents (:51). The
// switch is case-sensitive, unlike the two lookups above.
func FixUpScriptContents(shellType, contents string) string {
	switch shellType {
	case "cmd":
		// Use @echo off rather than /Q: with /Q, echo can't be turned on.
		return "@echo off\n" + contents
	case "powershell", "pwsh":
		return "$ErrorActionPreference = 'stop'\n" + contents + "\n" +
			`if ((Test-Path -LiteralPath variable:\LASTEXITCODE)) { exit $LASTEXITCODE }`
	}
	return contents
}

// ParseShellOption is ParseShellOptionString (:70): "COMMAND [..ARGS]" split at
// the first space.
func ParseShellOption(shell string) (command, args string) {
	command, args, _ = strings.Cut(shell, " ")
	return command, args
}

// ErrInvalidShell is the error of a shell whose argument format has no {0}
// (ScriptHandler.cs:245).
var ErrInvalidShell error = msgError("Invalid shell option. Shell must be a valid built-in (bash, sh, cmd, powershell, pwsh) or a format string containing '{0}'")

// Lookup finds executables like WhichUtil.Which (Runner.Sdk/Util/WhichUtil.cs):
// in PATH with the directories added by GITHUB_PATH in front.
type Lookup struct {
	// Path is the PATH of the executor process (not of the step).
	Path string
	// Prepend is Global.PrependPath in the order of the AddPath calls.
	Prepend []string
}

// NotFoundError is the FileNotFoundException of Which(require: true).
type NotFoundError struct{ Command string }

func (e *NotFoundError) Error() string { return e.Command + ": command not found" }

// Which returns the path of command, or "" when it is not found.
func (l Lookup) Which(command string) string {
	if filepath.IsAbs(command) {
		if isFile(command) {
			return command
		}
	}
	path := PrependedPath(l.Prepend, l.Path)
	for _, seg := range strings.Split(path, ":") {
		if seg == "" {
			continue
		}
		seg = os.ExpandEnv(seg)
		// Directory.EnumerateFiles(seg, command): a plain file name in the directory.
		if strings.ContainsRune(command, '/') {
			continue
		}
		p := filepath.Join(seg, command)
		if isFile(p) {
			return p
		}
	}
	return ""
}

// isFile reports whether p is a file, or a symbolic link whose target exists
// (WhichUtil.IsPathValid).
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Require is Which(command, require: true).
func (l Lookup) Require(command string) (string, error) {
	if p := l.Which(command); p != "" {
		return p, nil
	}
	return "", &NotFoundError{command}
}

// ShellPlan is how a step's script is run: the shell chosen by the step, the
// job defaults, or the runner's default.
type ShellPlan struct {
	// Command is the shell command: "sh" for the default shell (whose path is
	// bash when found), else the first word of the shell option.
	Command string
	// CommandPath is the executable that is started, and shown on the
	// "shell:" line of the log.
	CommandPath string
	// ArgFormat is the argument format with {0} for the script path.
	ArgFormat string
}

// PlanShell chooses the shell like ScriptHandler.RunAsync (:170-234) and
// PrintActionDetails (:65-117). shell is the step's `shell` or, when empty,
// defaults.run.shell (the caller does that merge, :171-180).
//
// With no shell: bash if it is in PATH, else sh, with the arguments of "sh"
// (-e {0}). "sh" is only the name used for the arguments and the extension.
func PlanShell(shell string, l Lookup) (ShellPlan, error) {
	if shell == "" {
		p, err := l.lookupBashOrSh()
		if err != nil {
			return ShellPlan{}, err
		}
		return checkFormat(ShellPlan{Command: "sh", CommandPath: p, ArgFormat: ScriptArgumentsFormat("sh")})
	}
	cmd, args := ParseShellOption(shell)
	p, err := l.Require(cmd)
	if err != nil {
		return ShellPlan{}, err
	}
	format := strings.TrimLeft(args, " \t\n\v\f\r")
	if format == "" {
		format = ScriptArgumentsFormat(cmd)
	}
	return checkFormat(ShellPlan{Command: cmd, CommandPath: p, ArgFormat: format})
}

func (l Lookup) lookupBashOrSh() (string, error) {
	if p := l.Which("bash"); p != "" {
		return p, nil
	}
	return l.Require("sh")
}

func checkFormat(p ShellPlan) (ShellPlan, error) {
	if p.ArgFormat == "" || !strings.Contains(p.ArgFormat, "{0}") {
		return p, ErrInvalidShell
	}
	return p, nil
}

// ShellLine is the text after "shell: " in the step header (:121-126).
func (p ShellPlan) ShellLine() string { return p.CommandPath + " " + p.ArgFormat }

// Extension is the extension of the script file, from the shell command.
func (p ShellPlan) Extension() string { return ScriptFileExtension(p.Command) }

// FormatArguments is string.Format(argFormat, path) of .NET with one argument:
// {0} is the path, {{ and }} are braces; anything else is a format error. The
// script path has its double quotes escaped first (ScriptHandler.cs:252).
func FormatArguments(format, scriptPath string) (string, error) {
	path := strings.ReplaceAll(scriptPath, `"`, `\"`)
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		switch c := format[i]; {
		case c == '{' && i+1 < len(format) && format[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(format) && format[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(format[i:], '}')
			if end < 0 {
				return "", ghErrorf("Input string was not in a correct format.")
			}
			spec := format[i+1 : i+end]
			index, _, _ := strings.Cut(spec, ",")
			index, _, _ = strings.Cut(index, ":")
			if strings.TrimSpace(index) != "0" {
				return "", ghErrorf("Index (zero based) must be greater than or equal to zero and less than the size of the argument list.")
			}
			b.WriteString(path)
			i += end
		case c == '}':
			return "", ghErrorf("Input string was not in a correct format.")
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// ParseArguments splits an argument string into arguments the way .NET does
// for Process.Start on Unix (Process.Unix.cs ParseArgumentsIntoList): spaces
// and tabs separate; double quotes group; backslashes only count before a
// quote.
func ParseArguments(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i == len(s) {
			break
		}
		var cur strings.Builder
		inQuotes := false
		for i < len(s) {
			n := 0
			for i < len(s) && s[i] == '\\' {
				i++
				n++
			}
			if n > 0 {
				if i >= len(s) || s[i] != '"' {
					cur.WriteString(strings.Repeat(`\`, n))
				} else {
					cur.WriteString(strings.Repeat(`\`, n/2))
					if n%2 != 0 {
						cur.WriteByte('"')
						i++
					}
				}
				continue
			}
			c := s[i]
			if c == '"' {
				if inQuotes && i < len(s)-1 && s[i+1] == '"' {
					cur.WriteByte('"')
					i++
				} else {
					inQuotes = !inQuotes
				}
				i++
				continue
			}
			if (c == ' ' || c == '\t') && !inQuotes {
				break
			}
			cur.WriteByte(c)
			i++
		}
		out = append(out, cur.String())
	}
	return out
}

// Script is a script written to the temp directory, ready to run.
type Script struct {
	// Path is the script file.
	Path string
	// Argv is the command: the shell and its arguments with the script path.
	Argv []string
}

// WriteScript writes the script of a step to the directory of root (RUNNER_TEMP)
// under a random name with the extension of the shell, and builds the command
// line (ScriptHandler.RunAsync :247-285). The file has no BOM and is not
// executable: the shell reads it. The directory belongs to the user of the steps,
// so the file is created through root, exclusively and without following links
// (see createExcl).
func WriteScript(root *os.Root, p ShellPlan, contents string) (Script, error) {
	return WriteScriptFor(root, p, contents, nil)
}

// WriteScriptFor is WriteScript for a shell that sees the file under another
// path: tr translates the host path for the arguments (a job container sees
// RUNNER_TEMP as /__w/_temp, ScriptHandler.cs:252). Script.Path stays the
// host's.
func WriteScriptFor(root *os.Root, p ShellPlan, contents string, tr func(string) string) (Script, error) {
	name := uuid.NewString() + p.Extension()
	path := filepath.Join(root.Name(), name)
	shown := path
	if tr != nil {
		shown = tr(path)
	}
	args, err := FormatArguments(p.ArgFormat, shown)
	if err != nil {
		return Script{}, err
	}
	contents = FixUpScriptContents(p.Command, contents)
	if err := createExcl(root, name, []byte(contents), 0o644, nil); err != nil {
		return Script{}, fmt.Errorf("write script: %w", err)
	}
	return Script{Path: path, Argv: append([]string{p.CommandPath}, ParseArguments(args)...)}, nil
}

// ResolveWorkingDirectory is Path.Combine(workspace, workingDirectory ?? "")
// (ScriptHandler.cs:168): an absolute directory replaces the workspace.
func ResolveWorkingDirectory(workspace, dir string) string {
	if filepath.IsAbs(dir) {
		return dir
	}
	if dir == "" {
		return workspace
	}
	return strings.TrimRight(workspace, "/") + "/" + dir
}

// PlanShellInContainer is the shell of a step that runs in the job container
// (ScriptHandler.cs:73-117, :182-232 with isContainerStepHost): nothing is
// looked up, the shell is found by the container itself, so the default is
// always "sh -e {0}" even when the image has bash.
func PlanShellInContainer(shell string) (ShellPlan, error) {
	if shell == "" {
		return checkFormat(ShellPlan{Command: "sh", CommandPath: "sh", ArgFormat: ScriptArgumentsFormat("sh")})
	}
	cmd, args := ParseShellOption(shell)
	format := strings.TrimLeft(args, " \t\n\v\f\r")
	if format == "" {
		format = ScriptArgumentsFormat(cmd)
	}
	return checkFormat(ShellPlan{Command: cmd, CommandPath: cmd, ArgFormat: format})
}
