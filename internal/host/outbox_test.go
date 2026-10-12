// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
)

// AppendNow says so when an earlier send was cut short and the offset may be
// behind the server's (GO-005), and stays silent otherwise.
func TestAppendNowWarnsOfAStaleOffset(t *testing.T) {
	const warning = "log offset may be stale"
	logs := &logBuf{}
	log := zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: logs})
	tm := Timing{CallTimeout: time.Hour, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond}
	cl := &stubClient{hang: true}
	o := newOutbox(cl, "j1", tm, func(error) {}, log)

	// A send that Close cut short: the server may have applied it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	o.sendLines(ctx, []event{{line: &logLine{step: 1, text: "a"}}})

	cl.mu.Lock()
	cl.hang = false
	cl.mu.Unlock()
	o.AppendNow(context.Background(), logLine{step: 1, text: "b"})
	if !strings.Contains(logs.String(), warning) {
		t.Fatalf("no warning about the offset: %q", logs.String())
	}

	// The send went through: the offset is the server's again.
	logs.mu.Lock()
	logs.b.Reset()
	logs.mu.Unlock()
	o.AppendNow(context.Background(), logLine{step: 1, text: "c"})
	if strings.Contains(logs.String(), warning) {
		t.Fatalf("a warning with a fresh offset: %q", logs.String())
	}
}
