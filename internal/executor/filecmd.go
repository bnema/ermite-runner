// SPDX-License-Identifier: MIT

// Package executor runs the steps of a GitHub Actions job inside a sandbox.
// It is a port of the parts of the official runner (actions/runner, C#, MIT)
// that CI-1 needs: file commands, shells, contexts and the steps runner. The
// package does no sandboxing and no networking: it runs local processes and
// reports to a Reporter.
package executor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Names of the file commands (the GitHub context names) and their
// environment variables.
const (
	cmdPath        = "path"
	cmdEnv         = "env"
	cmdStepSummary = "step_summary"
	cmdState       = "state"
	cmdOutput      = "output"
)

// fileCommand describes one file command like the IFileCommandExtension of
// FileCommandManager.cs.
type fileCommand struct {
	ContextName string // github context name; the variable is GITHUB_<UPPER>
	FilePrefix  string
}

// fileCommands is in the order of the official runner's extension list, which
// is the order ProcessFiles runs them.
var fileCommands = []fileCommand{
	{cmdPath, "add_path_"},            // AddPathFileCommand (FileCommandManager.cs:127)
	{cmdEnv, "set_env_"},              // SetEnvFileCommand (:160)
	{cmdStepSummary, "step_summary_"}, // CreateStepSummaryCommand (:221)
	{cmdState, "save_state_"},         // SaveStateFileCommand (:295)
	{cmdOutput, "set_output_"},        // SetOutputFileCommand (:328)
}

// EnvVarName returns the environment variable that holds the file of a command:
// GITHUB_ENV, GITHUB_OUTPUT, GITHUB_PATH, GITHUB_STATE, GITHUB_STEP_SUMMARY
// (GitHubContext.GetRuntimeEnvironmentVariables, GitHubContext.cs:52).
func envVarName(contextName string) string { return "GITHUB_" + strings.ToUpper(contextName) }

// stepSummaryLimit is CreateStepSummaryCommand.AttachmentSizeLimit
// (FileCommandManager.cs:223).
const stepSummaryLimit = 1024 * 1024

// setEnvBlockList is SetEnvFileCommand._setEnvBlockList (FileCommandManager.cs:215).
var setEnvBlockList = []string{"NODE_OPTIONS"}

// Pair is one name and value read from an environment-style file.
type Pair struct{ Key, Value string }

// Sink receives the effects of the file commands of a step.
type Sink interface {
	// SetEnv sets an environment variable of the job (GITHUB_ENV).
	SetEnv(name, value string)
	// SetOutput sets an output of the step (GITHUB_OUTPUT).
	SetOutput(name, value string)
	// AddPath prepends a directory to PATH for the next steps (GITHUB_PATH). The
	// caller keeps the list like Global.PrependPath: the last added is first
	// in PATH, and a repeated entry moves to the end of the list.
	AddPath(path string)
	// SaveState keeps a value for the step's own post phase (GITHUB_STATE).
	SaveState(name, value string)
	// Error writes an error issue to the log (##[error]).
	Error(msg string)
	// Debug writes a ##[debug] line when step debugging is on.
	Debug(msg string)
	// CommandFailed marks the step as failed (context.CommandResult = Failed).
	CommandFailed()
}

// FileCommands manages the per-step files, like FileCommandManager.
type FileCommands struct {
	dir    string
	suffix string
}

// NewFileCommands uses dir (RUNNER_TEMP/_runner_file_commands), creating it.
func NewFileCommands(dir string) (*FileCommands, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileCommands{dir: dir}, nil
}

func (f *FileCommands) path(c fileCommand, suffix string) string {
	return filepath.Join(f.dir, c.FilePrefix+suffix)
}

