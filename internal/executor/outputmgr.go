// SPDX-License-Identifier: MIT

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dlclark/regexp2"
)

// outputManager is OutputManager (Runner.Worker/Handlers/OutputManager.cs): it
// receives the lines a step prints, runs the workflow commands and the
// problem matchers on them, and logs what is left. One per process of a step.

const (
	// maxMatcherAttempts is OutputManager._maxAttempts.
	maxMatcherAttempts = 3
	// matcherTimeoutKey is OutputManager._timeoutKey.
	matcherTimeoutKey = "GITHUB_ACTIONS_RUNNER_ISSUE_MATCHER_TIMEOUT"
	// repoPathFailsafe is OutputManager._failsafe.
	repoPathFailsafe = 50
)

// colorCodeRegex is OutputManager._colorCodeRegex, @"\x0033\[[0-9;]*m?". In
// .NET "\x00" takes exactly two hex digits, so the pattern is NUL, "33", "["
// and not ESC: the runner strips nothing from real terminal colors (its own
// test writes "\033", which C# reads as NUL followed by "33"). Ported as is.
var colorCodeRegex = regexp2.MustCompile("\x0033\\[[0-9;]*m?", 0)

// colorCodePrefix is OutputManager._colorCodePrefix, "\033[": NUL, "33[".
const colorCodePrefix = "\x0033["

// parseTimeSpan is TimeSpan.TryParse with the invariant culture: "d" days,
// "hh:mm", "hh:mm:ss[.fffffff]" and "d.hh:mm:ss[.fffffff]", with an optional
// leading minus.
func parseTimeSpan(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var days int64
	parts := strings.Split(s, ":")
	if len(parts) == 1 {
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return 0, false
		}
		d := time.Duration(n) * 24 * time.Hour
		if neg {
			d = -d
		}
		return d, true
	}
	if len(parts) > 3 {
		return 0, false
	}
	if d, rest, ok := strings.Cut(parts[0], "."); ok {
		n, err := strconv.ParseInt(d, 10, 32)
		if err != nil {
			return 0, false
		}
		days, parts[0] = n, rest
	}
	num := func(x string) (int64, bool) {
		n, err := strconv.ParseInt(x, 10, 32)
		return n, err == nil && n >= 0
	}
	h, ok := num(parts[0])
	if !ok {
		return 0, false
	}
	m, ok := num(parts[1])
	if !ok || m > 59 {
		return 0, false
	}
	var sec float64
	if len(parts) == 3 {
		f, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || f < 0 || f >= 60 || strings.ContainsAny(parts[2], "eE+-") {
			return 0, false
		}
		sec = f
	}
	if h > 23 {
		return 0, false
	}
	d := time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec*float64(time.Second))
	if neg {
		d = -d
	}
	return d, true
}

// matcherTimeout is the constructor of OutputManager (:43-60): the variable of
// the job, then the environment of the runner, then one second. ermite has no
// job variables; the environment is the executor's own, as the runner's is its
// worker's.
func matcherTimeout(getenv func(string) string) time.Duration {
	if v := getenv(matcherTimeoutKey); v != "" {
		if d, ok := parseTimeSpan(v); ok && d > 0 {
			return d
		}
	}
	return time.Second
}

type outputManager struct {
	s *stepRun
	// cancel is closed when the step is cancelled or times out: the matchers
	// then stop (see stopped). Nil means never.
	cancel  <-chan struct{}
	root    *matcherRoot
	timeout time.Duration

	mu       sync.Mutex
	matchers []*issueMatcher
	dirMap   map[string]string // directory -> repository path ("" = none)
	hasDir   map[string]bool
}

// newOutputManager subscribes to the matchers of the job.
//
// cancel is the cancellation channel of the step (the one the process gets):
// once it is closed no matcher runs any more. The runner has no such rule, but
// its worker waits for the output of a step with the step's cancellation
// token, and a pile of lines against catastrophic patterns (1 s each, 3
// attempts, per matcher) would hold a step past its timeout for minutes.
func newOutputManager(s *stepRun, cancel <-chan struct{}) *outputManager {
	o := &outputManager{s: s, cancel: cancel, root: s.j.matchers, timeout: matcherTimeout(s.j.getenv), dirMap: map[string]string{}, hasDir: map[string]bool{}}
	o.mu.Lock()
	o.root.subscribe(o)
	for _, c := range o.root.snapshot() {
		if m, err := newIssueMatcher(c, o.timeout); err == nil {
			o.matchers = append(o.matchers, m)
		}
	}
	o.mu.Unlock()
	return o
}

