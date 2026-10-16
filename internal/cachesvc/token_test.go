// SPDX-License-Identifier: MIT

package cachesvc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The runtime token is a JWT as @actions/artifact reads it: three base64url
// segments, a JSON header and claims that jwt-decode accepts, and an scp claim
// whose Actions.Results entry splits on ':' into the run and job ids
// (toolkit packages/artifact/src/internal/shared/util.ts:21-75). Its signature
// is a real HS256 under the key derived for tokens.
func TestRuntimeTokenIsAJWT(t *testing.T) {
	r := newRig(t, 1<<20)
	tok := r.svc.Token("j1", Scope{Repository: repoA, WriteRef: main_, ReadRefs: []string{main_}}, time.Hour, "Actions.Results:42:abc")
	seg := strings.Split(tok, ".")
	if len(seg) != 3 {
		t.Fatalf("%d segments", len(seg))
	}
	var hdr map[string]string
	if b, err := b64.DecodeString(seg[0]); err != nil || json.Unmarshal(b, &hdr) != nil || hdr["alg"] != "HS256" || hdr["typ"] != "JWT" {
		t.Fatalf("header %q: %v", seg[0], hdr)
	}
	var c struct {
		Scp string `json:"scp"`
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
		J   string `json:"j"`
	}
	if b, err := b64.DecodeString(seg[1]); err != nil || json.Unmarshal(b, &c) != nil {
		t.Fatalf("claims %q", seg[1])
	}
	now := r.clk.now().Unix()
	if c.J != "j1" || c.Iat != now || c.Exp != now+3600 {
		t.Fatalf("claims %+v", c)
	}
	scopes := strings.Fields(c.Scp)
	if len(scopes) != 2 || scopes[0] != genericReadScope || scopes[1] != "Actions.Results:42:abc" {
		t.Fatalf("scp %q", c.Scp)
	}
	if p := strings.Split(scopes[1], ":"); len(p) != 3 || p[1] != "42" || p[2] != "abc" {
		t.Fatalf("results scope %q", scopes[1])
	}
	h := hmac.New(sha256.New, r.svc.sign.mac("token"))
	h.Write([]byte(seg[0] + "." + seg[1]))
	if got, _ := b64.DecodeString(seg[2]); !hmac.Equal(got, h.Sum(nil)) {
		t.Fatal("not an HS256 signature under the token key")
	}

	job, scp, exp, err := r.svc.RuntimeToken(tok)
	if err != nil || job != "j1" || scp != c.Scp || exp.Unix() != c.Exp {
		t.Fatalf("RuntimeToken: %q %q %v %v", job, scp, exp, err)
	}
	// A cache-only token has no results scope; a results-only one is accepted
	// by RuntimeToken but not by the cache (TestBadTokens).
	if _, scp, _, _ := r.svc.RuntimeToken(r.job("j2", repoA, main_, main_).token); strings.Contains(scp, "Actions.Results") {
		t.Fatalf("cache-only token with %q", scp)
	}
	if job, _, _, err := r.svc.RuntimeToken(r.svc.Token("j3", Scope{}, time.Hour, "Actions.Results:1:x")); err != nil || job != "j3" {
		t.Fatalf("results-only token: %q %v", job, err)
	}
	if _, _, _, err := r.svc.RuntimeToken(tok[:len(tok)-2]); err == nil {
		t.Fatal("bad signature accepted")
	}
	// Blob signatures have a domain of their own.
	if hmac.Equal(r.svc.BlobMAC(), r.svc.sign.mac("token")) || hmac.Equal(r.svc.BlobMAC([]byte("x")), r.svc.sign.mac("archive", []byte("x"))) {
		t.Fatal("blob MAC shares a domain")
	}
}

// With WithResults, the channel sends the cache API to the cache and every
// other path to the results handler.
func TestChannelRoutesResults(t *testing.T) {
	r := newRig(t, 1<<20)
	results := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		_, _ = io.WriteString(w, "results "+req.URL.Path+" "+string(b))
	})
	c := channel(t, r.svc, "er0000000a", "job-a", WithResults(results))
	tok := r.job("job-a", repoA, main_, main_).token
	if got := lookupStatus(t, c, tok); got != http.StatusNoContent {
		t.Fatalf("cache lookup: %d", got)
	}
	resp, err := c.Post("http://cache/twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "results /twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts {}" {
		t.Fatalf("results: %d %q", resp.StatusCode, b)
	}
	// Without it, those paths are not found.
	plain := channel(t, r.svc, "er0000000b", "job-b")
	resp, err = plain.Post("http://cache/twirp/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no results handler: %d", resp.StatusCode)
	}
}
