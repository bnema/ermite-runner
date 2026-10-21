// SPDX-License-Identifier: MIT

package host

import "time"

// timeline is protected by jobRun.mu.
type timeline map[string]int64

func (t *timeline) set(key string, d time.Duration) {
	if *t == nil {
		*t = make(timeline)
	}
	(*t)[key] = max(0, d.Milliseconds())
}
func (t *timeline) add(key string, d time.Duration) {
	if *t == nil {
		*t = make(timeline)
	}
	(*t)[key] += max(0, d.Milliseconds())
}
func (t timeline) fields() map[string]any {
	fields := make(map[string]any, len(t))
	for k, v := range t {
		fields[k] = v
	}
	return fields
}
func (j *jobRun) timeSet(key string, d time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.timeline.set(key, d)
}
func (j *jobRun) timingFields() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.timeline.set("total_ms", time.Since(j.receivedAt))
	return j.timeline.fields()
}
