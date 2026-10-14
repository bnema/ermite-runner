// SPDX-License-Identifier: MIT

package host

import (
	"encoding/json"
	"strings"
	"testing"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

const publicContexts = `{"github":{"job":"","ref":"refs/heads/main","repository":"octo/hello","repositoryUrl":"git://ermite.bnema.dev/octo/hello.git",` +
	`"server_url":"https://ermite.bnema.dev","api_url":"https://ermite.bnema.dev/-/github/api","graphql_url":"https://ermite.bnema.dev/-/github/api/graphql",` +
	`"event":{"compare":"https://ermite.bnema.dev/octo/hello/compare/a...b","repository":{"html_url":"https://ermite.bnema.dev/octo/hello"}},` +
	`"note":"a & <b>"},"needs":{},"vars":{"U":"https://ermite.bnema.dev/x"}}`

func TestRewriteServerURLs(t *testing.T) {
	got, err := RewriteServerURLs(publicContexts, "http://ermite.bnema.dev:8088")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		`"server_url":"https://ermite.bnema.dev"`, `"server_url":"http://ermite.bnema.dev:8088"`,
		`"api_url":"https://ermite.bnema.dev/-/github/api"`, `"api_url":"http://ermite.bnema.dev:8088/-/github/api"`,
		`"graphql_url":"https://ermite.bnema.dev/-/github/api/graphql"`, `"graphql_url":"http://ermite.bnema.dev:8088/-/github/api/graphql"`,
	).Replace(publicContexts)
	// Only the three fields change: same bytes everywhere else (key order, the
	// event payload as recorded, other URLs, escaping).
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if !json.Valid([]byte(got)) {
		t.Fatal("invalid JSON")
	}
}

func TestRewriteServerURLsMatchesOriginsOnly(t *testing.T) {
	const target = "http://forge.internal:8088"
	cases := map[string]struct {
		in   string
		same bool
	}{
		"other origin is untouched":   {`{"github":{"server_url":"https://ermite.bnema.dev","api_url":"https://api.example.org/x","graphql_url":"https://ermite.bnema.dev:8443/g"}}`, false},
		"host prefix is not a match":  {`{"github":{"server_url":"https://ermite.bnema.dev","api_url":"https://ermite.bnema.dev.evil.test/x","graphql_url":"https://xermite.bnema.dev/g"}}`, false},
		"no server_url":               {`{"github":{"api_url":"https://ermite.bnema.dev/-/github/api"}}`, true},
		"no github":                   {`{"vars":{"a":"b"}}`, true},
		"empty server_url":            {`{"github":{"server_url":"","api_url":"https://ermite.bnema.dev/a"}}`, true},
		"non-string values":           {`{"github":{"server_url":"https://ermite.bnema.dev","api_url":null,"graphql_url":3}}`, false},
		"already the target":          {`{"github":{"server_url":"http://forge.internal:8088","api_url":"http://forge.internal:8088/a"}}`, true},
		"credentials are not origins": {`{"github":{"server_url":"https://ermite.bnema.dev","api_url":"https://u:p@ermite.bnema.dev/a"}}`, false},
	}
	for name, c := range cases {
		got, err := RewriteServerURLs(c.in, target)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.same && got != c.in {
			t.Errorf("%s: changed:\n%s", name, got)
		}
		var v struct {
			GitHub map[string]any `json:"github"`
		}
		if err := json.Unmarshal([]byte(got), &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		switch name {
		case "other origin is untouched", "host prefix is not a match", "credentials are not origins":
			if v.GitHub["server_url"] != target {
				t.Errorf("%s: server_url %v", name, v.GitHub["server_url"])
			}
			if u, _ := v.GitHub["api_url"].(string); strings.HasPrefix(u, target) {
				t.Errorf("%s: api_url rewritten: %s", name, u)
			}
			if u, _ := v.GitHub["graphql_url"].(string); strings.HasPrefix(u, target) {
				t.Errorf("%s: graphql_url rewritten: %s", name, u)
			}
		case "non-string values":
			if v.GitHub["server_url"] != target || v.GitHub["api_url"] != nil || v.GitHub["graphql_url"] != float64(3) {
				t.Errorf("%s: %v", name, v.GitHub)
			}
		}
	}
}

func TestRewriteServerURLsSpellings(t *testing.T) {
	// Case of scheme and host and a default port do not change the origin.
	in := `{"github":{"server_url":"HTTPS://Ermite.BNEMA.dev:443/","api_url":"https://ermite.bnema.dev/-/github/api?x=1#f"}}`
	got, err := RewriteServerURLs(in, "https://forge.test")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"github":{"server_url":"https://forge.test/","api_url":"https://forge.test/-/github/api?x=1#f"}}` {
		t.Fatal(got)
	}
	// Off: the contexts are returned as received, valid or not.
	if got, err := RewriteServerURLs("not json", ""); err != nil || got != "not json" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"not json", `[1]`, `{"github":[1]}`, `{"github":{}} x`} {
		if _, err := RewriteServerURLs(bad, "http://a.test"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOriginOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://Ermite.example/x": "https://ermite.example", "http://a.test:80": "http://a.test",
		"http://a.test:8088/p": "http://a.test:8088", "https://a.test:443": "https://a.test",
		"ftp://a.test": "", "a.test": "", "https://u@a.test": "", "": "",
		// GO-012: an IPv6 host stays in brackets, and the host "::1:8443" is not the
		// host "::1" on port 8443.
		"https://[::1]:8443/x": "https://[::1]:8443", "https://[::1:8443]/x": "https://[::1:8443]", "https://[::1]:443/": "https://[::1]",
		"http://[2001:DB8::1]:80": "http://[2001:db8::1]",
	} {
		if got := originOf(in); got != want {
			t.Errorf("originOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The executor receives the rewritten contexts when the access is on, and the
// recorded ones when it is off.
func TestJobContextsAreRewrittenOnlyWithGuestForge(t *testing.T) {
	for _, on := range []bool{true, false} {
		h := newHarness(t, simpleScript("x"))
		job := testJob("j1")
		job.Contexts = publicContexts
		h.srv.jobs = []*ermite.Job{job}
		if on {
			h.runner.Cfg.GuestForgeOrigin = "http://ermite.bnema.dev:8088"
		}
		var seen string
		inner := h.driver.script
		h.driver.script = func(t *testing.T, c *execstream.Conn, run *execpb.Run, cn <-chan *execpb.Cancel) {
			seen = run.Job.Contexts
			inner(t, c, run, cn)
		}
		h.run(t)
		eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
		if got := strings.Contains(seen, `"server_url":"http://ermite.bnema.dev:8088"`); got != on {
			t.Errorf("guest forge %v: contexts %s", on, seen)
		}
		if !on && seen != publicContexts {
			t.Errorf("contexts changed while off: %s", seen)
		}
	}
}

// GO-012: guest forge access on an IPv6 address. The URLs of the contexts are
// rewritten to the runner's origin, the brackets and the port kept, and the
// origin of the other host with the look-alike spelling is not touched.
func TestRewriteServerURLsIPv6(t *testing.T) {
	in := `{"github":{"server_url":"https://[::1]:8443","api_url":"https://[::1]:8443/-/github/api","graphql_url":"https://[::1:8443]/x"}}`
	got, err := RewriteServerURLs(in, "http://[fd00::2]:8088")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"github":{"server_url":"http://[fd00::2]:8088","api_url":"http://[fd00::2]:8088/-/github/api","graphql_url":"https://[::1:8443]/x"}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
