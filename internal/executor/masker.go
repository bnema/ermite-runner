// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// masker replaces secret values by *** in text that leaves the executor. It is
// a port of GitHub.DistributedTask.Logging.SecretMasker (Sdk/DTLogging/Logging/
// SecretMasker.cs) with the value encoders HostContext registers
// (Runner.Common/HostContext.cs:103-113, Sdk/DTLogging/Logging/ValueEncoders.cs).
// A value is masked as is and in the forms the encoders give: base64 and its
// two shifts, the command line, expression, JSON, URI, XML escapes, the value
// without its double quotes and the sections around "&" of PowerShell.
//
// Values can be added while the job runs (::add-mask::). Masking finds every
// occurrence of every value (overlaps included), merges the ranges that
// overlap or touch and replaces each merged range by one ***, like
// SecretMasker.MaskSecrets.
//
// Bytes that follow the secret change the last, incomplete group of its base64
// (`echo "$T" | base64` appends a newline), so each base64 form is also
// registered without that group: only the characters the secret alone
// determines. actions/runner only registers the full forms; the truncated ones
// are an addition of ermite.
type masker struct {
	mu sync.RWMutex
	// originals are the values passed to add (SecretMasker.m_originalValueSecrets);
	// values are the forms to look for: the originals and their encodings. A
	// value that is the encoding of an earlier one is still a new original: its
	// own encodings must be registered (AddValue tests the originals only).
	originals map[string]struct{}
	values    map[string]struct{}
	list      []string
}

const maskedValue = "***"

// minBase64Tail is the shortest base64 form, in characters, registered once its
// incomplete last group is removed: a shorter one (3 bytes of the secret)
// would mask ordinary text.
const minBase64Tail = 8

func newMasker(secrets ...string) *masker {
	m := &masker{originals: map[string]struct{}{}, values: map[string]struct{}{}}
	for _, s := range secrets {
		m.add(s)
	}
	return m
}

// add is SecretMasker.AddValue: the value and its encodings. An empty value is
// ignored.
func (m *masker) add(value string) {
	if m == nil || value == "" {
		return
	}
	m.mu.RLock()
	_, known := m.originals[value]
	m.mu.RUnlock()
	if known {
		return
	}
	forms := append([]string{value}, secretEncodings(value)...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.originals[value] = struct{}{}
	for _, f := range forms {
		if f == "" {
			continue
		}
		if _, ok := m.values[f]; !ok {
			m.values[f] = struct{}{}
			m.list = append(m.list, f)
		}
	}
}

// utf16Len is the length of s in .NET string units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// secretEncodings are the other forms of v that the runner masks, in the order
// of HostContext.cs:103-113.
func secretEncodings(v string) []string {
	var out []string
	b := []byte(v)
	// Base64StringEscape, ...Shift1, ...Shift2 (ValueEncoders.cs:17-52, 150-163).
	for shift := range 3 {
		sub := b
		if len(b) > shift {
			sub = b[shift:]
		}
		out = append(out, base64.StdEncoding.EncodeToString(sub))
		// ermite: whole groups only, 3 bytes give 4 characters that no later
		// byte changes.
		if n := len(sub) / 3 * 4; n >= minBase64Tail {
			out = append(out, base64.StdEncoding.EncodeToString(sub[:len(sub)/3*3]))
		}
	}
	out = append(out,
		strings.ReplaceAll(v, `"`, `\"`), // CommandLineArgumentEscape
		strings.ReplaceAll(v, "'", "''"), // ExpressionStringEscape
		jsonStringEscape(v),
		uriDataEscape(v),
		xmlDataEscape(v),
		trimDoubleQuotes(v),
		powerShellPreAmpersandEscape(v),
		powerShellPostAmpersandEscape(v),
	)
	return out
}

// jsonStringEscape is ValueEncoders.JsonStringEscape: JsonConvert.ToString
// without the quotes. Newtonsoft escapes the quote, the backslash, the control
// characters below U+0020 and U+0085, U+2028, U+2029; it leaves the rest of
// Unicode, "/" and "<>&'" alone.
func jsonStringEscape(s string) string {
	var sb strings.Builder
	const hex = "0123456789abcdef"
	for _, r := range s {
		switch {
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\b':
			sb.WriteString(`\b`)
		case r == '\f':
			sb.WriteString(`\f`)
		case r < 0x20 || r == 0x85 || r == 0x2028 || r == 0x2029:
			sb.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				sb.WriteByte(hex[(r>>shift)&15])
			}
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// uriDataEscape is ValueEncoders.UriDataEscape: Uri.EscapeDataString, which
// leaves only the unreserved characters of RFC 3986 and writes the others as
// the percent-encoded UTF-8 bytes in upper case. The runner cuts values of
// more than 65519 characters into segments that do not split a surrogate
// pair; the escape of a character does not depend on its neighbours, so the
// result is the same.
func uriDataEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte('%')
		sb.WriteByte(hex[c>>4])
		sb.WriteByte(hex[c&15])
	}
	return sb.String()
}

// xmlDataEscape is SecurityElement.Escape.
func xmlDataEscape(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;", "&", "&amp;").Replace(s)
}

// trimDoubleQuotes is ValueEncoders.TrimDoubleQuotes: the value without its
// quotes when it has more than 8 characters and starts and ends with one.
func trimDoubleQuotes(s string) string {
	if utf16Len(s) > 8 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return ""
}

// powerShellPreAmpersandEscape is ValueEncoders.PowerShellPreAmpersandEscape:
// the part up to the last "&" (up to "&+" when there is one), when it has at
// least 6 characters.
func powerShellPreAmpersandEscape(s string) string {
	if !strings.Contains(s, "&") {
		return ""
	}
	var section string
	if i := strings.Index(s, "&+"); i >= 0 {
		section = s[:i+2]
	} else {
		section = s[:strings.LastIndex(s, "&")+1]
	}
	if utf16Len(section) >= 6 {
		return section
	}
	return ""
}

// powerShellPostAmpersandEscape is ValueEncoders.PowerShellPostAmpersandEscape:
// the part after the last "&" (after "&+" and the letter that got coloured),
// when it has at least 6 characters.
func powerShellPostAmpersandEscape(s string) string {
	if !strings.Contains(s, "&") {
		return ""
	}
	var section string
	if i := strings.Index(s, "&+"); i >= 0 {
		// Substring(i + 2 + 1): skip the letter that got coloured.
		rest := s[i+2:]
		if _, size := utf8.DecodeRuneInString(rest); size > 0 && len(rest) > size {
			section = rest[size:]
		}
	} else {
		section = s[strings.LastIndex(s, "&")+1:]
	}
	if utf16Len(section) >= 6 {
		return section
	}
	return ""
}

type span struct{ start, end int }

// mask returns s with every secret replaced (SecretMasker.MaskSecrets).
func (m *masker) mask(s string) string {
	if m == nil || s == "" {
		return s
	}
	var spans []span
	m.mu.RLock()
	for _, v := range m.list {
		if len(v) > len(s) || !strings.Contains(s, v) {
			continue
		}
		for from := 0; from <= len(s)-len(v); {
			i := strings.Index(s[from:], v)
			if i < 0 {
				break
			}
			spans = append(spans, span{from + i, from + i + len(v)})
			from += i + 1
		}
	}
	m.mu.RUnlock()
	if len(spans) == 0 {
		return s
	}
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].start < spans[b].start })
	var sb strings.Builder
	pos := 0
	cur := spans[0]
	flush := func(sp span) {
		sb.WriteString(s[pos:sp.start])
		sb.WriteString(maskedValue)
		pos = sp.end
	}
	for _, sp := range spans[1:] {
		if sp.start <= cur.end {
			cur.end = max(cur.end, sp.end)
			continue
		}
		flush(cur)
		cur = sp
	}
	flush(cur)
	sb.WriteString(s[pos:])
	return sb.String()
}

