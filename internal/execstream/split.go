// SPDX-License-Identifier: MIT

package execstream

import (
	"strings"
	"unicode/utf8"
)

// MaxLogLine is the largest piece of a log line that fits an AppendLog line
// (64 KiB); longer lines are cut by SplitLine.
const MaxLogLine = 64 * 1024

// SplitLine cuts text into pieces of at most max bytes at UTF-8 character
// boundaries. The result has one piece for a short line, and always at least
// one (an empty line is one empty piece). Invalid UTF-8 in text is replaced
// with U+FFFD first, because the protocol only carries valid text.
func SplitLine(text string, max int) []string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\ufffd")
	}
	if len(text) <= max {
		return []string{text}
	}
	var out []string
	for len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 { // cannot happen with max >= utf8.UTFMax
			cut = max
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}
