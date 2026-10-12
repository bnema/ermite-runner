// SPDX-License-Identifier: MIT

package hosttest

import (
	"io"
	"strings"
	"sync"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
)

// LogStore is the log of one job as the server of runner.proto keeps it, with
// the rules of AppendLog (the one place where they are written down for tests):
//
//   - a request has 1 to 1,000 lines, at most 64 KiB of text in total, no line
//     feed in a line, a time on every line, and a continued first line only if
//     it follows a line;
//   - offset is the number of lines the server holds: equal appends, below skips
//     the first (count - offset) lines of the request (a retry), above is
//     out_of_range;
//   - the step of a request is at least the step of the last stored line
//     (failed_precondition otherwise; lines skipped as already stored are not
//     checked).
//
// A request that breaks a rule the client must never break (the first group)
// is reported through OnViolation as well as answered with the error.
type LogStore struct {
	// OnViolation, when set, receives the description of each client violation.
	OnViolation func(string)

	mu    sync.Mutex
	lines []Line
}

// Lines returns a copy of the stored lines.
func (s *LogStore) Lines() []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Line(nil), s.lines...)
}

// Apply applies a request and returns the number of lines held afterwards.
func (s *LogStore) Apply(r *ermite.AppendLogRequest) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bad := func(code connect.Code, why string) (int64, error) {
		if s.OnViolation != nil {
			s.OnViolation("AppendLog: " + why)
		}
		return 0, connect.NewError(code, io.EOF)
	}
	if r.Step < 1 {
		return bad(connect.CodeInvalidArgument, "step below 1")
	}
	n, bytes := len(r.Lines), 0
	if n == 0 || n > 1000 {
		return bad(connect.CodeInvalidArgument, "bad number of lines")
	}
	have := int64(len(s.lines))
	for i, l := range r.Lines {
		bytes += len(l.Text)
		if strings.Contains(l.Text, "\n") || l.Time == nil {
			return bad(connect.CodeInvalidArgument, "invalid line")
		}
		if l.Continued && i == 0 && have == 0 && r.Offset == 0 {
			return bad(connect.CodeInvalidArgument, "the first line of the job is continued")
		}
	}
	if bytes > 64*1024 {
		return bad(connect.CodeInvalidArgument, "more than 64 KiB")
	}
	if r.Offset > have {
		return bad(connect.CodeOutOfRange, "a gap in the offsets")
	}
	skip := have - r.Offset
	if skip < int64(n) && have > 0 && r.Step < s.lines[have-1].Step {
		return bad(connect.CodeFailedPrecondition, "a step went back")
	}
	for i, l := range r.Lines {
		if int64(i) < skip {
			continue
		}
		s.lines = append(s.lines, Line{Step: r.Step, Text: l.Text, Continued: l.Continued})
	}
	return int64(len(s.lines)), nil
}
