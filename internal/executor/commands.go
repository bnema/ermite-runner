// SPDX-License-Identifier: MIT

package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Workflow commands in the output of a step: a port of ActionCommand.cs
// (Runner.Common) and of ActionCommandManager.cs.

type escapeMapping struct{ token, replacement string }

// _escapeMappings, _escapeDataMappings, _escapePropertyMappings of
// ActionCommand.cs:11-35, in order: the replacements run one after the other.
var (
	escapeMappings = []escapeMapping{{";", "%3B"}, {"\r", "%0D"}, {"\n", "%0A"}, {"]", "%5D"}, {"%", "%25"}}
	escapeData     = []escapeMapping{{"\r", "%0D"}, {"\n", "%0A"}, {"%", "%25"}}
	escapeProperty = []escapeMapping{{"\r", "%0D"}, {"\n", "%0A"}, {":", "%3A"}, {",", "%2C"}, {"%", "%25"}}
)

func unescape(s string, maps []escapeMapping) string {
	for _, m := range maps {
		s = strings.ReplaceAll(s, m.replacement, m.token)
	}
	return s
}

// actionCommand is a parsed workflow command.
type actionCommand struct {
	Name  string
	Props map[string]string // keys compared case-insensitively (lower-cased)
	Data  string
}

func (c *actionCommand) prop(name string) string { return c.Props[strings.ToLower(name)] }

const commandKey = "::"

// parseCommandV2 is ActionCommand.TryParseV2 (ActionCommand.cs:57): "::name
// key=value,key=value::data".
func parseCommandV2(message string, registered func(string) bool) (*actionCommand, bool) {
	// .NET TrimStart() trims Unicode white space.
	message = strings.TrimLeftFunc(message, unicode.IsSpace)
	if !strings.HasPrefix(message, commandKey) {
		return nil, false
	}
	end := strings.Index(message[len(commandKey):], commandKey)
	if end < 0 {
		return nil, false
	}
	end += len(commandKey)
	info := message[len(commandKey):end]
	name, rest, hasSpace := strings.Cut(info, " ")
	if !registered(name) {
		return nil, false
	}
	cmd := &actionCommand{Name: name, Props: map[string]string{}}
	if hasSpace && len(info) > len(name) { // spaceIndex > 0
		for _, p := range strings.Split(strings.TrimSpace(rest), ",") {
			if p == "" {
				continue
			}
			k, v, ok := strings.Cut(p, "=")
			if ok && k != "" && v != "" { // Split(RemoveEmptyEntries): both parts non-empty
				cmd.Props[strings.ToLower(k)] = unescape(v, escapeProperty)
			}
		}
	}
	cmd.Data = unescape(message[end+len(commandKey):], escapeData)
	return cmd, true
}

// parseCommandV1 is ActionCommand.TryParse (ActionCommand.cs:127): the legacy
// "##[name key=value;key=value]data".
func parseCommandV1(message string, registered func(string) bool) (*actionCommand, bool) {
	const prefix = "##["
	pi := strings.Index(message, prefix)
	if pi < 0 {
		return nil, false
	}
	rb := strings.IndexByte(message[pi:], ']')
	if rb < 0 {
		return nil, false
	}
	rb += pi
	info := message[pi+len(prefix) : rb]
	name, rest, hasSpace := strings.Cut(info, " ")
	if !registered(name) {
		return nil, false
	}
	cmd := &actionCommand{Name: name, Props: map[string]string{}}
	if hasSpace && len(info) > len(name) {
		for _, p := range strings.Split(rest, ";") {
			if p == "" {
				continue
			}
			k, v, ok := strings.Cut(p, "=")
			if ok && k != "" && v != "" {
				cmd.Props[strings.ToLower(k)] = unescape(v, escapeMappings)
			}
		}
	}
	cmd.Data = unescape(message[rb+1:], escapeMappings)
	return cmd, true
}

