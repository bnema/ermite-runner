// SPDX-License-Identifier: MIT

// Package cmdrun runs host commands (ip, nft, mkfs.ext4) behind a small
// interface so that callers can be tested with a fake.
package cmdrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner runs a command to completion and returns its combined output.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error)
}

// Exec runs real commands.
type Exec struct{}

// Run implements Runner. A non-zero exit becomes an error carrying the command
// line and its output.
func (Exec) Run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// Call is one recorded invocation of a Fake.
type Call struct {
	Name  string
	Args  []string
	Stdin string
}

// String renders the call as a shell-like line.
func (c Call) String() string { return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " ")) }

// Fake records calls and answers them with Handle (nil means success).
type Fake struct {
	Calls  []Call
	Handle func(Call) ([]byte, error)
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	c := Call{Name: name, Args: append([]string(nil), args...)}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.Stdin = string(b)
	}
	f.Calls = append(f.Calls, c)
	if f.Handle != nil {
		return f.Handle(c)
	}
	return nil, nil
}

// Lines returns the recorded calls as strings.
func (f *Fake) Lines() []string {
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.String()
	}
	return out
}
