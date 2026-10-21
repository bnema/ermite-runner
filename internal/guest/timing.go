// SPDX-License-Identifier: MIT

package guest

import (
	"sort"
	"strconv"
	"strings"
)

func formatBootMs(ms map[string]int64) string {
	keys := make([]string, 0, len(ms))
	for k := range ms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+strconv.FormatInt(ms[k], 10))
	}
	return strings.Join(pairs, ",")
}

// ParseBootMs reads init's boot timing environment, ignoring malformed pairs.
func ParseBootMs(value string) map[string]int64 {
	ms := make(map[string]int64)
	for _, pair := range strings.Split(value, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err == nil && n >= 0 {
			ms[key] = n
		}
	}
	return ms
}
