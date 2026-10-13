// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseGuestForge(t *testing.T) {
	ok := map[string]struct {
		insecure     bool
		host, origin string
		port         uint16
	}{
		"https://ermite.example.org":      {false, "ermite.example.org", "https://ermite.example.org", 443},
		"https://Ermite.Example.org:8443": {false, "ermite.example.org", "https://ermite.example.org:8443", 8443},
		"https://ermite.example.org/":     {false, "ermite.example.org", "https://ermite.example.org", 443},
		"https://ermite.example.org.":     {false, "ermite.example.org", "https://ermite.example.org", 443},
		"http://ermite.bnema.dev:8088":    {true, "ermite.bnema.dev", "http://ermite.bnema.dev:8088", 8088},
		"http://forge":                    {true, "forge", "http://forge", 80},
		"https://a-b.c1.test:65535":       {false, "a-b.c1.test", "https://a-b.c1.test:65535", 65535},
	}
	for raw, w := range ok {
		g, err := ParseGuestForge(raw, w.insecure)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if g.Host != w.host || g.Port != w.port || g.Origin != w.origin {
			t.Errorf("%s: %+v", raw, g)
		}
	}
	bad := map[string]string{
		"http://ermite.bnema.dev:8088":                 "INSECURE_HTTP", // plain http needs the opt-in
		"https://10.0.0.1":                             "IP address",
		"https://10.0.0.1:8443":                        "IP address",
		"https://[::1]":                                "IP address",
		"https://[2001:db8::1]:8443":                   "IP address",
		"https://2130706433":                           "IP address",
		"https://127.1":                                "IP address",
		"https://*.example.org":                        "valid host name",
		"https://exa_mple.org":                         "valid host name",
		"https://-a.example.org":                       "valid host name",
		"https://a..example.org":                       "valid host name",
		"https://bücher.example":                       "valid host name",
		"https://" + strings.Repeat("a", 64) + ".test": "valid host name",
		"https://ermite.example.org:0":                 "1-65535",
		"https://ermite.example.org:65536":             "",
		"https://ermite.example.org:99999":             "",
		"https://ermite.example.org/sub":               "bare origin",
		"https://ermite.example.org/?q=1":              "bare origin",
		"https://u:p@ermite.example.org":               "bare origin",
		"ftp://ermite.example.org":                     "http or https",
		"ermite.example.org":                           "valid URL",
		"":                                             "valid URL",
	}
	for raw, frag := range bad {
		if _, err := ParseGuestForge(raw, false); err == nil || !strings.Contains(err.Error(), frag) || !strings.Contains(err.Error(), EnvGuestForge) {
			t.Errorf("%q: %v (want an error with %q)", raw, err, frag)
		}
	}
	// With the opt-in for plain http, the other refusals still apply.
	for _, raw := range []string{"http://10.0.0.1:8088", "http://localhost:99999", "http://[::1]:8088"} {
		if _, err := ParseGuestForge(raw, true); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestResolveGuestForge(t *testing.T) {
	ctx := context.Background()
	var asked string
	lookup := func(_ context.Context, h string) ([]string, error) { asked = h; return []string{"127.0.0.1"}, nil }
	g, err := ResolveGuestForge(ctx, "http://ermite.bnema.dev:8088", true, lookup)
	if err != nil || g.Port != 8088 || asked != "ermite.bnema.dev" {
		t.Fatalf("%+v %v %q", g, err, asked)
	}
	_, err = ResolveGuestForge(ctx, "https://nx.example.org", false, func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	})
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("%v", err)
	}
	_, err = ResolveGuestForge(ctx, "https://empty.example.org", false, func(context.Context, string) ([]string, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("%v", err)
	}
	called := false
	_, err = ResolveGuestForge(ctx, "https://10.0.0.1", false, func(context.Context, string) ([]string, error) { called = true; return nil, nil })
	if err == nil || called {
		t.Fatalf("an IP literal was looked up or accepted: %v", err)
	}
}

func TestGuestForgeNilIsOff(t *testing.T) {
	var g *GuestForge
	if g.DNSName() != "" || g.TCPPort() != 0 {
		t.Fatal("nil must mean off")
	}
	g, _ = ParseGuestForge("https://ermite.example.org:8443", false)
	if g.Warning() != "guest forge access: sandboxes may reach https://ermite.example.org:8443 on the host" {
		t.Fatal(g.Warning())
	}
}

func TestLoadConfigGuestForge(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvGuestForge {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]bool{"": false, "0": false, "no": false, "1": true, " true ": true, "YES": true} {
		c, err := LoadConfig(get(v))
		if err != nil || c.GuestForgeEnabled != want {
			t.Errorf("%q: %v %v", v, c.GuestForgeEnabled, err)
		}
	}
	if _, err := LoadConfig(get("on")); err == nil || !strings.Contains(err.Error(), EnvGuestForge) {
		t.Errorf("a bad value was accepted: %v", err)
	}
}