// Messages of Constants.cs:234-236.
const (
	unsupportedCommandMessage         = "The `%s` command is deprecated and will be disabled soon. Please upgrade to using Environment Files. For more information see: https://github.blog/changelog/2022-10-11-github-actions-deprecating-save-state-and-set-output-commands/"
	unsupportedCommandMessageDisabled = "The `%s` command is disabled. Please upgrade to using Environment Files or opt into unsecure command execution by setting the `ACTIONS_ALLOW_UNSECURE_COMMANDS` environment variable to `true`. For more information see: https://github.blog/changelog/2020-10-01-github-actions-deprecating-set-env-and-add-path-commands/"
	unsupportedStopTokenDisabled      = "You cannot use a endToken that is an empty string, the string 'pause-logging', or another workflow command. For more information see: https://docs.github.com/actions/learn-github-actions/workflow-commands-for-github-actions#example-stopping-and-starting-workflow-commands or opt into insecure command execution by setting the `ACTIONS_ALLOW_UNSECURE_STOPCOMMAND_TOKENS` environment variable to `true`."
)

// commandNames are the commands the executor registers, in ActionCommandManager
// (the names of the extensions at ActionCommandManager.cs).
var commandNames = []string{
	"set-env", "set-output", "save-state", "add-mask", "add-path", "add-matcher", "remove-matcher",
	"debug", "warning", "error", "notice", "group", "endgroup", "echo", "stop-commands",
}

// commandHost is what commands act on: the step that is running.
type commandHost interface {
	// Out writes a log line (several when it contains line feeds).
	Out(text string)
	// Tagged writes a ##[tag]message line.
	Tagged(tag, message string)
	Debug(msg string)
	SetOutput(name, value string)
	SaveState(name, value string)
	SetEnv(name, value string)
	AddPath(path string)
	CommandFailed()
	// EnvFlag reads a variable of the env context, then of the process.
	EnvFlag(name string) bool
	// AddIssue is ExecutionContext.AddIssue: it logs the issue and keeps it for
	// the annotations of the step.
	AddIssue(issue)
	// Workspace is github.workspace.
	Workspace() string
	// HostPath translates a path printed by a process in the job container
	// back to the host's (ContainerInfo.TranslateToHostPath); without a
	// container it is the identity.
	HostPath(p string) string
	// AddMask is SecretMasker.AddValue.
	AddMask(value string)
	// Matchers is the root of the problem matchers of the job.
	Matchers() *matcherRoot
	// ReadMatcherFile reads a problem matcher file (IOUtil.LoadObject).
	ReadMatcherFile(path string) ([]byte, error)
}

// commandManager is ActionCommandManager: one per step.
type commandManager struct {
	host       commandHost
	registered map[string]bool
	stopped    bool
	stopToken  string
	echo       bool
	// fatal is the exception that ActionCommandManager.ValidateStopToken throws
	// outside the try block of TryProcessCommand: it leaves OutputDataReceived,
	// ProcessInvoker.ExecuteAsync and the step with it, so the step fails with
	// the message alone and the output that follows is never read. The
	// official runner leaves the process running (ProcessInvoker is only
	// disposed; "Complete job" kills it as an orphan); ermite stops it through
	// the cancellation path (SIGINT, SIGTERM, SIGKILL) so that the step does not
	// wait for it, and discards what it still writes.
	fatal error
	// onFatal, when set, is called once when fatal is set: the process that
	// writes the output is then stopped (runProc).
	onFatal func()
}

func newCommandManager(h commandHost, echo bool) *commandManager {
	m := &commandManager{host: h, registered: map[string]bool{}, echo: echo}
	for _, n := range commandNames {
		m.registered[strings.ToLower(n)] = true
	}
	return m
}

func (m *commandManager) isRegistered(name string) bool { return m.registered[strings.ToLower(name)] }

