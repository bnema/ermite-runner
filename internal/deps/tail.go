// SPDX-License-Identifier: MIT
package deps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf8"
)

const tailBytes = 1900 // Leave room for the summary within a 2 KiB error.

var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)
var pullNoise = regexp.MustCompile(`(?i)^(?:[0-9a-f]+:\s*(?:Pulling fs layer|Waiting|Verifying Checksum|Download complete|Pull complete|Downloading|Extracting)?|Digest:.*|Status: Downloaded.*|Unable to find image.*)$`)

// commandTail retains only a bounded suffix. Secrets are masked before any
// clipping, including when a credential spans writes or exceeds the line cap.
// exec.Cmd serializes writes when stdout and stderr share this writer.
type commandTail struct {
	secrets []string
	pending string
	line    string
	lines   []string
}

func (t *commandTail) Write(p []byte) (int, error) {
	for _, b := range p {
		t.pending += string([]byte{b})
		t.drain(false)
	}
	return len(p), nil
}

func (t *commandTail) drain(final bool) {
	for t.pending != "" {
		prefix := false
		matched := false
		for _, secret := range t.secrets {
			if secret == "" {
				continue
			}
			if strings.HasPrefix(t.pending, secret) {
				t.emit("[REDACTED]")
				t.pending = t.pending[len(secret):]
				matched = true
				break
			}
			if strings.HasPrefix(secret, t.pending) {
				prefix = true
			}
		}
		if matched {
			continue
		}
		if prefix && !final {
			return
		}
		t.emit(t.pending[:1])
		t.pending = t.pending[1:]
	}
}

func (t *commandTail) emit(s string) {
	for _, b := range []byte(s) {
		if b == '\n' {
			t.finishLine()
			continue
		}
		t.line += string([]byte{b})
		if len(t.line) > 4096 {
			t.line = t.line[len(t.line)-4096:]
		}
	}
}

func utf8Suffix(s string, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return strings.ToValidUTF8(s, "")
}

func (t *commandTail) finishLine() {
	s := strings.TrimSpace(strings.ReplaceAll(ansiEscape.ReplaceAllString(t.line, ""), "\r", ""))
	t.line = ""
	for _, secret := range t.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	if s == "" || pullNoise.MatchString(s) {
		return
	}
	for _, prefix := range []string{"go: downloading ", "go: finding module ", "Cloning into", "HEAD is now at"} {
		if strings.HasPrefix(s, prefix) {
			return
		}
	}
	t.lines = append(t.lines, utf8Suffix(s, tailBytes))
	for len(t.lines) > 10 || len(strings.Join(t.lines, "\n")) > tailBytes {
		t.lines = t.lines[1:]
	}
}

func (t *commandTail) text() string {
	t.drain(true)
	if t.line != "" {
		t.finishLine()
	}
	return strings.Join(t.lines, "\n")
}

func runCommand(ctx context.Context, cmd *exec.Cmd, log io.Writer, secrets []string) error {
	tail := &commandTail{secrets: secrets}
	output := io.MultiWriter(log, tail)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		summary := "dependency command failed"
		var exit *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			summary = "dependency command timed out"
		case errors.Is(ctx.Err(), context.Canceled):
			summary = "dependency command canceled"
		case errors.As(err, &exit):
			summary = fmt.Sprintf("dependency command failed (exit %d)", exit.ExitCode())
		}
		if text := tail.text(); text != "" {
			summary += "\n" + text
		}
		return errors.New(summary)
	}
	return nil
}
