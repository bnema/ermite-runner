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
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	// The two artifact files exist on GitHub-hosted runners (recordings: the
	// github context has artifacts and artifacts_list, the steps GITHUB_ARTIFACTS
	// and GITHUB_ARTIFACTS_LIST). ermite does not process them, see below.
	cmdArtifacts     = "artifacts"
	cmdArtifactsList = "artifacts_list"
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
	// CreateArtifactsFileCommand (CreateArtifactsFileCommand.cs:39) and
	// ArtifactsListFileCommand (ArtifactsListFileCommand.cs:37), registered in
	// ExtensionManager.cs:65-66. Both do nothing unless the feature flag
	// actions_runner_allow_artifacts_file or ACTIONS_RUNNER_ALLOW_ARTIFACTS_FILE
	// is on (CreateArtifactsFileCommand.cs:67-79); ermite has neither, so the
	// files are created empty (as the C# runner does when the feature is off)
	// and what a step writes to them is ignored. Artifact subjects are not
	// declared yet.
	{cmdArtifacts, "artifacts_"},
	{cmdArtifactsList, "artifacts_list_"},
}

// EnvVarName returns the environment variable that holds the file of a command:
// GITHUB_ARTIFACTS, GITHUB_ARTIFACTS_LIST, GITHUB_ENV, GITHUB_OUTPUT, GITHUB_PATH,
// GITHUB_STATE, GITHUB_STEP_SUMMARY
// (GitHubContext.GetRuntimeEnvironmentVariables, GitHubContext.cs:52).
func envVarName(contextName string) string { return "GITHUB_" + strings.ToUpper(contextName) }

// stepSummaryLimit is CreateStepSummaryCommand.AttachmentSizeLimit
// (FileCommandManager.cs:223).
const stepSummaryLimit = 1024 * 1024

// setEnvBlockList is SetEnvFileCommand._setEnvBlockList (FileCommandManager.cs:215).
var setEnvBlockList = []string{"NODE_OPTIONS"}

// msgError is an error whose text is GitHub's own message, capitalised and
// punctuated like the official runner's.
type msgError string

func (e msgError) Error() string { return string(e) }

func ghErrorf(format string, a ...any) error { return msgError(fmt.Sprintf(format, a...)) }

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
//
// The directory belongs to the user of the steps, and the executor reads the
// files as root after each step, so every access goes through an os.Root opened
// before the first step ran: replacing the directory or a file by a symbolic
// link cannot make the executor read or write anything outside it. Files are
// opened with O_NOFOLLOW and must be regular files.
type FileCommands struct {
	dir    string
	root   *os.Root
	suffix string
	// owner, when set, is the user the files are given to.
	owner *[2]int
}

// NewFileCommands uses dir (RUNNER_TEMP/_runner_file_commands), creating it.
// Call Close when the job is over.
func NewFileCommands(dir string) (*FileCommands, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &FileCommands{dir: dir, root: root}, nil
}

// Close releases the directory.
func (f *FileCommands) Close() error { return f.root.Close() }

// SetOwner gives the files of the next steps to uid:gid.
func (f *FileCommands) SetOwner(uid, gid int) { f.owner = &[2]int{uid, gid} }

func (f *FileCommands) name(c fileCommand, suffix string) string { return c.FilePrefix + suffix }

func (f *FileCommands) path(c fileCommand, suffix string) string {
	return filepath.Join(f.dir, f.name(c, suffix))
}

// Init creates fresh, empty files for the next step and returns the
// environment variables that name them (InitializeFiles,
// FileCommandManager.cs:42). The files of the previous step are kept: the
// runner makes a new FileCommandManager for every step (ActionRunner.cs:171),
// whose suffix starts empty, so it never deletes an old file (recorded:
// sum-env, "previous file still exists: yes").
func (f *FileCommands) Init() (map[string]string, error) {
	f.suffix = uuid.NewString()
	env := make(map[string]string, len(fileCommands))
	for _, c := range fileCommands {
		if err := createExcl(f.root, f.name(c, f.suffix), nil, 0o644, f.owner); err != nil {
			return nil, err
		}
		env[envVarName(c.ContextName)] = f.path(c, f.suffix)
	}
	return env, nil
}

