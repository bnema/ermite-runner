// SPDX-License-Identifier: MIT

package host

import "time"

// timeline is the per-phase timing of a job, logged on its "job finished"
// line: durations in milliseconds (the *_ms keys) and counters. A jobRun
// creates it once, in startJob; it is guarded by jobRun.mu.
type timeline map[string]int64

// setMs records d as the duration of a phase, in whole milliseconds.
func (t timeline) setMs(key string, d time.Duration) { t[key] = max(0, d.Milliseconds()) }

// addMs adds d to the summed duration of a phase that may run several times.
func (t timeline) addMs(key string, d time.Duration) { t[key] += max(0, d.Milliseconds()) }

// add adds n to a counter.
func (t timeline) add(key string, n int64) { t[key] += n }

// guestBootKeys are the boot phases the guest may report in its Hello: the
// host logs these as guest_<key>_ms and ignores any other key, as it does not
// trust the guest.
var guestBootKeys = []string{"kernel", "setup", "containerd", "dockerd"}

// addGuestBoot records the guest's boot phases: known keys with a value of
// at least 0.
func (t timeline) addGuestBoot(ms map[string]int64) {
	for _, k := range guestBootKeys {
		if v, ok := ms[k]; ok && v >= 0 {
			t["guest_"+k+"_ms"] = v
		}
	}
}

func (j *jobRun) timeSet(key string, d time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.timeline.setMs(key, d)
}

// timingFields sets total_ms and returns a copy of the timeline for the log.
func (j *jobRun) timingFields() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.timeline.setMs("total_ms", time.Since(j.receivedAt))
	fields := make(map[string]any, len(j.timeline))
	for k, v := range j.timeline {
		fields[k] = v
	}
	return fields
}
