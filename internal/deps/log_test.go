// SPDX-License-Identifier: MIT
package deps

import (
	"bytes"
	"testing"
)

func TestDownloadFilter(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{
			"download lines only",
			"go: downloading a v1\ngo: downloading b v2\n",
			"go: downloaded 2 modules\n",
		},
		{
			"other lines pass through",
			"go: downloading a v1\ngo: upgraded a v1 => v2\ngo: downloading b v2\ngo: added c v1\nerror: broken\n",
			"go: upgraded a v1 => v2\ngo: added c v1\nerror: broken\ngo: downloaded 2 modules\n",
		},
		{
			"unterminated download line",
			"go: downloading a v1\ngo: downloading b v2",
			"go: downloaded 2 modules\n",
		},
		{
			"no download line",
			"go: added x\npartial",
			"go: added x\npartial",
		},
		{
			"partial line after a download is ended before the summary",
			"go: downloading example.org/a v1\ngo: ",
			"go: \ngo: downloaded 1 modules\n",
		},
		{
			"partial line that is not a download prefix",
			"go: downloading example.org/a v1\ngo: added",
			"go: added\ngo: downloaded 1 modules\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Whatever the write boundaries, the output is the same.
			for size := 1; size <= len(tc.input); size++ {
				var out bytes.Buffer
				f := &downloadFilter{dst: &out}
				for start := 0; start < len(tc.input); start += size {
					_, _ = f.Write([]byte(tc.input[start:min(start+size, len(tc.input))]))
				}
				f.close()
				if out.String() != tc.want {
					t.Fatalf("chunk size %d: got %q, want %q", size, out.String(), tc.want)
				}
			}
		})
	}
}