// Process reads the files of the step that just ran and applies them to sink.
// A command that fails does not stop the others (ProcessFiles, :75).
func (f *FileCommands) Process(sink Sink) {
	for _, c := range fileCommands {
		p := f.name(c, f.suffix)
		var err error
		switch c.ContextName {
		case cmdPath:
			err = processAddPath(sink, f.root, p)
		case cmdEnv:
			err = processSetEnv(sink, f.root, p)
		case cmdStepSummary:
			err = processStepSummary(sink, f.root, p)
		case cmdState:
			err = processPairs(sink, f.root, p, sink.SaveState, func(k, v string) { sink.Debug(fmt.Sprintf("Save intra-action state %s = %s", k, v)) })
		case cmdArtifacts, cmdArtifactsList:
			// Disabled feature: nothing to process (see fileCommands).
		case cmdOutput:
			err = processPairs(sink, f.root, p, sink.SetOutput, func(k, v string) { sink.Debug(fmt.Sprintf("Set output %s = %s", k, v)) })
		}
		if err != nil {
			sink.Error(fmt.Sprintf("Unable to process file command '%s' successfully.", c.ContextName))
			sink.Error(err.Error())
			sink.CommandFailed()
		}
	}
}

// errNotRegular is the error of a file command file that was replaced by
// something else (a symbolic link, a FIFO, a directory).
var errNotRegular = ghErrorf("The file is not a regular file")

// openRegular opens name in root for reading without following a symbolic link
// and without blocking on a FIFO, and checks that it is a regular file.
func openRegular(root *os.Root, name string) (*os.File, error) {
	fh, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, errNotRegular
		}
		return nil, err
	}
	st, err := fh.Stat()
	if err != nil {
		_ = fh.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = fh.Close()
		return nil, errNotRegular
	}
	return fh, nil
}

// readText is File.ReadAllText: a UTF-8 BOM is dropped and invalid bytes
// become U+FFFD. A missing file gives ok=false and no error (the callers
// debug-log it; AddPath and StepSummary skip silently); a file that is not a
// regular file is an error.
func readText(root *os.Root, name string) (text string, ok bool, err error) {
	fh, err := openRegular(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = fh.Close() }()
	b, err := io.ReadAll(fh)
	if err != nil {
		return "", false, err
	}
	return decodeText(b), true, nil
}

// decodeText is the decoding of File.ReadAllText: a UTF-8 BOM is dropped and
// invalid bytes become U+FFFD.
func decodeText(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		b = bytes.ToValidUTF8(b, []byte("\ufffd"))
	}
	return string(b)
}

// createExcl creates name in root with the content, failing if it exists (an
// existing entry, a symbolic link included, is removed first, never followed),
// and gives it to owner when set. The creation uses O_EXCL|O_NOFOLLOW, so
// something planted between the removal and the creation makes it fail. The
// owner is set on the open file, not by name: the user of the steps owns the
// directory and could swap the name for something else between the close and a
// chown by name.
func createExcl(root *os.Root, name string, content []byte, perm os.FileMode, owner *[2]int) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fh, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := fh.Write(content); err != nil {
		_ = fh.Close()
		return err
	}
	if owner != nil {
		if err := fh.Chown(owner[0], owner[1]); err != nil {
			_ = fh.Close()
			return err
		}
	}
	return fh.Close()
}

