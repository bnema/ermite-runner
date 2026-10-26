// SPDX-License-Identifier: MIT
package deps

import (
	"bytes"
	"fmt"
	"io"
)

// downloadFilter buffers only the candidate prefix, not arbitrary log lines.
// Non-download output passes through byte-for-byte, including split writes.
type downloadFilter struct {
	dst           io.Writer
	prefix        []byte
	decided, drop bool
	count         int
}

const downloadPrefix = "go: downloading "

func (f *downloadFilter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if !f.decided {
			b := p[0]
			p = p[1:]
			f.prefix = append(f.prefix, b)
			if b != downloadPrefix[len(f.prefix)-1] {
				if _, err := f.dst.Write(f.prefix); err != nil {
					return 0, err
				}
				f.prefix = f.prefix[:0]
				f.decided = b != '\n'
			} else if len(f.prefix) == len(downloadPrefix) {
				f.prefix = f.prefix[:0]
				f.decided = true
				f.drop = true
				f.count++
			}
			continue
		}
		end := bytes.IndexByte(p, '\n')
		size := len(p)
		if end >= 0 {
			size = end + 1
		}
		if !f.drop {
			if _, err := f.dst.Write(p[:size]); err != nil {
				return 0, err
			}
		}
		p = p[size:]
		if end >= 0 {
			f.decided = false
			f.drop = false
		}
	}
	return n, nil
}
func (f *downloadFilter) close() {
	if len(f.prefix) > 0 {
		_, _ = f.dst.Write(f.prefix)
	}
	if f.count > 0 {
		if f.decided && !f.drop {
			_, _ = io.WriteString(f.dst, "\n")
		}
		_, _ = fmt.Fprintf(f.dst, "go: downloaded %d modules\n", f.count)
	}
}