// has reports whether s contains a secret.
func (m *masker) has(s string) bool { return m != nil && m.mask(s) != s }

// maskReporter masks everything the executor reports: the log lines, the names
// of the steps and the error of the job. The outputs are checked where they are
// evaluated (a job output with a secret is dropped).
type maskReporter struct {
	j    *job
	next Reporter

	mu    sync.Mutex
	names map[int]string // the masked name each step started with
}

// StepStarted masks the name with the secrets known when the step starts, which
// is when the runner masks it: ActionRunner.EvaluateDisplayName masks the name
// once, as it evaluates it (ActionRunner.cs:343-347), and nothing masks the
// name of the timeline record afterwards. A secret that the step itself
// registers (add-mask, the token of stop-commands) is thus not applied to its
// own name (recorded: cmd-stop-commands, "pause-logging allowed by step env").
func (r *maskReporter) StepStarted(n int, name string, at time.Time) {
	name = r.j.mask(name)
	r.mu.Lock()
	if r.names == nil {
		r.names = map[int]string{}
	}
	r.names[n] = name
	r.mu.Unlock()
	r.next.StepStarted(n, name, at)
}

func (r *maskReporter) Log(n int, at time.Time, line string) {
	r.next.Log(n, at, r.j.mask(line))
}

func (r *maskReporter) StepCompleted(n int, name string, c Conclusion, at time.Time) {
	// The results of the step (outcome, annotations, summary) go first.
	r.j.flushStepResults(r.next, n, c)
	// The name as reported at the start: the secrets a step registers while it
	// runs do not apply to its own name (see StepStarted).
	r.mu.Lock()
	started, ok := r.names[n]
	delete(r.names, n)
	r.mu.Unlock()
	if ok {
		name = started
	} else {
		name = r.j.mask(name)
	}
	r.next.StepCompleted(n, name, c, at)
}

func (r *maskReporter) JobCompleted(res Result) {
	res.Error = r.j.mask(res.Error)
	r.next.JobCompleted(res)
}
