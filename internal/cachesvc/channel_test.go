// SPDX-License-Identifier: MIT

package cachesvc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// channel serves svc on a Unix socket as the channel of sandbox id and returns
// an HTTP client that can reach that socket only.
func channel(t *testing.T, svc *Service, id, job string, opts ...ServeOption) *http.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "c")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Serve(id, ln, job, opts...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Stop(id) })
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
}

func lookupStatus(t *testing.T, c *http.Client, token string) int {
	t.Helper()
	req, err := http.NewRequest("GET", "http://cache"+APIPath+"cache?keys=k&version="+v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A channel is the listener of one sandbox. The token rules are the service's
// (a missing or foreign token is a 403, a valid one is served), and on top of
// them a channel serves the job of its sandbox only: the token of another job,
// even a valid one, gets nothing, so a token that leaves its sandbox is useless
// on every other sandbox's channel.
func TestChannelServesItsOwnJobOnly(t *testing.T) {
	r := newRig(t, 1<<20)
	a := channel(t, r.svc, "er0000000a", "job-a")
	b := channel(t, r.svc, "er0000000b", "job-b")
	tokA := r.job("job-a", repoA, main_, main_).token
	tokB := r.job("job-b", repoA, main_, main_).token

	if got := lookupStatus(t, a, tokA); got != http.StatusNoContent {
		t.Fatalf("A's token on A's channel: %d", got)
	}
	if got := lookupStatus(t, b, tokB); got != http.StatusNoContent {
		t.Fatalf("B's token on B's channel: %d", got)
	}
	if got := lookupStatus(t, b, tokA); got != http.StatusForbidden {
		t.Fatalf("A's token on B's channel: %d", got)
	}
	if got := lookupStatus(t, a, tokB); got != http.StatusForbidden {
		t.Fatalf("B's token on A's channel: %d", got)
	}
	if got := lookupStatus(t, a, ""); got != http.StatusForbidden {
		t.Fatalf("no token: %d", got)
	}
	if got := lookupStatus(t, a, "forged.token"); got != http.StatusForbidden {
		t.Fatalf("forged token: %d", got)
	}
}

// archiveStatus requests the archive at loc (a lookup's archiveLocation) on the
// channel c, with the given method, and returns the status and the body.
func archiveStatus(t *testing.T, c *http.Client, method, loc string) (int, string) {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, "http://cache"+u.RequestURI(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The signed URL of an archive is bound to the job it was issued to: it works on
// the channel of that job and is refused, for GET and HEAD, on the channel of
// any other, even for a job of the same repository and scope.
func TestChannelArchiveURLBoundToJob(t *testing.T) {
	r := newRig(t, 1<<20)
	a := channel(t, r.svc, "er0000000a", "job-a")
	b := channel(t, r.svc, "er0000000b", "job-b")
	r.job("job-a", repoA, main_, main_).save("k", v1, []byte("secret-a"))

	req, _ := http.NewRequest("GET", "http://cache"+APIPath+"cache?keys=k&version="+v1, nil)
	req.Header.Set("Authorization", "Bearer "+r.job("job-a", repoA, main_, main_).token)
	resp, err := a.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	err = json.NewDecoder(resp.Body).Decode(&e)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup: %d %v", resp.StatusCode, err)
	}

	if code, body := archiveStatus(t, a, "GET", e.ArchiveLocation); code != 200 || body != "secret-a" {
		t.Fatalf("GET on the issuing channel: %d %q", code, body)
	}
	if code, _ := archiveStatus(t, a, "HEAD", e.ArchiveLocation); code != 200 {
		t.Fatalf("HEAD on the issuing channel: %d", code)
	}
	if code, body := archiveStatus(t, b, "GET", e.ArchiveLocation); code != http.StatusForbidden || strings.Contains(body, "secret") {
		t.Fatalf("GET on another job's channel: %d %q", code, body)
	}
	if code, _ := archiveStatus(t, b, "HEAD", e.ArchiveLocation); code != http.StatusForbidden {
		t.Fatalf("HEAD on another job's channel: %d", code)
	}
	// Rewriting the job in the URL does not help: the signature covers it.
	u, _ := url.Parse(e.ArchiveLocation)
	q := u.Query()
	q.Set("job", "job-b")
	u.RawQuery = q.Encode()
	if code, _ := archiveStatus(t, b, "GET", u.String()); code != http.StatusForbidden {
		t.Fatalf("URL with the job rewritten: %d", code)
	}
}

// The scope rules do not depend on the channel: a read-only job cannot reserve,
// a job writes to its own ref and never reads another repository's entries.
func TestChannelKeepsScopeRules(t *testing.T) {
	r := newRig(t, 1<<20)
	w := channel(t, r.svc, "er0000000a", "job-w")
	ro := channel(t, r.svc, "er0000000b", "job-ro")
	other := channel(t, r.svc, "er0000000c", "job-o")
	r.job("job-w", repoA, main_, main_).save("k", v1, []byte("data"))

	check := func(c *http.Client, jobID, repo, write string, wantHit bool) {
		t.Helper()
		tok := r.svc.Token(jobID, Scope{Repository: repo, WriteRef: write, ReadRefs: []string{main_}}, time.Hour)
		got := lookupStatus(t, c, tok)
		if wantHit != (got == http.StatusOK) || (!wantHit && got != http.StatusNoContent) {
			t.Fatalf("%s: lookup %d, want hit=%v", jobID, got, wantHit)
		}
	}
	check(w, "job-w", repoA, main_, true)
	check(ro, "job-ro", repoA, "", true)
	check(other, "job-o", "bnema/other", main_, false)

	roTok := r.svc.Token("job-ro", Scope{Repository: repoA, ReadRefs: []string{main_}}, time.Hour)
	req, _ := http.NewRequest("POST", "http://cache"+APIPath+"caches", nil)
	req.Header.Set("Authorization", "Bearer "+roTok)
	resp, err := ro.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a read-only job reserved: %d", resp.StatusCode)
	}
}

// The connection caps apply to a channel as to any listener, and Stop closes
// the listener and the open connections of that sandbox only.
func TestChannelCapsAndStop(t *testing.T) {
	r := newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, ConnsPerListener: 1})
	sockA, sockB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for id, sock := range map[string]string{"er0000000a": sockA, "er0000000b": sockB} {
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.svc.Serve(id, ln, "j-"+id); err != nil {
			t.Fatal(err)
		}
	}
	held, err := net.Dial("unix", sockA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	// The one slot of A is taken once the server has accepted `held`: a second
	// connection is then closed at once. B has its own share.
	waitUntil(t, func() bool {
		c, err := net.Dial("unix", sockA)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, err = c.Read(make([]byte, 1))
		return err != nil && !isTimeout(err)
	})
	other, err := net.Dial("unix", sockB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	r.svc.Stop("er0000000a")
	_ = held.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := held.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("the open connection of a stopped channel: %v", err)
	}
	if c, err := net.Dial("unix", sockA); err == nil {
		_ = c.Close()
		t.Fatal("a stopped channel still accepts")
	}
	// B is not affected.
	_ = other.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := other.Read(make([]byte, 1)); err == nil || !isTimeout(err) {
		t.Fatalf("B's connection after A's stop: %v", err)
	}
	r.svc.Stop("er0000000a") // idempotent, unknown ids ignored
	r.svc.Stop("unknown")
}

// One channel per sandbox id, none once the service is closed, and CloseAll
// closes the listeners it was given.
func TestChannelServeErrorsAndCloseAll(t *testing.T) {
	r := newRig(t, 1<<20)
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Serve("er0000000a", ln, "j"); err != nil {
		t.Fatal(err)
	}
	dup, err := net.Listen("unix", filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Serve("er0000000a", dup, "j"); err == nil {
		t.Fatal("two channels for one sandbox")
	}
	if _, err := dup.Accept(); err == nil {
		t.Fatal("the refused listener was left open")
	}
	if err := r.svc.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("unix", sock); err == nil {
		_ = c.Close()
		t.Fatal("CloseAll left a channel open")
	}
	late, err := net.Listen("unix", filepath.Join(t.TempDir(), "l"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Serve("er0000000b", late, "j"); err == nil {
		t.Fatal("Serve after CloseAll")
	}
	if _, err := late.Accept(); err == nil {
		t.Fatal("the listener of a refused Serve was left open")
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached in 10s")
}
