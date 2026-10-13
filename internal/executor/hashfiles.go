// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"bnema.dev/ermite-actions/expr"
)

// hashFiles is the function of the same name (Runner.Worker/Expressions/
// HashFilesFunction.cs). The official runner starts node on its hashFiles
// script (Misc/expressionFunc/hashFiles, which uses @actions/glob); this is a
// port of that script, evaluated by the executor, which runs in the sandbox
// next to the workspace.
//
// For each file that matches the patterns, in the order of @actions/glob's
// traversal (a depth first walk with the names of a directory sorted), the
// SHA-256 of the file is written to a second SHA-256; the result is its
// hexadecimal digest, or "" when no file matched.
//
// Differences from the official function, on purpose: the files are read
// through an os.Root on the workspace, so that a symbolic link that leaves the
// workspace is not followed (the executor can be root, the workspace belongs
// to the user of the steps) and its file is skipped; and only regular files
// are hashed (a FIFO would block until the timeout). A matched symbolic link
// whose target is missing fails the call, as it does in the script. Braces ({a,b}), extglobs (+(a|b)) and a leading
// "!" in the middle of a pattern are not special, as in @actions/glob.
const hashFilesTimeout = 120 * time.Second

func (j *job) hashFilesFunc() expr.Func {
	return func(ctx context.Context, args []expr.Value) (expr.Value, error) {
		var patterns []string
		follow := false
		for i, a := range args {
			p := expr.CoerceString(a)
			if i == 0 && strings.HasPrefix(p, "--") {
				if strings.EqualFold(p, "--follow-symbolic-links") {
					follow = true
					continue
				}
				// new ArgumentOutOfRangeException(message) takes the text as the parameter name.
				//nolint:staticcheck,misspell // GitHub's text, typo included
				return nil, fmt.Errorf("Specified argument was out of the range of valid values. (Parameter 'Invalid glob option %s, avaliable option: '--follow-symbolic-links'.')", p)
			}
			patterns = append(patterns, p)
		}
		ws := j.state.Layout().Workspace
		ctx, cancel := context.WithTimeout(ctx, hashFilesTimeout)
		defer cancel()
		sum, err := hashFilesIn(ctx, ws, j.state.Layout().Home, patterns, follow)
		desc := strings.ReplaceAll(strings.Join(patterns, ", "), "'", "''")
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			//nolint:staticcheck // GitHub's text, with its final period
			return nil, fmt.Errorf("hashFiles('%s') couldn't finish within %d seconds.", desc, int(hashFilesTimeout/time.Second))
		case err != nil:
			j.cfg.Log.Debug().Err(err).Msg("hashFiles failed")
			return nil, fmt.Errorf("hashFiles('%s') failed. Fail to hash files under directory '%s'", desc, ws)
		}
		return expr.String(sum), nil
	}
}

// ---------------------------------------------------------------- patterns

// seg is a segment of a pattern: its literal value (getLiteral, "" when it has
// a wildcard) and its minimatch form.
type seg struct {
	raw  string // the segment as written, for the "**" test
	lit  string
	mm   mmSeg
	root bool
}

// mmSeg is a parsed segment: the globstar, a literal, or a regular expression.
type mmSeg struct {
	globstar bool
	lit      string
	re       *regexp.Regexp
}

type globPattern struct {
	negate      bool
	segs        []seg
	trailingSep bool
	implicit    bool
	searchPath  string
}

const (
	kindNone = 0
	kindDir  = 1
	kindFile = 2
	kindAll  = 3
)

func splitSlash(s string) []string {
	return regexp.MustCompile(`/+`).Split(s, -1)
}

var errPattern = errors.New("invalid pattern")

