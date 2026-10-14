// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Annotations: a port of the issue handling of the official runner,
// ExecutionContext.AddIssue (ExecutionContext.cs:796) and
// IssueCommandExtension (ActionCommandManager.cs:660-770).

const (
	// maxCountPerIssueType is ExecutionContext._maxCountPerIssueType.
	maxCountPerIssueType = 10
	// Limits of the contract (runner.proto, Annotation): the server refuses a
	// whole call that exceeds them, so the executor keeps within them.
	maxAnnotationTitle = 255
	maxAnnotationFile  = 1024
)

// issue is what AddIssue receives (DTWebApi.Issue and the log options).
type issue struct {
	Level   AnnotationLevel
	Message string
	// Props are the Data of the issue: title, file, line, endline, col,
	// endcolumn (keys in lower case), as the commands and matchers set them.
	Props map[string]string
	// LogText replaces the message in the log (ExecutionContextLogOptions
	// .LogMessageOverride, set by problem matchers).
	LogText string
}

func tagOf(l AnnotationLevel) string {
	switch l {
	case LevelFailure:
		return "##[error]"
	case LevelWarning:
		return "##[warning]"
	}
	return "##[notice]"
}

func levelOfTag(tag string) (AnnotationLevel, bool) {
	switch tag {
	case "##[error]":
		return LevelFailure, true
	case "##[warning]":
		return LevelWarning, true
	case "##[notice]":
		return LevelNotice, true
	}
	return "", false
}

// issueCollector keeps the annotations of one step of the workflow, which is
// what the server stores them under. The steps inside a composite action
// (embedded contexts) add to the collector of the step that uses the action:
// like the runner, their issues are held back and only counted against the
// limit of ten per level when the outer step completes
// (ExecutionContext.Complete, :528-540).
type issueCollector struct {
	mu       sync.Mutex
	own      []Annotation
	counts   map[AnnotationLevel]int
	embedded []Annotation
}

func newIssueCollector() *issueCollector {
	return &issueCollector{counts: map[AnnotationLevel]int{}}
}

func (c *issueCollector) add(a Annotation, embedded bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if embedded {
		c.embedded = append(c.embedded, a)
		return
	}
	c.addLocked(a)
}

func (c *issueCollector) addLocked(a Annotation) {
	if c.counts[a.Level] < maxCountPerIssueType {
		c.counts[a.Level]++
		c.own = append(c.own, a)
	}
}

// result returns the annotations of the step: its own, then those of the
// embedded steps within what is left of the limits.
func (c *issueCollector) result() []Annotation {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.embedded {
		c.addLocked(a)
	}
	c.embedded = nil
	return append([]Annotation(nil), c.own...)
}

// collector returns the collector of a step of the workflow.
func (j *job) collector(num int) *issueCollector {
	j.issuesMu.Lock()
	defer j.issuesMu.Unlock()
	if j.collectors == nil {
		j.collectors = map[int]*issueCollector{}
	}
	c := j.collectors[num]
	if c == nil {
		c = newIssueCollector()
		j.collectors[num] = c
	}
	return c
}

// truncateUTF16 cuts s to n UTF-16 code units, the unit of .NET's
// Substring(0, n), without splitting a character.
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return s[:k]
		}
		i++
	}
	return s
}

// addIssue is ExecutionContext.AddIssue: the message is masked and cut to
// 4096 characters, the issue is kept for the annotations of the step (at most
// ten per level) and written to the log.
func (j *job) addIssue(num int, c *issueCollector, embedded bool, is issue) {
	msg := j.mask(is.Message)
	msg = truncateUTF16(msg, maxIssueMessageLength)
	if c == nil {
		c = j.collector(num)
	}
	if msg != "" {
		c.add(j.annotationOf(is, msg), embedded)
	}
	logMsg := msg
	if is.LogText != "" {
		logMsg = j.mask(is.LogText)
	}
	if logMsg != "" { // an empty message is not logged (:855)
		j.logTagged(num, tagOf(is.Level), logMsg)
	}
}

// logTagged writes tag+message; only the first line carries the tag.
func (j *job) logTagged(num int, tag, msg string) {
	lines := strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n")
	j.write(num, tag+lines[0])
	for _, l := range lines[1:] {
		j.write(num, l)
	}
}

// parseInt is int.TryParse(string, out int): white space around, an optional
// sign, digits, within 32 bits.
func parseInt(s string) (int, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	return int(n), err == nil
}

