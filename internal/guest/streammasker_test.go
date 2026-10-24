// SPDX-License-Identifier: MIT
package guest

import (
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/deps"
)

func collect(secrets ...string) (*streamMasker, *strings.Builder, *int) {
	var out strings.Builder
	writes := 0
	m := newStreamMasker(func(s string) error { out.WriteString(s); writes++; return nil }, secrets...)
	return m, &out, &writes
}

func TestStreamMaskerEverySplit(t *testing.T) {
	secrets := deps.Secrets("ghs_abcDEF123456")
	for _, sec := range secrets {
		text := "before " + sec + " after"
		for i := 0; i <= len(text); i++ {
			m, out, _ := collect(secrets...)
			_, _ = m.Write([]byte(text[:i]))
			_, _ = m.Write([]byte(text[i:]))
			_ = m.Close()
			if got := out.String(); got != "before *** after" {
				t.Fatalf("secret %q split %d: %q", sec, i, got)
			}
		}
	}
}

func TestStreamMaskerByteByByte(t *testing.T) {
	secrets := deps.Secrets("tok-123")
	m, out, _ := collect(secrets...)
	for _, b := range []byte("a tok-123 b x-access-token:tok-123 c") {
		_, _ = m.Write([]byte{b})
	}
	_ = m.Close()
	if got := out.String(); got != "a *** b *** c" {
		t.Fatalf("%q", got)
	}
}

func TestStreamMaskerNoFalseRetention(t *testing.T) {
	m, out, _ := collect(deps.Secrets("tok-123")...)
	_, _ = m.Write([]byte("plain output\n"))
	if out.String() != "plain output\n" || m.tail != "" {
		t.Fatalf("retained: out=%q tail=%q", out.String(), m.tail)
	}
	_, _ = m.Write([]byte("to")) // possible secret prefix is held
	_ = m.Close()
	if out.String() != "plain output\nto" {
		t.Fatalf("%q", out.String())
	}
}

func TestStreamMaskerBasicHeaderMasked(t *testing.T) {
	m, out, _ := collect(deps.Secrets("tok")...)
	cred := deps.Secrets("tok")[2]
	_, _ = m.Write([]byte("hdr " + cred[:5]))
	_, _ = m.Write([]byte(cred[5:] + "\n"))
	_ = m.Close()
	if strings.Contains(out.String(), cred) || out.String() != "hdr ***\n" {
		t.Fatalf("%q", out.String())
	}
}
