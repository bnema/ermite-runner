// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2"
)

// Problem matchers: a port of Runner.Worker/IssueMatcher.cs (actions/runner,
// MIT). The regular expressions are .NET's with RegexOptions.CultureInvariant |
// RegexOptions.ECMAScript (IssueMatcher.cs:450); regexp2 is the .NET engine in
// Go (RE2 has no lookarounds or backreferences, which real matchers use).

// optInt is an int? of the JSON files: a number, a numeric string or null.
type optInt struct {
	set bool
	v   int
}

func (o *optInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`)
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return ghErrorf("Error converting value %s to type 'System.Nullable`1[System.Int32]'.", string(b))
	}
	o.set, o.v = true, n
	return nil
}

// optBool is a bool of the JSON files: a boolean or the string "true"/"false".
type optBool bool

func (o *optBool) UnmarshalJSON(b []byte) error {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(string(b)), `"`)) {
	case "true":
		*o = true
	case "false", "null":
		*o = false
	default:
		return ghErrorf("Error converting value %s to type 'System.Boolean'.", string(b))
	}
	return nil
}

// patternConfig is IssuePatternConfig.
type patternConfig struct {
	File     optInt  `json:"file"`
	Line     optInt  `json:"line"`
	Column   optInt  `json:"column"`
	Severity optInt  `json:"severity"`
	Code     optInt  `json:"code"`
	Message  optInt  `json:"message"`
	FromPath optInt  `json:"fromPath"`
	Loop     optBool `json:"loop"`
	Regexp   string  `json:"regexp"`
}

// matcherConfig is IssueMatcherConfig.
type matcherConfig struct {
	Owner    string          `json:"owner"`
	Severity string          `json:"severity"`
	Patterns []patternConfig `json:"pattern"`
	FromPath string          `json:"fromPath"`
}

// matchersConfig is IssueMatchersConfig.
type matchersConfig struct {
	Matchers []*matcherConfig `json:"problemMatcher"`
}