// annotationOf converts an issue to the Annotation of the contract. The
// runner puts the properties in Issue.Data and lets the service build the
// check annotation; negative or non numeric numbers cannot be sent, and are
// left out. The title and the file are masked too (the runner masks only the
// message; on GitHub a secret in a title stays visible, recorded in
// mask-add-mask, which ermite deliberately does not reproduce).
//
// Deliberate divergence: GitHub keeps negative and zero lines (int.TryParse,
// ActionCommandManager.cs:723-725; ann-levels records start_line -1); ermite's
// contract is positive lines, 0 = no line, and the host clamps.
func (j *job) annotationOf(is issue, msg string) Annotation {
	a := Annotation{Level: is.Level, Message: msg}
	p := is.Props
	a.Title = truncateRunes(j.mask(p["title"]), maxAnnotationTitle)
	if f := j.mask(p["file"]); utf8.RuneCountInString(f) <= maxAnnotationFile {
		a.File = f
	}
	num := func(k string) int32 {
		if n, ok := parseInt(p[k]); ok && n > 0 {
			return int32(n)
		}
		return 0
	}
	a.StartLine, a.EndLine = num("line"), num("endline")
	a.StartColumn, a.EndColumn = num("col"), num("endcolumn")
	if a.EndLine != 0 && a.EndLine < a.StartLine {
		a.EndLine = 0
	}
	// GitHub's annotations end where they start when the command gave no end
	// (recorded: ann-levels, match-basic).
	if a.StartLine != 0 && a.EndLine == 0 {
		a.EndLine = a.StartLine
	}
	if a.StartColumn != 0 && a.EndColumn == 0 {
		a.EndColumn = a.StartColumn
	}
	return a
}

// validateLinesAndColumns is IssueCommandExtension.ValidateLinesAndColumns
// (ActionCommandManager.cs:731-785). props has lower-case keys; command is the
// name in the debug messages.
func validateLinesAndColumns(props map[string]string, command string, debug func(string)) {
	line, endLine, column, endColumn := props["line"], props["endline"], props["col"], props["endcolumn"]
	lineNumber, hasStartLine := parseInt(line)
	endLineNumber, hasEndLine := parseInt(endLine)
	columnNumber, hasStartColumn := parseInt(column)
	endColumnNumber, hasEndColumn := parseInt(endColumn)
	hasColumn := hasStartColumn || hasEndColumn
	msg := func(f string, a ...any) {
		debug(fmt.Sprintf("Invalid %s command value. ", command) + fmt.Sprintf(f, a...))
	}

	if hasEndLine && !hasStartLine {
		msg("'endLine' can only be set if 'line' is provided")
		props["line"] = endLine
		hasStartLine = true
		line = endLine
		lineNumber = endLineNumber
	}
	if hasEndColumn && !hasStartColumn {
		msg("'endColumn' can only be set if 'col' is provided")
		props["col"] = endColumn
		hasStartColumn = true
		column = endColumn
		columnNumber = endColumnNumber
	}
	if !hasStartLine && hasColumn {
		msg("'col' and 'endColumn' can only be set if 'line' value is provided.")
		delete(props, "col")
		delete(props, "endcolumn")
	}
	if hasEndLine && line != endLine && hasColumn {
		msg("'col' and 'endColumn' cannot be set if 'line' and 'endLine' are different values.")
		delete(props, "col")
		delete(props, "endcolumn")
	}
	if hasStartLine && hasEndLine && endLineNumber < lineNumber {
		msg("'endLine' cannot be less than 'line'.")
		delete(props, "line")
		delete(props, "endline")
	}
	if hasStartColumn && hasEndColumn && endColumnNumber < columnNumber {
		msg("'endColumn' cannot be less than 'col'.")
		delete(props, "col")
		delete(props, "endcolumn")
	}
	_ = column
}

// makeRelative is IOUtil.MakeRelative on Linux: path relative to folder when
// folder is a prefix of it at a directory boundary, else path unchanged.
func makeRelative(path, folder string) string {
	if !strings.HasPrefix(path, folder) {
		return path
	}
	if len(path) == len(folder) {
		return ""
	}
	if folder != "" && folder[len(folder)-1] == '/' {
		return path[len(folder):]
	}
	if path[len(folder)] == '/' {
		return path[len(folder)+1:]
	}
	return path
}

// commandIssue is IssueCommandExtension.ProcessCommand.
func (m *commandManager) commandIssue(level AnnotationLevel, name string, c *actionCommand) {
	h := m.host
	props := map[string]string{}
	for k, v := range c.Props {
		props[k] = v
	}
	validateLinesAndColumns(props, name, h.Debug)
	if file := props["file"]; file != "" {
		// ActionCommandManager.cs:675-681: the path of a container is the host's.
		if hp := h.HostPath(file); hp != file {
			file = hp
			props["file"] = hp
		}
		if rel := makeRelative(file, h.Workspace()); rel != file && rel != "" {
			props["file"] = rel
		}
	}
	h.AddIssue(issue{Level: level, Message: c.Data, Props: props})
}