// newGlobPattern is Pattern's constructor for a pattern written by the user,
// relative to cwd (the workspace, as segments).
func newGlobPattern(text, cwd, home string) (*globPattern, error) {
	p := &globPattern{}
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, "!") {
		p.negate = !p.negate
		text = strings.TrimSpace(text[1:])
	}
	if text == "" {
		return nil, fmt.Errorf("%w: pattern cannot be empty", errPattern)
	}
	norm := regexp.MustCompile(`//+`).ReplaceAllString(text, "/")
	p.trailingSep = strings.HasSuffix(norm, "/")
	trimmed := norm
	if trimmed != "/" {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	rooted := strings.HasPrefix(trimmed, "/")
	var rawSegs []string
	if rooted {
		rawSegs = append([]string{"/"}, splitNonEmpty(trimmed)...)
	} else {
		rawSegs = strings.Split(trimmed, "/")
	}
	for i, s := range rawSegs {
		l := getLiteral(s)
		if (l == "." && i != 0) || l == ".." {
			return nil, fmt.Errorf("%w: relative pathing '.' and '..' is not allowed: %s", errPattern, text)
		}
	}
	var prefix []seg
	rest := rawSegs
	switch {
	case trimmed == "." || strings.HasPrefix(trimmed, "./"):
		prefix, rest = literalSegs(cwd), rawSegs[1:]
	case trimmed == "~" || strings.HasPrefix(trimmed, "~/"):
		prefix, rest = literalSegs(home), rawSegs[1:]
	case rooted:
		rest = rawSegs[1:]
		prefix = []seg{{raw: "/", lit: "/", mm: mmSeg{lit: ""}, root: true}}
	default:
		prefix = literalSegs(cwd)
	}
	p.segs = prefix
	for _, r := range rest {
		p.segs = append(p.segs, seg{raw: r, lit: getLiteral(r), mm: parseMM(r)})
	}
	p.computeSearchPath()
	return p, nil
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, x := range strings.Split(s, "/") {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// literalSegs is the absolute path abs as literal segments.
func literalSegs(abs string) []seg {
	out := []seg{{raw: "/", lit: "/", mm: mmSeg{lit: ""}, root: true}}
	for _, s := range splitNonEmpty(abs) {
		out = append(out, seg{raw: "", lit: s, mm: mmSeg{lit: s}})
	}
	return out
}

func (p *globPattern) computeSearchPath() {
	var parts []string
	for _, s := range p.segs {
		if s.lit == "" {
			break
		}
		parts = append(parts, s.lit)
	}
	p.searchPath = joinSegs(parts)
}

func joinSegs(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, s := range parts[1:] {
		if !strings.HasSuffix(out, "/") {
			out += "/"
		}
		out += s
	}
	return out
}

func (p *globPattern) withDescendants() *globPattern {
	q := *p
	q.segs = append(append([]seg(nil), p.segs...), seg{raw: "**", lit: "", mm: mmSeg{globstar: true}})
	q.trailingSep = false
	q.implicit = true
	q.computeSearchPath()
	return &q
}

// getLiteral is Pattern.getLiteral: the segment with its escapes removed, or ""
// when it has a wildcard.
func getLiteral(s string) string {
	r := []rune(s)
	var lit []rune
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\' && i+1 < len(r):
			i++
			lit = append(lit, r[i])
		case c == '*' || c == '?':
			return ""
		case c == '[' && i+1 < len(r):
			var set []rune
			closed := -1
			for i2 := i + 1; i2 < len(r); i2++ {
				c2 := r[i2]
				if c2 == '\\' && i2+1 < len(r) {
					i2++
					set = append(set, r[i2])
					continue
				}
				if c2 == ']' {
					closed = i2
					break
				}
				set = append(set, c2)
			}
			if closed >= 0 {
				if len(set) > 1 {
					return ""
				}
				if len(set) == 1 {
					lit = append(lit, set...)
					i = closed
					continue
				}
			}
			lit = append(lit, c)
		default:
			lit = append(lit, c)
		}
	}
	return string(lit)
}

const reSpecials = "().*{}+?[]^$\\!"

// parseMM is minimatch's parse for one segment with the options of
// @actions/glob (dot, nobrace, noext, nonegate, nocomment).
func parseMM(s string) mmSeg {
	if s == "**" {
		return mmSeg{globstar: true}
	}
	if s == "" {
		return mmSeg{lit: ""}
	}
	re, magic := translateMM([]rune(s))
	if !magic {
		return mmSeg{lit: globUnescape(s)}
	}
	rx, err := regexp.Compile("^" + re + "$")
	if err != nil {
		return mmSeg{re: regexp.MustCompile(`$.`)}
	}
	return mmSeg{re: rx}
}

func globUnescape(s string) string {
	r := []rune(s)
	var out []rune
	for i := 0; i < len(r); i++ {
		if r[i] == '\\' && i+1 < len(r) {
			i++
		}
		out = append(out, r[i])
	}
	return string(out)
}

