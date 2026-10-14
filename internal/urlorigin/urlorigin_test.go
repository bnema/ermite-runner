// SPDX-License-Identifier: MIT

package urlorigin

import "testing"

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"https://Ermite.example/x":     "https://ermite.example:443",
		"HTTPS://ermite.example":       "https://ermite.example:443",
		"https://ermite.example:443/x": "https://ermite.example:443",
		"http://a.test:80":             "http://a.test:80",
		"http://a.test":                "http://a.test:80",
		"http://a.test:8088/p?q#f":     "http://a.test:8088",
		"https://[::1]:8443/x":         "https://[::1]:8443",
		"https://[::1]/x":              "https://[::1]:443",
		"https://[::1]:443":            "https://[::1]:443",
		"https://[2001:DB8::1]:8443":   "https://[2001:db8::1]:8443",
		"http://[::1]:80":              "http://[::1]:80",
		"http://10.0.2.2:8088":         "http://10.0.2.2:8088",
		"ftp://a.test":                 "", "a.test": "", "https://u@a.test": "", "https://u:p@a.test": "", "": "", "https://": "", "//a.test": "",
		"https://[::1:8443]/x": "https://[::1:8443]:443", // an IPv6 host with no port at all
	} {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
	// GO-012: the host ::1 on port 8443 is not the host ::1:8443.
	if Canonical("https://[::1]:8443/x") == Canonical("https://[::1:8443]/x") {
		t.Error("two different origins collide")
	}
	// Spellings of one origin agree.
	for _, pair := range [][2]string{
		{"https://a.test", "https://A.TEST:443/path"}, {"http://a.test:80/", "http://a.test"}, {"https://[::1]", "https://[::1]:443"},
	} {
		if Canonical(pair[0]) != Canonical(pair[1]) {
			t.Errorf("%q and %q differ: %q %q", pair[0], pair[1], Canonical(pair[0]), Canonical(pair[1]))
		}
	}
	// Other ports, schemes and hosts differ.
	for _, pair := range [][2]string{
		{"https://a.test", "http://a.test"}, {"https://a.test", "https://a.test:8443"}, {"https://a.test", "https://b.a.test"}, {"http://a.test", "http://a.test:443"},
	} {
		if Canonical(pair[0]) == Canonical(pair[1]) {
			t.Errorf("%q and %q collide", pair[0], pair[1])
		}
	}
}

func TestPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"https://Ermite.example:443/x": "https://ermite.example", "http://a.test:80": "http://a.test", "http://a.test:8088": "http://a.test:8088",
		"https://[::1]:443": "https://[::1]", "http://[::1]:80/": "http://[::1]", "https://[::1]:8443": "https://[::1]:8443",
		"http://10.0.2.2:8088": "http://10.0.2.2:8088", "https://a.test:80": "https://a.test:80", "ftp://x": "",
	} {
		if got := Prefix(in); got != want {
			t.Errorf("Prefix(%q) = %q, want %q", in, got, want)
		}
	}
}
