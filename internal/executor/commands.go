// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"strings"
	"unicode"
)

// Workflow commands in the output of a step: a port of ActionCommand.cs
// (Runner.Common) and of the parts of ActionCommandManager.cs that CI-1 needs.
// Masking (add-mask) and problem matchers are CI-2; they are announced in the
// log and otherwise ignored.

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
// (the names of the extensions at ActionCommandManager.cs). add-mask,
// add-matcher and remove-matcher are recognised and reported as not supported
// yet.
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
}

// commandManager is ActionCommandManager: one per step.
type commandManager struct {
	host       commandHost
	registered map[string]bool
	stopped    bool
	stopToken  string
	echo       bool
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
	if input == "" {
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
			m.fail(input, false, err)
			return true
		}
		m.stopToken, m.stopped = cmd.Data, true
		m.registered[strings.ToLower(cmd.Data)] = true
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
		return fmt.Errorf("%s", unsupportedStopTokenDisabled)
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
	case "warning", "error", "notice":
		// IssueCommandExtension: the annotation is logged; ermite does not store
		// annotations yet (CI-2), so file, line and title are dropped.
		h.Tagged("##["+name+"]", c.Data)
	case "echo":
		if c.Data == "" {
			return fmt.Errorf("Value cannot be null or empty. (Parameter 'value')")
		}
		switch strings.ToUpper(strings.TrimSpace(c.Data)) {
		case "ON":
			m.echo = true
			h.Debug("Setting echo command value to 'on'")
		case "OFF":
			m.echo = false
			h.Debug("Setting echo command value to 'off'")
		default:
			return fmt.Errorf("Invalid echo command value. Possible values can be: 'on', 'off'. Current value is: '%s'.", c.Data)
		}
	case "set-output":
		h.Tagged("##[warning]", fmt.Sprintf(unsupportedCommandMessage, name))
		n := c.prop("name")
		if n == "" {
			return fmt.Errorf("Required field 'name' is missing in ##[set-output] command.")
		}
		h.SetOutput(n, c.Data)
		h.Debug(fmt.Sprintf("steps.<id>.outputs.%s='%s'", n, c.Data))
	case "save-state":
		h.Tagged("##[warning]", fmt.Sprintf(unsupportedCommandMessage, name))
		n := c.prop("name")
		if n == "" {
			return fmt.Errorf("Required field 'name' is missing in ##[save-state] command.")
		}
		h.SaveState(n, c.Data)
		h.Debug(fmt.Sprintf("Save intra-action state %s = %s", n, c.Data))
	case "set-env":
		if !h.EnvFlag("ACTIONS_ALLOW_UNSECURE_COMMANDS") {
			return fmt.Errorf(unsupportedCommandMessageDisabled, name)
		}
		n := c.prop("name")
		if n == "" {
			return fmt.Errorf("Required field 'name' is missing in ##[set-env] command.")
		}
		if b := blockedEnvName(n); b != "" {
			h.Tagged("##[error]", fmt.Sprintf("Can't update %s environment variable using ::set-env:: command.", b))
			return nil
		}
		h.SetEnv(n, c.Data)
		h.Debug(fmt.Sprintf("%s='%s'", n, c.Data))
	case "add-path":
		if !h.EnvFlag("ACTIONS_ALLOW_UNSECURE_COMMANDS") {
			return fmt.Errorf(unsupportedCommandMessageDisabled, name)
		}
		if c.Data == "" {
			return fmt.Errorf("Value cannot be null or empty. (Parameter 'path')")
		}
		h.AddPath(c.Data)
	case "add-mask", "add-matcher", "remove-matcher":
		// Not supported yet (CI-2). The value of add-mask is not echoed.
		h.Tagged("##[warning]", fmt.Sprintf("The `%s` command is not supported yet by this runner.", name))
	}
	return nil
}