// processAddPath is AddPathFileCommand.ProcessCommand (FileCommandManager.cs:134).
func processAddPath(sink Sink, root *os.Root, name string) error {
	text, ok, err := readText(root, name)
	if err != nil || !ok {
		return err
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
func processSetEnv(sink Sink, root *os.Root, name string) error {
	pairs, err := readEnvFile(sink, root, name)
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
func processPairs(sink Sink, root *os.Root, name string, apply func(k, v string), debug func(k, v string)) error {
	pairs, err := readEnvFile(sink, root, name)
	for _, p := range pairs {
		apply(p.Key, p.Value)
		debug(p.Key, p.Value)
	}
	return err
}

func readEnvFile(sink Sink, root *os.Root, name string) ([]Pair, error) {
	text, ok, err := readText(root, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		sink.Debug(fmt.Sprintf("File does not exist '%s'", filepath.Join(root.Name(), name)))
		return nil, nil
	}
	return ParseEnvFile(text)
}

// afterSummaryStat is called between the size check of the summary and its
// read; tests use it to make the file grow.
var afterSummaryStat func()

// processStepSummary is CreateStepSummaryCommand.ProcessCommand
// (FileCommandManager.cs:230-275): a missing or empty file is ignored, a file
// above 1 MiB is an error; otherwise the lines are masked one by one and the
// result goes to the sink when it keeps summaries. mask may be nil.
func processStepSummary(sink Sink, root *os.Root, name string) error {
	fh, err := openRegular(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	st, err := fh.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	tooLarge := func(size int64) {
		sink.Error(fmt.Sprintf("$GITHUB_STEP_SUMMARY upload aborted, supports content up to a size of %dk, got %dk. For more information see: https://docs.github.com/actions/using-workflows/workflow-commands-for-github-actions#adding-a-markdown-summary", stepSummaryLimit/1024, size/1024))
	}
	if st.Size() > stepSummaryLimit {
		tooLarge(st.Size())
		return nil
	}
	ss, ok := sink.(summarySink)
	if !ok {
		return nil
	}
	if afterSummaryStat != nil {
		afterSummaryStat()
	}
	// The file belongs to the user of the step, who can still write to it (a
	// background process of the step): what is read is bounded by the limit on
	// the descriptor that was checked, not by the size seen a moment ago.
	b, err := io.ReadAll(io.LimitReader(fh, stepSummaryLimit+1))
	if err != nil {
		return err
	}
	if len(b) > stepSummaryLimit {
		tooLarge(int64(len(b)))
		return nil
	}
	text := decodeText(b)
	var mask func(string) string
	if m, has := sink.(interface{ MaskText(string) string }); has {
		mask = m.MaskText
	} else {
		mask = func(s string) string { return s }
	}
	content := summaryContent(text, mask)
	// The size test of the runner is on the file (above). A file of exactly
	// 1 MiB without a final line feed gets one from WriteLine and would pass the
	// limit of the contract by that byte (recorded: sum-too-large, "exactly
	// 1 MiB" uploads on GitHub): drop that line feed.
	if len(content) == stepSummaryLimit+1 && st.Size() <= stepSummaryLimit && !strings.HasSuffix(text, "\n") {
		content = content[:stepSummaryLimit]
	}
	if len(content) > stepSummaryLimit {
		// Masking made it longer than the limit of the contract.
		tooLarge(int64(len(content)))
		return nil
	}
	ss.Summary(content)
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
					return out, ghErrorf("Invalid format '%s'. Name must not be empty", line)
				}
				key, output = k, v
			case heredoc >= 0 && (equals < 0 || heredoc < equals):
				k, delim, _ := strings.Cut(line, "<<")
				if k == "" || delim == "" {
					return out, ghErrorf("Invalid format '%s'. Name must not be empty and delimiter must not be empty", line)
				}
				key = k
				start, end := index, index
				temp, newline, tok := readLine(text, &index)
				for temp != delim || !tok {
					if !tok {
						return out, ghErrorf("Invalid value. Matching delimiter not found '%s'", delim)
					}
					if newline == "" {
						return out, ghErrorf("Invalid value. EOF marker missing new line.")
					}
					end = index - len(newline)
					temp, newline, tok = readLine(text, &index)
				}
				if end > start {
					output = text[start:end]
				}
			default:
				return out, ghErrorf("Invalid format '%s'", line)
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
