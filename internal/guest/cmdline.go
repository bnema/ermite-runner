// SPDX-License-Identifier: MIT

// Package guest is the code that runs inside a sandbox: `ermite-runner init`
// (the guest PID 1) and `ermite-runner exec` (the executor).
package guest

import "strings"

// ParseCmdline splits a kernel command line into key=value pairs; a bare word
// has the value "". The last occurrence of a key wins. Values may be quoted
// with double quotes, as the kernel allows.
func ParseCmdline(s string) map[string]string {
	out := map[string]string{}
	for _, f := range splitCmdline(strings.TrimSpace(s)) {
		k, v, _ := strings.Cut(f, "=")
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

func splitCmdline(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