// errStopped is the result of a match that was abandoned because the step was
// cancelled.
var errStopped = msgError("matching stopped: the step was cancelled")

// runMatch is m.match, abandoned when the step is cancelled while it runs: the
// engine cannot be interrupted, so it goes on in its goroutine until its own
// timeout, but the step does not wait for it and the matcher is not used again
// (stopped is true from then on).
func (o *outputManager) runMatch(m *issueMatcher, line string) (*issueMatch, error) {
	if o.cancel == nil {
		return m.match(line)
	}
	type result struct {
		m   *issueMatch
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := m.match(line)
		ch <- result{r, err}
	}()
	select {
	case r := <-ch:
		return r.m, r.err
	case <-o.cancel:
		return nil, errStopped
	}
}

// stopped reports whether the step was cancelled.
func (o *outputManager) stopped() bool {
	if o.cancel == nil {
		return false
	}
	select {
	case <-o.cancel:
		return true
	default:
		return false
	}
}

func (o *outputManager) dispose() { o.root.unsubscribe(o) }

// matcherChanged is OnMatcherChanged: the matcher goes first, the others of
// the same owner (ignoring case) go. A config without patterns only removes.
func (o *outputManager) matcherChanged(c *matcherConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var next []*issueMatcher
	if len(c.Patterns) > 0 {
		if m, err := newIssueMatcher(c, o.timeout); err == nil {
			next = append(next, m)
		}
	}
	for _, m := range o.matchers {
		if !strings.EqualFold(m.owner, c.Owner) {
			next = append(next, m)
		}
	}
	o.matchers = next
}

func (o *outputManager) removeMatcher(m *issueMatcher) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var next []*issueMatcher
	for _, x := range o.matchers {
		if x != m {
			next = append(next, x)
		}
	}
	o.matchers = next
}

// onData is OutputManager.OnDataReceived.
func (o *outputManager) onData(line string) {
	s := o.s
	if s.cmds.fatal != nil {
		return // the handler threw: ExecuteAsync is over, nothing more is read
	}
	// ## commands
	if line != "" && (strings.Contains(line, "##[") || strings.Contains(line, "::")) {
		if s.cmds.TryProcess(line) {
			return
		}
	}
	// Strip runner-controlled markers from user output to prevent injection.
	if strings.Contains(line, "##[start-action") || strings.Contains(line, "##[end-action") {
		line = strings.ReplaceAll(strings.ReplaceAll(line, "##[start-action", `##[\start-action`), "##[end-action", `##[\end-action`)
	}
	// Problem matchers.
	o.mu.Lock()
	matchers := append([]*issueMatcher(nil), o.matchers...)
	o.mu.Unlock()
	if len(matchers) > 0 && !o.stopped() {
		stripped := line
		if strings.Contains(line, colorCodePrefix) {
			stripped, _ = colorCodeRegex.Replace(line, "", -1, -1)
		}
		for _, m := range matchers {
			var match *issueMatch
			for attempt := 1; attempt <= maxMatcherAttempts; attempt++ {
				if o.stopped() {
					break // the lines that are left are only logged
				}
				mt, err := o.runMatch(m, stripped)
				if err == nil {
					match = mt
					break
				}
				if o.stopped() {
					break // a timeout that the cancellation caused counts for nothing
				}
				if attempt < maxMatcherAttempts {
					s.Debug(fmt.Sprintf("Timeout processing issue matcher '%s' against line '%s'. Exception: System.Text.RegularExpressions.RegexMatchTimeoutException: %s", m.owner, stripped, err.Error()))
				} else {
					s.j.addIssueFor(s, issue{Level: LevelWarning, Message: fmt.Sprintf("Removing issue matcher '%s'. Matcher failed %d times. Error: %s", m.owner, maxMatcherAttempts, err.Error())})
					o.removeMatcher(m)
				}
			}
			if match != nil {
				// Reset other matchers.
				for _, other := range matchers {
					if other != m {
						other.reset()
					}
				}
				if is, ok := o.convertToIssue(match); ok {
					is.LogText = stripped
					s.j.addIssueFor(s, is)
					return
				}
			}
		}
	}
	// Regular output.
	s.Out(line)
}

