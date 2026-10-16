// SPDX-License-Identifier: MIT

package actionscache

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"bnema.dev/ermite-runner/internal/urlorigin"
)

func originOf(raw string) string { return urlorigin.Canonical(raw) }

// NewClient returns the client that downloads the archives. It follows
// redirects (GitHub's codeload does redirect) but never carries credentials
// across an origin: the Authorization header is dropped as soon as the scheme,
// the host or the port changes, and a redirect from https to http is refused.
// (net/http alone would keep the header for subdomains and ignore the scheme and
// the port.) Tests may supply their own transport through Actions.HTTPClient;
// its redirect policy is replaced.
func NewClient(base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		cp := *base
		c = &cp
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		// Go builds every redirected request from the headers of the first one
		// (copying Authorization when the host matches), so the header must be
		// removed again on every hop that is not on the origin the token was meant
		// for: the first request's. Once the chain left that origin, it never gets
		// the header back, even if a later hop returns to it (A -> B -> A would
		// otherwise be re-authorised by the copy).
		origin := originOf(via[0].URL.String())
		chain := slices.Concat(via[1:], []*http.Request{req}) // not append: via belongs to the client
		for i, r := range chain {
			prev := via[i]
			if prev.URL.Scheme == "https" && r.URL.Scheme != "https" {
				return fmt.Errorf("refusing to follow a redirect from https to %s", r.URL.Scheme)
			}
		}
		for _, r := range chain {
			if originOf(r.URL.String()) != origin {
				req.Header.Del("Authorization")
				break
			}
		}
		return nil
	}
	return c
}
