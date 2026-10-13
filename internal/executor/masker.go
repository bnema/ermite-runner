// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"
)

// masker replaces secret values by *** in text that leaves the executor
// (HostContext.SecretMasker). A value is masked as is and in the forms the
// official runner registers for it (HostContext.cs:103-113, ValueEncoders): its
// base64 at the three byte offsets a prefix of one or two bytes can give
// ("user:" + secret is base64-encoded as a whole), and its JSON and URI
// escapes. Longer forms are replaced first.
//
// Bytes that follow the secret change the last, incomplete group of its base64
// (`echo "$T" | base64` appends a newline), so each base64 form is also
// registered without that group: only the characters the secret alone
// determines. actions/runner (ValueEncoders.Base64StringEscapeShift) only
// registers the full forms; the truncated ones are an addition.
type masker struct{ values []string }

const maskedValue = "***"

// minBase64Tail is the shortest base64 form, in characters, registered once its
// incomplete last group is removed: a shorter one (3 bytes of the secret)
// would mask ordinary text.
const minBase64Tail = 8

func newMasker(secrets ...string) *masker {
	seen := map[string]bool{}
	var vals []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	for _, s := range secrets {
		add(s)
		for _, e := range secretEncodings(s) {
			add(e)
		}
	}
	slices.SortStableFunc(vals, func(a, b string) int { return len(b) - len(a) })
	return &masker{values: vals}
}

// secretEncodings are the other forms of v that the runner masks.
func secretEncodings(v string) []string {
	var out []string
	b := []byte(v)
	for shift := range 3 {
		if len(b) <= shift && shift != 0 {
			continue
		}
		sub := b[shift:]
		out = append(out, base64.StdEncoding.EncodeToString(sub))
		// Whole groups only: 3 bytes give 4 characters that no later byte changes.
		if n := len(sub) / 3 * 4; n >= minBase64Tail {
			out = append(out, base64.StdEncoding.EncodeToString(sub[:len(sub)/3*3]))
		}
	}
	if j, err := json.Marshal(v); err == nil && len(j) > 2 {
		out = append(out, string(j[1:len(j)-1]))
	}
	out = append(out, url.QueryEscape(v), url.PathEscape(v))
	return out
}

// mask returns s with every secret replaced.
func (m *masker) mask(s string) string {
	if m == nil {
		return s
	}
	for _, v := range m.values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, maskedValue)
		}
	}
	return s
}

// has reports whether s contains a secret.
func (m *masker) has(s string) bool { return m != nil && m.mask(s) != s }

// maskReporter masks everything the executor reports: the log lines, the names
// of the steps and the error of the job. The outputs are checked where they are
// evaluated (a job output with a secret is dropped).
type maskReporter struct {
	j    *job
	next Reporter
}

func (r *maskReporter) StepStarted(n int, name string, at time.Time) {
	r.next.StepStarted(n, r.j.mask(name), at)
}

func (r *maskReporter) Log(n int, at time.Time, line string) {
	r.next.Log(n, at, r.j.mask(line))
}

func (r *maskReporter) StepCompleted(n int, name string, c Conclusion, at time.Time) {
	r.next.StepCompleted(n, r.j.mask(name), c, at)
}

func (r *maskReporter) JobCompleted(res Result) {
	res.Error = r.j.mask(res.Error)
	r.next.JobCompleted(res)
}