// convertToIssue is OutputManager.ConvertToIssue (:196-327).
func (o *outputManager) convertToIssue(m *issueMatch) (issue, bool) {
	s := o.s
	if strings.TrimFunc(m.message.s, unicode.IsSpace) == "" {
		s.Debug("Skipping logging an issue for the matched line because the message is empty.")
		return issue{}, false
	}
	var level AnnotationLevel
	switch strings.ToLower(m.severity.s) {
	case "", "error":
		level = LevelFailure
	case "warning":
		level = LevelWarning
	case "notice":
		level = LevelNotice
	default:
		s.Debug(fmt.Sprintf("Skipped logging an issue for the matched line because the severity '%s' is not supported.", m.severity.s))
		return issue{}, false
	}
	is := issue{Level: level, Message: m.message.s, Props: map[string]string{}}
	// unsigned parses like int.TryParse(NumberStyles.None): digits only.
	unsigned := func(v string) (string, bool) {
		for _, r := range v {
			if r < '0' || r > '9' {
				return "", false
			}
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || v == "" {
			return "", false
		}
		return strconv.FormatInt(n, 10), true
	}
	if m.line.s != "" {
		if n, ok := unsigned(m.line.s); ok {
			is.Props["line"] = n
		} else {
			s.Debug(fmt.Sprintf("Unable to parse line number '%s'", m.line.s))
		}
	}
	if m.column.s != "" {
		if n, ok := unsigned(m.column.s); ok {
			is.Props["col"] = n
		} else {
			s.Debug(fmt.Sprintf("Unable to parse column number '%s'", m.column.s))
		}
	}
	if c := strings.TrimFunc(m.code.s, unicode.IsSpace); c != "" {
		is.Props["code"] = c
	}
	// File.
	if strings.TrimFunc(m.file.s, unicode.IsSpace) != "" {
		file := m.file.s
		// Root using fromPath.
		if strings.TrimFunc(m.fromPath.s, unicode.IsSpace) != "" && !filepath.IsAbs(file) {
			if dir := filepath.Dir(m.fromPath.s); dir != "." && dir != "" {
				file = filepath.Join(dir, file)
			}
		}
		// Root using workspace; an absolute path is the container's and is
		// translated to the host's (OutputManager.cs:281-311).
		if !filepath.IsAbs(file) {
			file = filepath.Join(s.Workspace(), file)
		} else {
			file = filepath.Clean(file)
			file = filepath.Clean(s.HostPath(file))
		}
		file = filepath.Clean(file)
		if st, err := os.Stat(file); err == nil && !st.IsDir() {
			if repo := o.repositoryPath(file, 0); repo != "" {
				rel := strings.TrimLeft(file[len(repo):], "/")
				is.Props["file"] = rel
			} else {
				s.Debug(fmt.Sprintf("Dropping file value '%s'. Path is not under the workflow repo.", file))
			}
		} else {
			s.Debug(fmt.Sprintf("Dropping file value '%s'. Path does not exist", file))
		}
	}
	return is, true
}

// repositoryPath is OutputManager.GetRepositoryPath: the closest directory
// above filePath that has a .git/config naming the repository of the job.
func (o *outputManager) repositoryPath(filePath string, recursion int) string {
	if len(o.dirMap) > 100 {
		o.dirMap, o.hasDir = map[string]string{}, map[string]bool{}
	}
	dir := filepath.Dir(filePath)
	if dir == filePath || dir == "." || recursion > repoPathFailsafe {
		return ""
	}
	if o.hasDir[dir] {
		return o.dirMap[dir]
	}
	repo := ""
	gitConfig := filepath.Join(dir, ".git", "config")
	if st, err := os.Stat(gitConfig); err == nil && !st.IsDir() {
		serverURL := o.s.j.githubValue("server_url")
		if serverURL == "" {
			serverURL = "https://github.com"
		}
		host := serverURL
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if i := strings.IndexAny(host, "/:"); i >= 0 {
			host = host[:i]
		}
		nameWithOwner := o.s.j.githubValue("repository")
		patterns := []string{"url = " + serverURL + "/" + nameWithOwner, "url = git@" + host + ":" + nameWithOwner + ".git"}
		if b, err := os.ReadFile(gitConfig); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				l = strings.TrimFunc(l, unicode.IsSpace)
				for _, p := range patterns {
					if strings.EqualFold(l, p) {
						repo = dir
					}
				}
			}
		} else {
			o.s.Debug(fmt.Sprintf("Error when attempting to determine whether the path '%s' is under the workflow repository: %v", filePath, err))
		}
	} else {
		repo = o.repositoryPath(dir, recursion+1)
	}
	o.dirMap[dir], o.hasDir[dir] = repo, true
	return repo
}