// Init creates fresh, empty files for the next step (after deleting the ones
// of the previous step) and returns the environment variables that name them
// (InitializeFiles, FileCommandManager.cs:42).
func (f *FileCommands) Init() (map[string]string, error) {
	old := f.suffix
	f.suffix = uuid.NewString()
	env := make(map[string]string, len(fileCommands))
	for _, c := range fileCommands {
		if old != "" {
			_ = os.Remove(f.path(c, old))
		}
		p := f.path(c, f.suffix)
		_ = os.Remove(p)
		fh, err := os.Create(p)
		if err != nil {
			return nil, err
		}
		_ = fh.Close()
		env[envVarName(c.ContextName)] = p
	}
	return env, nil
}

// Process reads the files of the step that just ran and applies them to sink.
// A command that fails does not stop the others (ProcessFiles, :75).
func (f *FileCommands) Process(sink Sink) {
	for _, c := range fileCommands {
		p := f.path(c, f.suffix)
		var err error
		switch c.ContextName {
		case cmdPath:
			err = processAddPath(sink, p)
		case cmdEnv:
			err = processSetEnv(sink, p)
		case cmdStepSummary:
			err = processStepSummary(sink, p)
		case cmdState:
			err = processPairs(sink, p, sink.SaveState, func(k, v string) { sink.Debug(fmt.Sprintf("Save intra-action state %s = %s", k, v)) })
		case cmdOutput:
			err = processPairs(sink, p, sink.SetOutput, func(k, v string) { sink.Debug(fmt.Sprintf("Set output %s = %s", k, v)) })
		}
		if err != nil {
			sink.Error(fmt.Sprintf("Unable to process file command '%s' successfully.", c.ContextName))
			sink.Error(err.Error())
			sink.CommandFailed()
		}
	}
}

// readText is File.ReadAllText: a UTF-8 BOM is dropped and invalid bytes
// become U+FFFD. A missing file is reported as ok=false (the callers debug-log
// it; AddPath and StepSummary skip silently).
func readText(path string) (text string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		b = bytes.ToValidUTF8(b, []byte("\ufffd"))
	}
	return string(b), true
}

// processAddPath is AddPathFileCommand.ProcessCommand (FileCommandManager.cs:134).
func processAddPath(sink Sink, path string) error {
	text, ok := readText(path)
	if !ok {
		return nil
	}
	// File.ReadAllLines splits on \r\n, \n and \r.
	for _, line := range splitLines(text) {
		if line == "" {
			continue
		}
		sink.AddPath(line)
	}
	return nil
}

// splitLines is File.ReadAllLines / StreamReader.ReadLine: \r\n, \r and \n
// end a line; no empty last line is produced.
func splitLines(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexAny(s, "\r\n")
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i])
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
		}
		s = s[i+1:]
	}
	return out
}

// processSetEnv is SetEnvFileCommand.ProcessCommand (:167). Pairs read before
// a format error are applied, like the lazy enumeration of the original.
func processSetEnv(sink Sink, path string) error {
	pairs, err := readEnvFile(sink, path)
	for _, p := range pairs {
		if blocked := blockedEnvName(p.Key); blocked != "" {
			sink.Error(fmt.Sprintf("Can't store %s output parameter using '$GITHUB_ENV' command.", blocked))
			continue
		}
		sink.SetEnv(p.Key, p.Value)
		sink.Debug(fmt.Sprintf("%s='%s'", p.Key, p.Value))
	}
	return err
}

func blockedEnvName(name string) string {
	for _, b := range setEnvBlockList {
		if strings.EqualFold(b, name) {
			return b
		}
	}
	return ""
}

// processPairs serves GITHUB_STATE and GITHUB_OUTPUT.
func processPairs(sink Sink, path string, apply func(k, v string), debug func(k, v string)) error {
	pairs, err := readEnvFile(sink, path)
	for _, p := range pairs {
		apply(p.Key, p.Value)
		debug(p.Key, p.Value)
	}
	return err
}

func readEnvFile(sink Sink, path string) ([]Pair, error) {
	text, ok := readText(path)
	if !ok {
		sink.Debug(fmt.Sprintf("File does not exist '%s'", path))
		return nil, nil
	}
	return ParseEnvFile(text)
}