func translateMM(p []rune) (re string, magic bool) {
	var sb strings.Builder
	escaping, inClass := false, false
	classStart, reClassStart := -1, -1
	for i := 0; i < len(p); i++ {
		c := p[i]
		if escaping && strings.ContainsRune(reSpecials, c) {
			sb.WriteString(`\` + string(c))
			escaping = false
			continue
		}
		switch c {
		case '\\':
			escaping = true
		case '?', '*', '+', '@', '!':
			if inClass {
				if c == '!' && i == classStart+1 {
					c = '^'
				}
				sb.WriteRune(c)
				continue
			}
			switch c {
			case '*':
				sb.WriteString(`[^/]*?`)
				magic = true
			case '?':
				sb.WriteString(`[^/]`)
				magic = true
			default:
				sb.WriteString(`\` + string(c))
			}
		case '(':
			if inClass {
				sb.WriteRune('(')
			} else {
				sb.WriteString(`\(`)
			}
		case ')':
			sb.WriteString(`\)`)
		case '|':
			sb.WriteString(`\|`)
			escaping = false
		case '[':
			if inClass {
				sb.WriteString(`\[`)
				continue
			}
			inClass, classStart, reClassStart = true, i, sb.Len()
			sb.WriteRune('[')
		case ']':
			if i == classStart+1 || !inClass {
				sb.WriteString(`\]`)
				escaping = false
				continue
			}
			cs := string(p[classStart+1 : i])
			if _, err := regexp.Compile("[" + cs + "]"); err != nil {
				sub, m := translateMM([]rune(cs))
				cur := sb.String()
				sb.Reset()
				sb.WriteString(cur[:reClassStart] + `\[` + sub + `\]`)
				magic = magic || m
				inClass = false
				continue
			}
			magic = true
			inClass = false
			sb.WriteRune(']')
		default:
			if escaping {
				escaping = false
			} else if strings.ContainsRune(reSpecials, c) && (c != '^' || !inClass) {
				sb.WriteRune('\\')
			}
			sb.WriteRune(c)
		}
	}
	if inClass {
		cs := string(p[classStart+1:])
		sub, m := translateMM([]rune(cs))
		cur := sb.String()
		sb.Reset()
		sb.WriteString(cur[:reClassStart] + `\[` + sub)
		magic = magic || m
	}
	if escaping {
		sb.WriteString(`\\`)
	}
	return sb.String(), magic
}

// match is Pattern.match.
func (p *globPattern) match(item string) int {
	if p.segs[len(p.segs)-1].raw == "**" {
		item = regexp.MustCompile(`//+`).ReplaceAllString(item, "/")
		if !strings.HasSuffix(item, "/") && !p.implicit {
			item += "/"
		}
	} else {
		item = trimTrailingSep(item)
	}
	if p.matchOne(splitSlash(item), p.mmSegs(), 0, 0, false) {
		if p.trailingSep {
			return kindDir
		}
		return kindAll
	}
	return kindNone
}

func (p *globPattern) mmSegs() []mmSeg {
	out := make([]mmSeg, len(p.segs))
	for i, s := range p.segs {
		out[i] = s.mm
	}
	return out
}

// partialMatch is Pattern.partialMatch.
func (p *globPattern) partialMatch(item string) bool {
	item = trimTrailingSep(item)
	if path.Dir(item) == item {
		return true // the root matches the root of every pattern
	}
	return p.matchOne(splitSlash(item), p.mmSegs(), 0, 0, true)
}

func trimTrailingSep(s string) string {
	s = regexp.MustCompile(`//+`).ReplaceAllString(s, "/")
	if s != "/" && strings.HasSuffix(s, "/") {
		return s[:len(s)-1]
	}
	return s
}

func isDotName(s string) bool { return s == "." || s == ".." }

// matchOne is Minimatch.matchOne with the dot option set.
func (p *globPattern) matchOne(file []string, pat []mmSeg, fi, pi int, partial bool) bool {
	fl, pl := len(file), len(pat)
	for ; fi < fl && pi < pl; fi, pi = fi+1, pi+1 {
		ps, f := pat[pi], file[fi]
		if ps.globstar {
			fr, pr := fi, pi+1
			if pr == pl {
				for ; fi < fl; fi++ {
					if isDotName(file[fi]) {
						return false
					}
				}
				return true
			}
			for fr < fl {
				if p.matchOne(file, pat, fr, pr, partial) {
					return true
				}
				if isDotName(file[fr]) {
					break
				}
				fr++
			}
			if partial && fr == fl {
				return true
			}
			return false
		}
		var hit bool
		if ps.re != nil {
			hit = ps.re.MatchString(f)
		} else {
			hit = f == ps.lit
		}
		if !hit {
			return false
		}
	}
	switch {
	case fi == fl && pi == pl:
		return true
	case fi == fl:
		return partial
	default:
		return fi == fl-1 && file[fi] == ""
	}
}

// ---------------------------------------------------------------- walk

func matchAll(patterns []*globPattern, item string) int {
	r := kindNone
	for _, p := range patterns {
		if p.negate {
			r &^= p.match(item)
		} else {
			r |= p.match(item)
		}
	}
	return r
}

// getSearchPaths is internal-pattern-helper's getSearchPaths.
func getSearchPaths(patterns []*globPattern) []string {
	var pos []*globPattern
	for _, p := range patterns {
		if !p.negate {
			pos = append(pos, p)
		}
	}
	state := map[string]string{}
	for _, p := range pos {
		state[p.searchPath] = "candidate"
	}
	var out []string
	for _, p := range pos {
		if state[p.searchPath] == "included" {
			continue
		}
		found := false
		key := p.searchPath
		for parent := path.Dir(key); parent != key; parent = path.Dir(key) {
			if state[parent] != "" {
				found = true
				break
			}
			key = parent
		}
		if !found {
			out = append(out, p.searchPath)
			state[p.searchPath] = "included"
		}
	}
	return out
}

type searchItem struct {
	path  string
	level int
}

// hashFilesIn is the hashFiles script: the hex SHA-256 of the SHA-256 digests
// of the files under ws that match the patterns, "" when none does.
func hashFilesIn(ctx context.Context, ws, home string, lines []string, follow bool) (string, error) {
	cwd := path.Clean(ws)
	var patterns []*globPattern
	for _, line := range strings.Split(strings.Join(lines, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := newGlobPattern(line, cwd, home)
		if err != nil {
			return "", err
		}
		patterns = append(patterns, p)
	}
	all := make([]*globPattern, 0, 2*len(patterns))
	for _, p := range patterns {
		all = append(all, p)
		if p.trailingSep || p.segs[len(p.segs)-1].raw != "**" {
			all = append(all, p.withDescendants())
		}
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = root.Close() }()
	rel := func(abs string) (string, bool) {
		if abs == cwd {
			return ".", true
		}
		if strings.HasPrefix(abs, cwd+"/") {
			return abs[len(cwd)+1:], true
		}
		return "", false
	}

	var stack []searchItem
	for _, sp := range getSearchPaths(all) {
		if _, ok := rel(sp); !ok {
			if !strings.HasPrefix(cwd+"/", strings.TrimSuffix(sp, "/")+"/") {
				continue // neither under the workspace nor above it
			}
			sp = cwd
		}
		r, _ := rel(sp)
		if _, err := root.Lstat(r); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", err
		}
		stack = append([]searchItem{{sp, 1}}, stack...)
	}

	total := sha256.New()
	hashed := 0
	var chain []fs.FileInfo
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		match := matchAll(all, item.path)
		partial := match != kindNone
		if !partial {
			for _, p := range all {
				if !p.negate && p.partialMatch(item.path) {
					partial = true
					break
				}
			}
		}
		if !partial {
			continue
		}
		r, under := rel(item.path)
		if !under {
			// Above the workspace (an absolute pattern): only the way down matters.
			r = "."
			if item.path != "/" && !strings.HasPrefix(cwd+"/", item.path+"/") {
				continue
			}
		}
		var st fs.FileInfo
		if follow {
			st, err = root.Stat(r)
		} else {
			st, err = root.Lstat(r)
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || follow {
				continue // vanished, or a broken or escaping link
			}
			return "", err
		}
		if st.IsDir() && follow {
			for len(chain) >= item.level {
				chain = chain[:len(chain)-1]
			}
			cycle := false
			for _, c := range chain {
				if os.SameFile(c, st) {
					cycle = true
				}
			}
			if cycle {
				continue
			}
			chain = append(chain, st)
		}
		if st.IsDir() {
			if match&kindDir == 0 && !partial {
				continue
			}
			names, err := readDirNames(root, r)
			if err != nil {
				return "", err
			}
			base := item.path
			for i := len(names) - 1; i >= 0; i-- {
				stack = append(stack, searchItem{path.Join(base, names[i]), item.level + 1})
			}
			continue // directories are skipped by hashFiles
		}
		if match&kindFile == 0 || !under {
			continue
		}
		if !st.Mode().IsRegular() && !follow && st.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		sum, ok, err := hashOne(ctx, root, r)
		if err != nil {
			return "", err
		}
		if !ok {
			if st.Mode()&fs.ModeSymlink != 0 {
				if _, serr := root.Stat(r); errors.Is(serr, fs.ErrNotExist) {
					// The script stats the link's target and fails when it is
					// missing (fs.statSync throws ENOENT): so does the function.
					return "", fmt.Errorf("broken symbolic link %s: %w", item.path, serr)
				}
			}
			continue
		}
		total.Write(sum)
		hashed++
	}
	if hashed == 0 {
		return "", nil
	}
	return hex.EncodeToString(total.Sum(nil)), nil
}

func readDirNames(root *os.Root, rel string) ([]string, error) {
	d, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// hashOne is the SHA-256 digest of a regular file. ok is false for what is not
// hashed: a directory (reached through a link), a special file, a link that
// leaves the workspace or is broken.
func hashOne(ctx context.Context, root *os.Root, rel string) (sum []byte, ok bool, err error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, false, nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, &ctxReader{ctx, f}); err != nil {
		return nil, false, err
	}
	return h.Sum(nil), true, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