// parseMatchersConfig reads the JSON of a matchers file.
func parseMatchersConfig(data []byte) (*matchersConfig, error) {
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	var c matchersConfig
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil // StringUtil.ConvertFromJson of an empty text is null
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// regexOptions is IssuePatternConfig.RegexOptions. regexp2 has no
// CultureInvariant: its matching is culture independent.
const regexOptions = regexp2.ECMAScript

func compilePattern(p string) (*regexp2.Regexp, error) {
	return regexp2.Compile(p, regexOptions)
}

// validate is IssueMatchersConfig.Validate (IssueMatcher.cs:262). The error
// texts are the runner's.
func (c *matchersConfig) validate() error {
	owners := map[string]bool{}
	for _, m := range c.Matchers {
		if m == nil {
			m = &matcherConfig{}
		}
		if err := m.validate(); err != nil {
			return err
		}
		lo := strings.ToLower(m.Owner)
		if owners[lo] {
			return ghErrorf("Duplicate owner name '%s'", m.Owner)
		}
		owners[lo] = true
	}
	return nil
}

// validate is IssueMatcherConfig.Validate (IssueMatcher.cs:376).
func (m *matcherConfig) validate() error {
	if m.Owner == "" {
		return ghErrorf("Owner must not be empty")
	}
	switch strings.ToUpper(m.Severity) {
	case "", "ERROR", "WARNING", "NOTICE":
	default:
		return ghErrorf("Matcher '%s' contains unexpected default severity '%s'", m.Owner, m.Severity)
	}
	if len(m.Patterns) == 0 {
		return ghErrorf("Matcher '%s' does not contain any patterns", m.Owner)
	}
	var file, line, column, severity, code, message, fromPath *int
	for i := range m.Patterns {
		isFirst, isLast := i == 0, i == len(m.Patterns)-1
		if err := m.Patterns[i].validate(isFirst, isLast, &file, &line, &column, &severity, &code, &message, &fromPath); err != nil {
			return err
		}
	}
	if message == nil {
		return ghErrorf("At least one pattern must set 'message'")
	}
	return nil
}

// validate is IssuePatternConfig.Validate (IssueMatcher.cs:495).
func (p *patternConfig) validate(isFirst, isLast bool, file, line, column, severity, code, message, fromPath **int) error {
	if bool(p.Loop) && (isFirst || !isLast) {
		return ghErrorf("Only the last pattern in a multiline matcher may set 'loop'")
	}
	if bool(p.Loop) && !p.Message.set {
		return ghErrorf("The loop pattern must set 'message'")
	}
	re, err := compilePattern(p.Regexp)
	if err != nil {
		return err
	}
	groupCount := len(re.GetGroupNumbers())
	for _, f := range []struct {
		name string
		v    optInt
		to   **int
	}{
		{"file", p.File, file}, {"line", p.Line, line}, {"column", p.Column, column},
		{"severity", p.Severity, severity}, {"code", p.Code, code}, {"message", p.Message, message},
		{"fromPath", p.FromPath, fromPath},
	} {
		if !f.v.set {
			continue
		}
		if *f.to != nil {
			return ghErrorf("The property '%s' is set twice", f.name)
		}
		if f.v.v < 0 || f.v.v >= groupCount {
			return ghErrorf("The property '%s' is set to %d which is out of range", f.name, f.v.v)
		}
		n := f.v.v
		*f.to = &n
	}
	return nil
}

// errMatchTimeout is the RegexMatchTimeoutException; its message is .NET's.
var errMatchTimeout = msgError("The Regex engine has timed out while trying to match a pattern to an input string. This can occur for many reasons, including very large inputs or excessive backtracking caused by nested quantifiers, back-references and other factors.")

// issuePattern is IssuePattern.
type issuePattern struct {
	file, line, column, severity, code, message, fromPath optInt
	loop                                                  bool
	re                                                    *regexp2.Regexp
}

// nstr is a string that can be null, as the values of IssueMatch are: a group
// that the pattern does not name is null, a group that matched nothing is "".
type nstr struct {
	s  string
	ok bool
}

func (n nstr) or(o nstr) nstr {
	if n.ok {
		return n
	}
	return o
}

// issueMatch is IssueMatch.
type issueMatch struct {
	file, line, column, severity, code, message, fromPath nstr
}

// issueMatcher is IssueMatcher: a matcher config with the state of the
// multi-line patterns.
type issueMatcher struct {
	owner, defaultSeverity, defaultFromPath string
	patterns                                []*issuePattern
	state                                   []*issueMatch
}

func newIssueMatcher(c *matcherConfig, timeout time.Duration) (*issueMatcher, error) {
	m := &issueMatcher{owner: c.Owner, defaultSeverity: c.Severity, defaultFromPath: c.FromPath}
	for _, pc := range c.Patterns {
		re, err := compilePattern(pc.Regexp)
		if err != nil {
			return nil, err
		}
		re.MatchTimeout = timeout
		m.patterns = append(m.patterns, &issuePattern{
			file: pc.File, line: pc.Line, column: pc.Column, severity: pc.Severity, code: pc.Code,
			message: pc.Message, fromPath: pc.FromPath, loop: bool(pc.Loop), re: re,
		})
	}
	m.reset()
	return m, nil
}

func (m *issueMatcher) reset() { m.state = make([]*issueMatch, max(len(m.patterns)-1, 0)) }

// groupValue is IssueMatch.GetValue: the text of a group, null when the
// pattern does not name one or its number is out of range.
func groupValue(match *regexp2.Match, idx optInt) nstr {
	if !idx.set || idx.v >= match.GroupCount() {
		return nstr{}
	}
	g := match.GroupByNumber(idx.v)
	if g == nil {
		return nstr{}
	}
	return nstr{g.String(), true}
}

func newIssueMatch(running *issueMatch, p *issuePattern, g *regexp2.Match, defSeverity, defFromPath string) *issueMatch {
	r := running
	if r == nil {
		r = &issueMatch{}
	}
	im := &issueMatch{
		file:     r.file.or(groupValue(g, p.file)),
		line:     r.line.or(groupValue(g, p.line)),
		column:   r.column.or(groupValue(g, p.column)),
		severity: r.severity.or(groupValue(g, p.severity)),
		code:     r.code.or(groupValue(g, p.code)),
		message:  r.message.or(groupValue(g, p.message)),
		fromPath: r.fromPath.or(groupValue(g, p.fromPath)),
	}
	if im.severity.s == "" && defSeverity != "" {
		im.severity = nstr{defSeverity, true}
	}
	if im.fromPath.s == "" && defFromPath != "" {
		im.fromPath = nstr{defFromPath, true}
	}
	return im
}

// match is IssueMatcher.Match. A timeout of the engine is returned as
// errMatchTimeout.
func (m *issueMatcher) match(line string) (*issueMatch, error) {
	find := func(p *issuePattern) (*regexp2.Match, error) {
		mt, err := p.re.FindStringMatch(line)
		if err != nil {
			return nil, errMatchTimeout
		}
		return mt, nil
	}
	if len(m.patterns) == 1 {
		p := m.patterns[0]
		mt, err := find(p)
		if err != nil || mt == nil {
			return nil, err
		}
		return newIssueMatch(nil, p, mt, m.defaultSeverity, m.defaultFromPath), nil
	}
	for i := len(m.patterns) - 1; i >= 0; i-- {
		var running *issueMatch
		if i > 0 {
			running = m.state[i-1]
		}
		if i != 0 && running == nil {
			continue
		}
		p := m.patterns[i]
		isLast := i == len(m.patterns)-1
		mt, err := find(p)
		if err != nil {
			return nil, err
		}
		if mt != nil {
			if isLast {
				if p.loop {
					m.reset()
					m.state[i-1] = running
				} else {
					m.reset()
				}
				return newIssueMatch(running, p, mt, m.defaultSeverity, m.defaultFromPath), nil
			}
			m.state[i] = newIssueMatch(running, p, mt, "", "")
		} else if isLast {
			m.state[i-1] = nil
		} else {
			m.state[i] = nil
		}
	}
	return nil, nil
}

// matcherRoot holds the matchers of the job (ExecutionContext._matchers, on the
// root context) and tells the output managers of the steps that run when they
// change (OnMatcherChanged).
type matcherRoot struct {
	mu       sync.Mutex
	configs  []*matcherConfig
	handlers map[*outputManager]struct{}
}

// snapshot returns the matchers now.
func (r *matcherRoot) snapshot() []*matcherConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*matcherConfig(nil), r.configs...)
}

