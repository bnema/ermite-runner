// SPDX-License-Identifier: MIT

package host

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	ermite "bnema.dev/ermite-api/gen/ermite"
)

// jobFinished returns the fields of the one "job finished" line of logs.
func jobFinished(t *testing.T, logs string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(logs, "\n") {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) != nil || fields["message"] != "job finished" {
			continue
		}
		if found != nil {
			t.Fatal("two job finished lines")
		}
		found = fields
	}
	if found == nil {
		t.Fatalf("no job finished line: %s", logs)
	}
	return found
}

// The guest is untrusted: of its Hello.boot_ms, the job finished line keeps
// only the known phases with a value of at least 0, whatever else it sends.
func TestHostileGuestBootTimings(t *testing.T) {
	h := newHarness(t, simpleScript("hello"))
	hostile := map[string]int64{
		"kernel":     5,
		"dockerd":    -7, // negative: dropped
		"setup":      0,
		"total":      1, // would overwrite a host field as guest_total_ms
		"":           3,
		"x\"y":       4,
		"../../etc":  5,
		"containerd": 9,
	}
	for i := range 500 {
		hostile[fmt.Sprint("junk", i)] = int64(i)
	}
	h.driver.bootMs = hostile
	h.srv.jobs = []*ermite.Job{testJob("j1")}
	stop := h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	fields := jobFinished(t, h.logs.String())
	var guest []string
	for k := range fields {
		if strings.HasPrefix(k, "guest_") {
			guest = append(guest, k)
		}
	}
	want := map[string]float64{"guest_kernel_ms": 5, "guest_setup_ms": 0, "guest_containerd_ms": 9}
	if len(guest) != len(want) {
		t.Fatalf("guest fields %v, want %v", guest, want)
	}
	for k, v := range want {
		if got, ok := fields[k].(float64); !ok || got != v {
			t.Errorf("%s = %v, want %v", k, fields[k], v)
		}
	}
}