// processStepSummary is CreateStepSummaryCommand.ProcessCommand (:230) for
// CI-1: the file is only checked (an empty file is ignored, a file above 1 MiB
// is an error); ermite does not store summaries yet.
func processStepSummary(sink Sink, path string) error {
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return nil
	}
	if st.Size() > stepSummaryLimit {
		sink.Error(fmt.Sprintf("$GITHUB_STEP_SUMMARY upload aborted, supports content up to a size of %dk, got %dk. For more information see: https://docs.github.com/actions/using-workflows/workflow-commands-for-github-actions#adding-a-markdown-summary", stepSummaryLimit/1024, st.Size()/1024))
	}
	return nil
}

// ParseEnvFile parses the contents of GITHUB_ENV, GITHUB_OUTPUT or
// GITHUB_STATE: EnvFileKeyValuePairs.GetEnumerator (FileCommandManager.cs:364).
//
// Lines are NAME=VALUE (split at the first "=") or heredocs NAME<<DELIMITER up
// to a line equal to DELIMITER; the form whose marker comes first wins. Empty
// lines between entries are skipped. Only "\n" ends a line (on Linux). The
// pairs read before an error are returned with it, because the original
// applies them as it enumerates.
func ParseEnvFile(text string) ([]Pair, error) {
	var out []Pair
	index := 0
	line, _, ok := readLine(text, &index)
	for ok {
		if line != "" {
			var key, output string
			equals := strings.Index(line, "=")
			heredoc := strings.Index(line, "<<")
			switch {
			case equals >= 0 && (heredoc < 0 || equals < heredoc):
				// The original tests string.IsNullOrEmpty(line) here, which cannot
				// be true; the intent (the message says so) is an empty name, and
				// an empty variable name is useless: we reject it.
				k, v, _ := strings.Cut(line, "=")
				if k == "" {
					return out, fmt.Errorf("Invalid format '%s'. Name must not be empty", line)
				}
				key, output = k, v
			case heredoc >= 0 && (equals < 0 || heredoc < equals):
				k, delim, _ := strings.Cut(line, "<<")
				if k == "" || delim == "" {
					return out, fmt.Errorf("Invalid format '%s'. Name must not be empty and delimiter must not be empty", line)
				}
				key = k
				start, end := index, index
				temp, newline, tok := readLine(text, &index)
				for temp != delim || !tok {
					if !tok {
						return out, fmt.Errorf("Invalid value. Matching delimiter not found '%s'", delim)
					}
					if newline == "" {
						return out, errors.New("Invalid value. EOF marker missing new line.")
					}
					end = index - len(newline)
					temp, newline, tok = readLine(text, &index)
				}
				if end > start {
					output = text[start:end]
				}
			default:
				return out, fmt.Errorf("Invalid format '%s'", line)
			}
			out = append(out, Pair{key, output})
		}
		line, _, ok = readLine(text, &index)
	}
	return out, nil
}

// readLine is the private ReadLine of EnvFileKeyValuePairs (:462): it returns
// the line without its "\n", the newline ("\n" or "" at the end of the text)
// and false once the text is exhausted.
func readLine(text string, index *int) (line, newline string, ok bool) {
	if *index >= len(text) {
		return "", "", false
	}
	orig := *index
	lf := strings.IndexByte(text[orig:], '\n')
	if lf < 0 {
		*index = len(text)
		return text[orig:], "", true
	}
	*index = orig + lf + 1
	return text[orig : orig+lf], "\n", true
}

// PrependedPath returns the PATH value that results from the AddPath calls
// recorded in list (in call order) in front of orig, like
// Handler.AddPrependPathToEnvironment (Handler.cs:205) with PathUtil.PrependPath.
func PrependedPath(list []string, orig string) string {
	if len(list) == 0 {
		return orig
	}
	rev := slices.Clone(list)
	slices.Reverse(rev)
	prepend := strings.Join(rev, ":")
	if orig == "" {
		return prepend
	}
	if strings.HasPrefix(orig, prepend+":") {
		return orig
	}
	return prepend + ":" + orig
}
