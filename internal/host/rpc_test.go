// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"
)

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		url      string
		insecure bool
		want     string // "" = accepted
	}{
		{"https://ermite.example.org", false, ""},
		{"https://ermite.example.org:8443/base", false, ""},
		{"http://localhost:8080", false, ""},
		{"http://LOCALHOST:8080", false, ""},
		{"http://127.0.0.1:8080", false, ""},
		{"http://[::1]:8080", false, ""},
		{"http://ermite.example.org", false, "must be https"},
		{"http://10.0.0.5:8080", false, "must be https"},
		{"http://localhost.evil.example", false, "must be https"},
		{"http://127.0.0.1.evil.example", false, "must be https"},
		{"http://ermite.example.org", true, ""},
		{"ftp://ermite.example.org", false, "scheme must be https"},
		{"ermite.example.org", false, "not a valid URL"},
		{"https://user:pw@ermite.example.org", false, "credentials"},
		{"", false, "not a valid URL"},
	} {
		err := CheckURL(tc.url, tc.insecure)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%q: %v", tc.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q (insecure=%v): %v, want %q", tc.url, tc.insecure, err, tc.want)
		}
	}
}

// The client does not follow a redirect: the token must not reach the target,
// whoever it is.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	var leaked atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		leaked.Store(r.Header.Get("Authorization"))
		w.WriteHeader(200)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := NewClient(redirector.URL, "ermiterun_secret", nil)
	_, err := c.FetchJob(context.Background(), connect.NewRequest(&ermite.FetchJobRequest{}))
	if err == nil {
		t.Fatal("a redirect answered a call")
	}
	if hits.Load() != 0 {
		t.Fatalf("the redirect was followed (%d hits), the token was sent to the target: %v", hits.Load(), leaked.Load())
	}
}

// The token is not sent over plain http to a host that is not loopback.
func TestClientRefusesToSendTheTokenInClear(t *testing.T) {
	var hits atomic.Int32
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { hits.Add(1); return nil, nil })
	c := NewClient("http://ermite.example.org", "ermiterun_secret", rt)
	_, err := c.FetchJob(context.Background(), connect.NewRequest(&ermite.FetchJobRequest{}))
	if err == nil || !strings.Contains(err.Error(), "must be https") || hits.Load() != 0 {
		t.Fatalf("hits=%d err=%v", hits.Load(), err)
	}
	// The explicit flag allows it.
	c = NewInsecureClient("http://ermite.example.org", "ermiterun_secret", rt)
	_, _ = c.FetchJob(context.Background(), connect.NewRequest(&ermite.FetchJobRequest{}))
	if hits.Load() != 1 {
		t.Fatal("the insecure client did not send the request")
	}
}

// The API lives under /-/api of the base URL, which may carry a path prefix
// and a trailing slash.
func TestClientCallsTheAPIPath(t *testing.T) {
	for base, want := range map[string]string{
		"https://h":         "/-/api/ermite.RunnerService/FetchJob",
		"https://h/":        "/-/api/ermite.RunnerService/FetchJob",
		"https://h/ermite":  "/ermite/-/api/ermite.RunnerService/FetchJob",
		"https://h/ermite/": "/ermite/-/api/ermite.RunnerService/FetchJob",
	} {
		var got string
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.URL.Path
			return nil, context.Canceled
		})
		_, _ = NewClient(base, "ermiterun_secret", rt).FetchJob(context.Background(), connect.NewRequest(&ermite.FetchJobRequest{}))
		if got != want {
			t.Errorf("%s: called %q, want %q", base, got, want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoadConfigChecksTheURL(t *testing.T) {
	env := map[string]string{EnvURL: "http://ermite.example.org"}
	get := func(k string) string { return env[k] }
	if _, err := LoadConfig(get, "h", "/s", 1, ""); err == nil || !strings.Contains(err.Error(), EnvURL) {
		t.Fatalf("%v", err)
	}
	env[EnvInsecureHTTP] = "1"
	c, err := LoadConfig(get, "h", "/s", 1, "")
	if err != nil || !c.InsecureHTTP {
		t.Fatalf("%+v %v", c, err)
	}
	env[EnvInsecureHTTP] = "maybe"
	if _, err := LoadConfig(get, "h", "/s", 1, ""); err == nil {
		t.Fatal("a bad value was accepted")
	}
}

func TestResolveRegisterToken(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if _, err := ResolveRegisterToken("", false, strings.NewReader(""), get); err == nil || !strings.Contains(err.Error(), EnvRegisterToken) {
		t.Fatalf("%v", err)
	}
	if tok, _ := ResolveRegisterToken("from-flag", false, nil, get); tok != "from-flag" {
		t.Fatal(tok)
	}
	env[EnvRegisterToken] = " from-env \n"
	if tok, _ := ResolveRegisterToken("from-flag", false, nil, get); tok != "from-env" {
		t.Fatalf("the environment must win over the flag: %q", tok)
	}
	if tok, _ := ResolveRegisterToken("from-flag", true, strings.NewReader("from-stdin\n"), get); tok != "from-stdin" {
		t.Fatalf("standard input must win: %q", tok)
	}
	if _, err := ResolveRegisterToken("x", true, strings.NewReader("  \n"), get); err == nil {
		t.Fatal("an empty standard input was accepted")
	}
}
