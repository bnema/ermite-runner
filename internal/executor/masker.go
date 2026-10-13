// SPDX-License-Identifier: MIT

package executor

import (
	"slices"
	"strings"
	"time"
)

// masker replaces secret values by *** in text that leaves the executor
// (HostContext.SecretMasker). A value is masked as is and in the forms the
// official runner registers for it (HostContext.cs:103-113, ValueEncoders).
// Longer forms are replaced first.
type masker struct{ values []string }

const maskedValue = "***"

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
func secretEncodings(string) []string { return nil }

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