// TryProcess handles one line of output. It returns true when the line was a
// command and must not be logged as plain text (ActionCommandManager.cs:61).
func (m *commandManager) TryProcess(input string) bool {
	if input == "" || m.fatal != nil {
		return false
	}
	cmd, ok := parseCommandV2(input, m.isRegistered)
	if !ok {
		cmd, ok = parseCommandV1(input, m.isRegistered)
	}
	if !ok {
		return false
	}
	name := strings.ToLower(cmd.Name)
	if m.stopped {
		if m.stopToken != "" && strings.EqualFold(cmd.Name, m.stopToken) {
			m.host.Out(input)
			m.host.Debug("Resume processing commands")
			delete(m.registered, strings.ToLower(m.stopToken))
			m.stopped, m.stopToken = false, ""
			return true
		}
		m.host.Debug(fmt.Sprintf("Process commands has been stopped and waiting for '##[%s]' to resume.", m.stopToken))
		return false
	}
	if name == "stop-commands" {
		if err := m.validateStopToken(cmd.Data); err != nil {
			m.fatal = err
			if m.onFatal != nil {
				m.onFatal()
			}
			return true
		}
		m.stopToken, m.stopped = cmd.Data, true
		m.registered[strings.ToLower(cmd.Data)] = true
		if len(m.stopToken) > 6 { // ActionCommandManager.cs:117
			m.host.AddMask(m.stopToken)
		}
		m.host.Out(input)
		m.host.Debug("Paused processing commands until the token you called ::stopCommands:: with is received")
		return true
	}
	omitEcho := name == "debug" || name == "warning" || name == "error" || name == "notice" || name == "add-mask"
	if m.echo && !omitEcho {
		m.host.Out(input)
	}
	if err := m.run(name, cmd); err != nil {
		m.fail(input, omitEcho, err)
	}
	return true
}

func (m *commandManager) fail(input string, omitEcho bool, err error) {
	info := input
	if omitEcho {
		info = ""
	}
	m.host.Tagged("##[error]", fmt.Sprintf("Unable to process command '%s' successfully.", info))
	m.host.Tagged("##[error]", err.Error())
	m.host.CommandFailed()
}

func (m *commandManager) validateStopToken(token string) error {
	invalid := m.isRegistered(token) || token == "" || strings.EqualFold(token, "pause-logging")
	if invalid && !m.host.EnvFlag("ACTIONS_ALLOW_UNSECURE_STOPCOMMAND_TOKENS") {
		return msgError(unsupportedStopTokenDisabled)
	}
	return nil
}

func (m *commandManager) run(name string, c *actionCommand) error {
	h := m.host
	switch name {
	case "group":
		h.Out("##[group]" + c.Data) // GroupingCommandExtension, ActionCommandManager.cs:802
	case "endgroup":
		h.Out("##[endgroup]")
	case "debug":
		h.Debug(c.Data)
	case "warning":
		m.commandIssue(LevelWarning, name, c)
	case "error":
		m.commandIssue(LevelFailure, name, c)
	case "notice":
		m.commandIssue(LevelNotice, name, c)
	case "echo":
		if c.Data == "" {
			// ArgUtil.NotNullOrEmpty throws ArgumentNullException.
			return ghErrorf("Value cannot be null. (Parameter 'value')")
		}
		switch strings.ToUpper(strings.TrimSpace(c.Data)) {
		case "ON":
			m.echo = true
			h.Debug("Setting echo command value to 'on'")
		case "OFF":
			m.echo = false
			h.Debug("Setting echo command value to 'off'")
		default:
			return ghErrorf("Invalid echo command value. Possible values can be: 'on', 'off'. Current value is: '%s'.", c.Data)
		}
	case "set-output":
		h.Tagged("##[warning]", fmt.Sprintf(unsupportedCommandMessage, name))
		n := c.prop("name")
		if n == "" {
			return ghErrorf("Required field 'name' is missing in ##[set-output] command.")
		}
		h.SetOutput(n, c.Data)
		h.Debug(fmt.Sprintf("steps.<id>.outputs.%s='%s'", n, c.Data))
	case "save-state":
		h.Tagged("##[warning]", fmt.Sprintf(unsupportedCommandMessage, name))
		n := c.prop("name")
		if n == "" {
			return ghErrorf("Required field 'name' is missing in ##[save-state] command.")
		}
		h.SaveState(n, c.Data)
		h.Debug(fmt.Sprintf("Save intra-action state %s = %s", n, c.Data))
	case "set-env":
		if !h.EnvFlag("ACTIONS_ALLOW_UNSECURE_COMMANDS") {
			return ghErrorf(unsupportedCommandMessageDisabled, name)
		}
		n := c.prop("name")
		if n == "" {
			return ghErrorf("Required field 'name' is missing in ##[set-env] command.")
		}
		if b := blockedEnvName(n); b != "" {
			h.Tagged("##[error]", fmt.Sprintf("Can't update %s environment variable using ::set-env:: command.", b))
			return nil
		}
		h.SetEnv(n, c.Data)
		h.Debug(fmt.Sprintf("%s='%s'", n, c.Data))
	case "add-path":
		if !h.EnvFlag("ACTIONS_ALLOW_UNSECURE_COMMANDS") {
			return ghErrorf(unsupportedCommandMessageDisabled, name)
		}
		if c.Data == "" {
			return ghErrorf("Value cannot be null. (Parameter 'path')")
		}
		h.AddPath(c.Data)
	case "add-mask":
		m.addMask(c)
	case "add-matcher":
		return m.addMatcher(c)
	case "remove-matcher":
		return m.removeMatcher(c)
	}
	return nil
}

