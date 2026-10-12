// SPDX-License-Identifier: MIT

package execstream

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitLine(t *testing.T) {
	if got := SplitLine("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := SplitLine("short", 10); len(got) != 1 || got[0] != "short" {
		t.Fatalf("%q", got)
	}
	// Cuts at character boundaries and joins back exactly.
	in := strings.Repeat("é€😀a", 40) // 2, 3, 4, 1 bytes
	for _, max := range []int{4, 5, 7, 10, 33} {
		parts := SplitLine(in, max)
		var b strings.Builder
		for _, p := range parts {
			if len(p) > max || !utf8.ValidString(p) || p == "" {
				t.Fatalf("max %d: bad piece %q", max, p)
			}
			b.WriteString(p)
		}
		if b.String() != in {
			t.Fatalf("max %d: pieces do not rebuild the line", max)
		}
	}
	// Invalid UTF-8 is replaced before cutting.
	if got := SplitLine("a\xffb", 10); got[0] != "a\ufffdb" {
		t.Fatalf("%q", got)
	}
	big := SplitLine(strings.Repeat("x", 3*MaxLogLine+5), MaxLogLine)
	if len(big) != 4 || len(big[0]) != MaxLogLine || len(big[3]) != 5 {
		t.Fatalf("%d pieces", len(big))
	}
}
