// SPDX-License-Identifier: MIT
package deps

import (
	"bytes"
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

// commandTail normalizes and redacts complete bounded lines before clipping.
// Overlong lines are discarded in their entirety, never exposing partial secrets.
// exec.Cmd serializes writes when stdout and stderr share this writer.
type commandTail struct {
	secrets   []string
	line      []byte
	truncated bool
	lines     []string
}

const maxTailLine = 4096

func (t *commandTail) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if !t.truncated {
			if len(part) > maxTailLine-len(t.line) {
				t.truncated = true
				t.line = t.line[:0]
			} else {
				if t.line == nil {
					t.line = make([]byte, 0, maxTailLine)
				}
				t.line = append(t.line, part...)
			}
		}
		if end < 0 {
			break
		}
		t.finishLine()
		p = p[end+1:]
	}
	return n, nil
}

func utf8Suffix(s string, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func (t *commandTail) finishLine() {
	// Invalid bytes must not be deleted: doing so could join credential fragments.
	s := strings.ToValidUTF8(string(t.line), "\uFFFD")
	s = strings.TrimSpace(strings.ReplaceAll(ansiEscape.ReplaceAllString(s, ""), "\r", ""))
	if t.truncated {
		s = "[line truncated]"
	}
	t.line = t.line[:0]
	t.truncated = false
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
	if len(t.line) != 0 || t.truncated {
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
		baseSummary := summary
		if text := tail.text(); text != "" {
			summary += "\n" + text
		}
		// Defense in depth after all normalization, clipping and concatenation.
		// Discard the tail rather than apply another potentially joining transform.
		for _, secret := range secrets {
			if secret != "" && strings.Contains(summary, secret) {
				summary = baseSummary + "\n[output redacted]"
				break
			}
		}
		return errors.New(summary)
	}
	return nil
}
