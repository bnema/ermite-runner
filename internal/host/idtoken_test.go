// SPDX-License-Identifier: MIT

package host

import (
	"testing"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

const (
	oidcPublicURL = "https://ermite.bnema.dev/-/oidc/token?api-version=2.0"
	oidcToken     = "ermiteoidc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// The executor gets Job.id_token_url, its origin rewritten to the guest forge
// like github.server_url (path and query kept), and the token, only when the
// server sent both.
func TestIDTokenRequestReachesTheExecutor(t *testing.T) {
	for _, tc := range []struct {
		name, url, token, guest string
		wantURL, wantToken      string
	}{
		{"guest forge", oidcPublicURL, oidcToken, "http://ermite.bnema.dev:8088", "http://ermite.bnema.dev:8088/-/oidc/token?api-version=2.0", oidcToken},
		{"no guest forge", oidcPublicURL, oidcToken, "", oidcPublicURL, oidcToken},
		{"other origin", "https://other.example/-/oidc/token?api-version=2.0", oidcToken, "http://ermite.bnema.dev:8088", "https://other.example/-/oidc/token?api-version=2.0", oidcToken},
		{"url only", oidcPublicURL, "", "http://ermite.bnema.dev:8088", "", ""},
		{"token only", "", oidcToken, "http://ermite.bnema.dev:8088", "", ""},
		{"none", "", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, simpleScript("x"))
			job := testJob("j1")
			job.Contexts = publicContexts
			job.IdTokenUrl, job.IdTokenRequestToken = tc.url, tc.token
			h.srv.jobs = []*ermite.Job{job}
			h.runner.Cfg.GuestForgeOrigin = tc.guest
			var spec *execpb.JobSpec
			inner := h.driver.script
			h.driver.script = func(t *testing.T, c *execstream.Conn, run *execpb.Run, cn <-chan *execpb.Cancel) {
				spec = run.Job
				inner(t, c, run, cn)
			}
			h.run(t)
			eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
			if spec == nil {
				t.Fatal("no Run")
			}
			if spec.GetIdTokenUrl() != tc.wantURL || spec.GetIdTokenRequestToken() != tc.wantToken {
				t.Errorf("got %q %q, want %q %q", spec.GetIdTokenUrl(), spec.GetIdTokenRequestToken(), tc.wantURL, tc.wantToken)
			}
		})
	}
}

func TestRewriteOrigin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://ermite.bnema.dev/-/oidc/token?api-version=2.0", "http://g:8088/-/oidc/token?api-version=2.0"},
		{"https://ermite.bnema.dev:443?a=b", "http://g:8088?a=b"},
		{"https://ermite.bnema.dev", "http://g:8088"},
		{"https://other.example/x", "https://other.example/x"},
		{"https://u@ermite.bnema.dev/x", "https://u@ermite.bnema.dev/x"},
	} {
		if got, _ := rewriteOrigin(tc.in, "https://ermite.bnema.dev", "http://g:8088"); got != tc.want {
			t.Errorf("rewriteOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
