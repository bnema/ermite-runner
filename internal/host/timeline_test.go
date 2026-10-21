// SPDX-License-Identifier: MIT

package host

import (
	"testing"
	"time"
)

func TestTimeline(t *testing.T) {
	var timeline timeline
	timeline.set("run_ms", 3*time.Millisecond)
	timeline.add("run_ms", 2*time.Millisecond)
	fields := timeline.fields()
	if fields["run_ms"] != int64(5) {
		t.Fatal(fields)
	}
	fields["run_ms"] = int64(99)
	if timeline.fields()["run_ms"] != int64(5) {
		t.Fatal("shared fields")
	}
	timeline.set("run_ms", time.Millisecond)
	if timeline.fields()["run_ms"] != int64(1) {
		t.Fatal(timeline)
	}
}
