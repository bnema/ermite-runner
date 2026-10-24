// SPDX-License-Identifier: MIT
package guest

import (
	"strings"
	"sync"
)

const streamMask = "***"

// streamMasker masks secrets in a byte stream. Between writes it keeps only
// the longest suffix that is a proper prefix of a secret (at most longest-1
// bytes), so a secret split across writes is masked and text without a secret
// is released at once. Close releases what is kept.
type streamMasker struct {
	mu      sync.Mutex
	secrets []string
	sink    func(string) error
	tail    string
}

func newStreamMasker(sink func(string) error, secrets ...string) *streamMasker {
	m := &streamMasker{sink: sink}
	for _, s := range secrets {
		if s != "" {
			m.secrets = append(m.secrets, s)
		}
	}
	return m
}

func (m *streamMasker) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := m.tail + string(p)
	m.tail = ""
	buf = m.mask(buf)
	keep := m.partial(buf)
	m.tail = buf[len(buf)-keep:]
	if out := buf[:len(buf)-keep]; out != "" {
		if err := m.sink(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close flushes the kept tail, which cannot be a whole secret.
func (m *streamMasker) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.tail
	m.tail = ""
	if out == "" {
		return nil
	}
	return m.sink(out)
}

// mask replaces every occurrence, leftmost first and longest first on a tie.
func (m *streamMasker) mask(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		at, n := -1, 0
		for _, sec := range m.secrets {
			if i := strings.Index(s, sec); i >= 0 && (at < 0 || i < at || (i == at && len(sec) > n)) {
				at, n = i, len(sec)
			}
		}
		if at < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:at])
		b.WriteString(streamMask)
		s = s[at+n:]
	}
	return b.String()
}

// partial is the length of the longest suffix of s that is a proper prefix of
// a secret.
func (m *streamMasker) partial(s string) int {
	best := 0
	for _, sec := range m.secrets {
		for n := min(len(sec)-1, len(s)); n > best; n-- {
			if strings.HasSuffix(s, sec[:n]) {
				best = n
				break
			}
		}
	}
	return best
}