// warn is IExecutionContext.Warning.
func (m *commandManager) warn(msg string) { m.host.AddIssue(issue{Level: LevelWarning, Message: msg}) }

// addMask is AddMaskCommandExtension (ActionCommandManager.cs:402-424).
func (m *commandManager) addMask(c *actionCommand) {
	if strings.TrimFunc(c.Data, unicode.IsSpace) == "" {
		m.warn("Can't add secret mask for empty string in ##[add-mask] command.")
		return
	}
	if m.echo {
		m.host.Out("::add-mask::***")
	}
	m.host.AddMask(c.Data)
	// Also add each individual line. Typically individual lines are processed
	// from STDOUT of child processes.
	for _, item := range strings.FieldsFunc(c.Data, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if item = strings.TrimFunc(item, unicode.IsSpace); item != "" {
			m.host.AddMask(item)
		}
	}
}

// matcherPath roots a path of add-matcher and remove-matcher in the workspace.
func (m *commandManager) matcherPath(file string) string {
	file = m.host.HostPath(file) // ActionCommandManager.cs:513-517, :575-580
	if !filepath.IsAbs(file) {
		return filepath.Join(m.host.Workspace(), file)
	}
	return file
}

// loadMatchers is IOUtil.LoadObject<IssueMatchersConfig>.
func (m *commandManager) loadMatchers(file string) (*matchersConfig, error) {
	b, err := m.host.ReadMatcherFile(file)
	if err != nil {
		return nil, dotnetFileError(file, err)
	}
	return parseMatchersConfig(b)
}

// dotnetFileError gives the error of a file that cannot be read the text of
// the .NET exception (FileNotFoundException, DirectoryNotFoundException).
func dotnetFileError(file string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if st, e := os.Stat(filepath.Dir(file)); e != nil || !st.IsDir() {
			return ghErrorf("Could not find a part of the path '%s'.", file)
		}
		return ghErrorf("Could not find file '%s'.", file)
	case errors.Is(err, fs.ErrPermission):
		return ghErrorf("Access to the path '%s' is denied.", file)
	}
	return err
}

// addMatcher is AddMatcherCommandExtension (ActionCommandManager.cs:496-536).
func (m *commandManager) addMatcher(c *actionCommand) error {
	if c.Data == "" {
		m.warn("File path must be specified.")
		return nil
	}
	cfg, err := m.loadMatchers(m.matcherPath(c.Data))
	if err != nil {
		return err
	}
	if cfg != nil && len(cfg.Matchers) > 0 {
		if err := cfg.validate(); err != nil {
			return err
		}
		m.host.Debug(m.host.Matchers().add(cfg))
	}
	return nil
}

// removeMatcher is RemoveMatcherCommandExtension (ActionCommandManager.cs:538-607).
func (m *commandManager) removeMatcher(c *actionCommand) error {
	owner, file := c.prop("owner"), c.Data
	if owner != "" && file != "" {
		m.warn("Either specify an owner name or a file path in ##[remove-matcher] command. Both values cannot be set.")
		return nil
	}
	if owner == "" && file == "" {
		m.warn("Either an owner name or a file path must be specified in ##[remove-matcher] command.")
		return nil
	}
	if owner != "" {
		m.host.Debug(m.host.Matchers().remove([]string{owner}))
		return nil
	}
	cfg, err := m.loadMatchers(m.matcherPath(file))
	if err != nil {
		return err
	}
	if cfg != nil && len(cfg.Matchers) > 0 {
		var owners []string
		for _, mc := range cfg.Matchers {
			owners = append(owners, mc.Owner)
		}
		m.host.Debug(m.host.Matchers().remove(owners))
	}
	return nil
}
