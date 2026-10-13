// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Cases ported from actions/runner src/Test/L0/HostContextL0.cs
// (DefaultSecretMaskers, SecretSectionMasking), MIT, (c) 2019 GitHub; see
// THIRD_PARTY.md.
func TestDefaultSecretMaskers(t *testing.T) {
	m := newMasker()
	for _, v := range []string{"Password123!", "Pass\"word\"123!", "Pass word 123!", "Pass<word>123!", "Pass'word'123!", "\"Password123!!\"", "\"short\""} {
		m.add(v)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct{ in, want string }{
		{"123Password123!123", "123***123"},
		{"password123", "password123"},
		{`123Pass\"word\"123!123`, "123***123"},
		{"123Pass%20word%20123%21123", "123***123"},
		{"123Pass&lt;word&gt;123!123", "123***123"},
		{"123Pass''word''123!123", "123***123"},
		{b64(":Password123!"), "OlBh***"},
		{b64("a:Password123!"), "YTpQ***"},
		{b64("ab:Password123!"), "YWI6***"},
		{b64("abc:Password123!"), "YWJjOlBh***"},
		{b64("abcd:Password123!"), "YWJjZDpQ***"},
		{b64("abcde:Password123!"), "YWJjZGU6***"},
		{"123Password123!!123", "123***123"},
		{"123short123", "123short123"},
		{"123\"short\"123", "123***123"},
	} {
		if got := m.mask(tc.in); got != tc.want {
			t.Errorf("mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSecretSectionMasking(t *testing.T) {
	for _, tc := range []struct{ secret, raw, want string }{
		{"secret&secret&secret", "secret&secret&\x1b[96msecret\x1b[0m", "***\x1b[96m***\x1b[0m"},
		{"secret&secret+secret", "secret&\x1b[96msecret+secret\x1b[0m", "***\x1b[96m***\x1b[0m"},
		{"secret+secret&secret", "secret+secret&\x1b[96msecret\x1b[0m", "***\x1b[96m***\x1b[0m"},
		{"secret&secret&+secretsecret", "secret&secret&+\x1b[96ms\x1b[0mecretsecret", "***\x1b[96ms\x1b[0m***"},
		{"secret&+secret&secret", "secret&+\x1b[96ms\x1b[0mecret&secret", "***\x1b[96ms\x1b[0m***"},
		{"secret&+secret&+secret", "secret&+\x1b[96ms\x1b[0mecret&+secret", "***\x1b[96ms\x1b[0m***"},
		{"secret&+secret&secret&+secret", "secret&+\x1b[96ms\x1b[0mecret&secret&+secret", "***\x1b[96ms\x1b[0m***"},
		{"secret&secret&+", "secret&secret&+\x1b[96m\x1b[0m", "***\x1b[96m\x1b[0m"},
	} {
		m := newMasker(tc.secret)
		if got := m.mask(tc.raw); got != tc.want {
			t.Errorf("secret %q: mask(%q) = %q, want %q", tc.secret, tc.raw, got, tc.want)
		}
	}
}

// Encoders of ValueEncoders.cs, one by one.
func TestValueEncoders(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"cmdline", strings.ReplaceAll(`a"b`, `"`, `\"`), `a\"b`},
		{"json", jsonStringEscape("a\"b\\c\n\t\x01/é\u2028"), `a\"b\\c\n\t\u0001/é\u2028`},
		{"uri", uriDataEscape("a b/c~d-é"), "a%20b%2Fc~d-%C3%A9"},
		{"xml", xmlDataEscape(`<a href="x">'&'</a>`), "&lt;a href=&quot;x&quot;&gt;&apos;&amp;&apos;&lt;/a&gt;"},
		{"trim 9 chars", trimDoubleQuotes(`"12345678"`), "12345678"},
		{"trim 8 chars", trimDoubleQuotes(`"123456"`), ""},
		{"trim no quote", trimDoubleQuotes(`12345678901`), ""},
		{"pre", powerShellPreAmpersandEscape("secret&secret&secret"), "secret&secret&"},
		{"pre plus", powerShellPreAmpersandEscape("secret&+secret&secret"), "secret&+"},
		{"pre short", powerShellPreAmpersandEscape("ab&c"), ""},
		{"pre no ampersand", powerShellPreAmpersandEscape("secretsecret"), ""},
		{"post", powerShellPostAmpersandEscape("secret&secret&secret"), "secret"},
		{"post plus", powerShellPostAmpersandEscape("secret&+Xsecret&secret"), "secret&secret"},
		{"post plus, nothing after", powerShellPostAmpersandEscape("secret&+X"), ""},
		{"post short", powerShellPostAmpersandEscape("secret&abc"), ""},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// Adding a value after the first masks makes later text masked, empty values
// are ignored, overlapping matches merge into one ***.
func TestMaskerAddAndMerge(t *testing.T) {
	m := newMasker()
	m.add("")
	if m.mask("abc") != "abc" {
		t.Fatal("empty value masks")
	}
	m.add("abcd")
	m.add("cdef")
	if got := m.mask("xabcdefy abcd"); got != "x***y ***" {
		t.Errorf("%q", got)
	}
	if got := m.mask(""); got != "" {
		t.Error(got)
	}
	// the same value again changes nothing
	m.add("abcd")
	if got := m.mask("abcd"); got != "***" {
		t.Error(got)
	}
}
