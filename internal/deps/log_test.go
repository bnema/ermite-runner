// SPDX-License-Identifier: MIT
package deps

import (
	"bytes"
	"strings"
	"testing"
)

func TestDownloadFilter(t *testing.T) {
	for _, input := range []string{"go: downloading a v1\ngo: upgraded a v1 => v2\ngo: downloading b v2\ngo: added c v1\nerror: broken\n", "go: downloading a v1\ngo: downloading b v2"} {
		for size := 1; size <= len(input); size++ {
			var out bytes.Buffer
			f := &downloadFilter{dst: &out}
			for start := 0; start < len(input); start += size {
				_, _ = f.Write([]byte(input[start:min(start+size, len(input))]))
			}
			f.close()
			if strings.Contains(out.String(), "go: downloading ") || !strings.HasSuffix(out.String(), "go: downloaded 2 modules\n") {
				t.Fatalf("%q", out.String())
			}
			if strings.Contains(input, "upgraded") && !strings.Contains(out.String(), "go: upgraded a v1 => v2\ngo: added c v1\nerror: broken\n") {
				t.Fatal(out.String())
			}
		}
	}
	var out bytes.Buffer
	f := &downloadFilter{dst: &out}
	_, _ = f.Write([]byte("go: added x\npartial"))
	f.close()
	if out.String() != "go: added x\npartial" {
		t.Fatal(out.String())
	}
	// A final partial line that starts like a download line is ended before
	// the summary.
	for size := 1; size <= 3; size++ {
		input := "go: downloading example.org/a v1\ngo: "
		out.Reset()
		f = &downloadFilter{dst: &out}
		for start := 0; start < len(input); start += size {
			_, _ = f.Write([]byte(input[start:min(start+size, len(input))]))
		}
		f.close()
		if out.String() != "go: \ngo: downloaded 1 modules\n" {
			t.Fatalf("%q", out.String())
		}
	}
}
