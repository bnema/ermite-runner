// SPDX-License-Identifier: MIT
package deps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTailNoise(t *testing.T) {
	tail := &commandTail{}
	input := "\nabc123: Pulling fs layer\nabc123: Waiting\nabc123: Verifying Checksum\nabc123: Download complete\nabc123: Pull complete\nabc123:\nDigest: sha256:abc\nStatus: Downloaded newer image\nUnable to find image locally\ngo: downloading example.org/m v1\ngo: finding module for package x\nCloning into '/repo'...\nHEAD is now at abc\n\x1b[31mreal cause\x1b[0m\r\n\nlast cause"
	for _, b := range []byte(input) {
		_, _ = tail.Write([]byte{b})
	}
	if got := tail.text(); got != "real cause\nlast cause" {
		t.Fatalf("tail = %q", got)
	}
}

func TestTailCaps(t *testing.T) {
	tail := &commandTail{}
	for i := range 20 {
		_, _ = fmt.Fprintf(tail, "line %d\n", i)
	}
	if got := tail.text(); len(strings.Split(got, "\n")) != 10 || !strings.HasPrefix(got, "line 10\n") {
		t.Fatalf("tail = %q", got)
	}
	tail = &commandTail{}
	_, _ = io.WriteString(tail, strings.Repeat("界", 1000)+"final")
	got := tail.text()
	if !utf8.ValidString(got) || len(got) > tailBytes || !strings.HasSuffix(got, "final") || len(got) < tailBytes-3 {
		t.Fatalf("invalid capped tail: %d bytes", len(got))
	}
}

func TestTailRedaction(t *testing.T) {
	token := "super-secret-token"
	for _, secret := range Secrets(token) {
		tail := &commandTail{secrets: Secrets(token)}
		input := strings.Repeat("x", 2000) + secret + " failure\n"
		for _, b := range []byte(input) {
			_, _ = tail.Write([]byte{b})
		}
		got := tail.text()
		if strings.Contains(got, token) || strings.Contains(got, basicCredential(token)) || !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("unmasked tail: %q", got)
		}
	}
}

func TestFailingCommandTail(t *testing.T) {
	var log bytes.Buffer
	ctx := context.Background()
	err := runCommand(ctx, exec.CommandContext(ctx, "sh", "-c", "printf 'go: downloading noise\\n'; printf 'last real line' >&2; exit 7"), &log, nil)
	if err == nil || err.Error() != "dependency command failed (exit 7)\nlast real line" {
		t.Fatalf("error = %v", err)
	}
	if log.String() != "last real line\ngo: downloaded 1 modules\n" {
		t.Fatalf("log = %q", log.String())
	}
}

func TestCommandContextErrors(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := "dependency command canceled"
			if timeout {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = "dependency command timed out"
			}
			cancel()
			err := runCommand(ctx, exec.CommandContext(ctx, "sh", "-c", "exit 1"), io.Discard, nil)
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestTailNormalizationBeforeRedaction(t *testing.T) {
	token := "super-secret-token"
	for _, secret := range Secrets(token) {
		for _, insertion := range []string{"\xff", "\x1b[31m", "\r"} {
			for _, padding := range []int{0, tailBytes - 8, maxTailLine - 8} {
				tail := &commandTail{secrets: Secrets(token)}
				input := strings.Repeat("x", padding) + secret[:6] + insertion + secret[6:] + "\nlast cause\n"
				for _, b := range []byte(input) {
					_, _ = tail.Write([]byte{b})
				}
				got := tail.text()
				if !utf8.ValidString(got) || len(got) > tailBytes || !strings.HasSuffix(got, "last cause") {
					t.Fatalf("invalid tail: %q", got)
				}
				for _, forbidden := range Secrets(token) {
					if strings.Contains(got, forbidden) {
						t.Fatalf("credential reconstructed: %q", got)
					}
				}
				if padding == 0 && insertion == "\xff" && !strings.Contains(got, "\uFFFD") {
					t.Fatalf("invalid byte deleted: %q", got)
				}
				if padding == 0 && insertion != "\xff" && !strings.Contains(got, "[REDACTED]") {
					t.Fatalf("normalized credential not redacted: %q", got)
				}
			}
		}
	}
}

func TestTailOverlongLine(t *testing.T) {
	tail := &commandTail{secrets: Secrets("super-secret-token")}
	chunk := bytes.Repeat([]byte("x"), 1024)
	for range 4096 {
		_, _ = tail.Write(chunk)
	}
	_, _ = io.WriteString(tail, "super-secret-token\nlast cause")
	if got := tail.text(); got != "[line truncated]\nlast cause" {
		t.Fatalf("tail = %q", got)
	}
	if cap(tail.line) > maxTailLine {
		t.Fatalf("buffer capacity = %d", cap(tail.line))
	}
}

func BenchmarkTailLongLine(b *testing.B) {
	chunk := bytes.Repeat([]byte("x"), 1024)
	b.ReportAllocs()
	b.SetBytes(4 << 20)
	b.ResetTimer()
	for range b.N {
		tail := &commandTail{secrets: Secrets("super-secret-token")}
		for range 4096 {
			_, _ = tail.Write(chunk)
		}
		_ = tail.text()
	}
}

type cancelOnOutput struct{ cancel context.CancelFunc }

func (w cancelOnOutput) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestCancellationDuringOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'last cause\\n'; while :; do :; done")
	err := runCommand(ctx, cmd, cancelOnOutput{cancel}, nil)
	if err == nil || err.Error() != "dependency command canceled\nlast cause" {
		t.Fatalf("error = %v", err)
	}
}
