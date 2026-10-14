// SPDX-License-Identifier: MIT

package guest

import "testing"

func TestCacheURL(t *testing.T) {
	for _, tc := range []struct {
		gw   string
		port uint32
		want string
	}{
		// The toolkit appends "_apis/artifactcache/…" to this: the slash is needed.
		{"10.200.0.1", 8089, "http://10.200.0.1:8089/"},
		{"", 8089, ""},
		{"10.200.0.1", 0, ""},
		{"10.200.0.1", 70000, ""},
	} {
		if got := cacheURL(tc.gw, tc.port); got != tc.want {
			t.Errorf("cacheURL(%q, %d) = %q, want %q", tc.gw, tc.port, got, tc.want)
		}
	}
}
