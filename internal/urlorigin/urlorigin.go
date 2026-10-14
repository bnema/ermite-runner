// SPDX-License-Identifier: MIT

// Package urlorigin gives the one canonical form of the origin of an http(s)
// URL that the host and the executor both compare: the archive token of an
// action goes only to the origin the host named, so the two sides must agree on
// what "the same origin" means, whatever the spelling of the URL.
package urlorigin

import (
	"net"
	"net/url"
	"strings"
)

// Canonical returns "scheme://host:port" for an absolute http or https URL: the
// scheme and the host lower-cased, the default port (80, 443) filled in, an IPv6
// host in brackets (net.JoinHostPort), so that "https://[::1]:8443/x" and
// "https://[::1:8443]/x" (a different host, without a port) differ, and
// "https://a.test" and "https://a.test:443" are the same. It returns "" for
// anything else, including a URL with credentials, which never carries a token.
func Canonical(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	port, ok := effectivePort(scheme, u.Port())
	if !ok || u.Hostname() == "" {
		return ""
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func effectivePort(scheme, port string) (string, bool) {
	switch {
	case port != "":
		if scheme != "http" && scheme != "https" {
			return "", false
		}
		return port, true
	case scheme == "http":
		return "80", true
	case scheme == "https":
		return "443", true
	}
	return "", false
}

// Prefix returns the origin as it is written at the start of a URL: the same
// as Canonical without the default port, which is how a URL is built from it.
func Prefix(raw string) string {
	c := Canonical(raw)
	switch {
	case strings.HasPrefix(c, "http://") && strings.HasSuffix(c, ":80"):
		return strings.TrimSuffix(c, ":80")
	case strings.HasPrefix(c, "https://") && strings.HasSuffix(c, ":443"):
		return strings.TrimSuffix(c, ":443")
	}
	return c
}