func (r *matcherRoot) subscribe(o *outputManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[*outputManager]struct{}{}
	}
	r.handlers[o] = struct{}{}
}

func (r *matcherRoot) unsubscribe(o *outputManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handlers, o)
}

// add is ExecutionContext.AddMatchers (:1173): the new matchers go first, those
// of the same owner (ignoring case) are replaced. It returns the debug line.
func (r *matcherRoot) add(c *matchersConfig) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	owners := map[string]bool{}
	var next []*matcherConfig
	for _, m := range c.Matchers {
		owners[strings.ToLower(m.Owner)] = true
		next = append(next, m)
	}
	for _, m := range r.configs {
		if !owners[strings.ToLower(m.Owner)] {
			next = append(next, m)
		}
	}
	r.configs = next
	var names []string
	for _, m := range c.Matchers {
		names = append(names, "'"+m.Owner+"'")
		for o := range r.handlers {
			o.matcherChanged(m)
		}
	}
	return "Added matchers: " + strings.Join(names, ", ") + ". Problem matchers scan action output for known warning or error strings and report these inline."
}

// remove is ExecutionContext.RemoveMatchers (:1212). It returns the debug line.
func (r *matcherRoot) remove(owners []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	distinct := map[string]bool{}
	for _, o := range owners {
		distinct[strings.ToLower(o)] = true
	}
	var kept, removed []*matcherConfig
	for _, m := range r.configs {
		if distinct[strings.ToLower(m.Owner)] {
			removed = append(removed, m)
		} else {
			kept = append(kept, m)
		}
	}
	r.configs = kept
	var names []string
	for _, m := range removed {
		names = append(names, "'"+m.Owner+"'")
		for o := range r.handlers {
			o.matcherChanged(&matcherConfig{Owner: m.Owner})
		}
	}
	return "Removed matchers: " + strings.Join(names, ", ")
}
