// SPDX-License-Identifier: MIT

package resultssvc

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// jwtAuth is the Auth of the tests: HS256 three-segment JWTs as the runner
// mints them (claims j, exp, iat, scp), and blob MACs under their own domain.
type jwtAuth struct {
	key []byte
	now func() time.Time
}

func (a *jwtAuth) mac(domain string, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, a.key)
	h.Write([]byte(domain))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write(p)
	}
	return h.Sum(nil)
}

func (a *jwtAuth) tokenKey() []byte { return a.mac("token") }

// mint makes a runtime token of job with the scopes and a life of ttl.
func (a *jwtAuth) mint(job, scp string, ttl time.Duration) string {
	b64 := base64.RawURLEncoding
	now := a.now()
	h := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	c, _ := json.Marshal(map[string]any{"j": job, "scp": scp, "iat": now.Unix(), "exp": now.Add(ttl).Unix()})
	p := b64.EncodeToString(c)
	m := hmac.New(sha256.New, a.tokenKey())
	m.Write([]byte(h + "." + p))
	return h + "." + p + "." + b64.EncodeToString(m.Sum(nil))
}

func (a *jwtAuth) RuntimeToken(tok string) (string, string, time.Time, error) {
	bad := errors.New("invalid token")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", "", time.Time{}, bad
	}
	m := hmac.New(sha256.New, a.tokenKey())
	m.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, m.Sum(nil)) {
		return "", "", time.Time{}, bad
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		J   string `json:"j"`
		Scp string `json:"scp"`
		Exp int64  `json:"exp"`
	}
	if err != nil || json.Unmarshal(pb, &c) != nil || a.now().Unix() >= c.Exp {
		return "", "", time.Time{}, bad
	}
	return c.J, c.Scp, time.Unix(c.Exp, 0), nil
}

func (a *jwtAuth) BlobMAC(parts ...[]byte) []byte { return a.mac("blob", parts...) }

// genericRead is the scope that GitHub puts before Actions.Results.
const genericRead = "Actions.GenericRead:00000000-0000-0000-0000-000000000000"

// rig is a Service in front of a fake ermite.
type rig struct {
	t     *testing.T
	fake  *fakeErmite
	url   string
	auth  *jwtAuth
	clk   *clock
	svc   *Service
	nJobs int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clk := &clock{t: time.Now()}
	f := newFakeErmite()
	auth := &jwtAuth{key: []byte("test key"), now: clk.now}
	return &rig{t: t, fake: f, url: f.serve(t), auth: auth, clk: clk, svc: New(Config{Auth: auth, Now: clk.now})}
}

// tjob is a job of the rig: its Job, its runtime token and its channel.
type tjob struct {
	Job
	token string
	srv   *httptest.Server
}

// job registers a job in the fake ermite and serves its channel.
func (r *rig) job(run int64, id string, attempt int32) *tjob {
	r.t.Helper()
	r.nJobs++
	et := "ermitejob_" + strconv.Itoa(r.nJobs)
	r.fake.addJob(et, run, id, attempt)
	j := Job{ID: id, RunID: run, Attempt: attempt, Client: client(r.url, et)}
	srv := httptest.NewServer(r.svc.Handler(j))
	r.t.Cleanup(srv.Close)
	return &tjob{Job: j, token: r.auth.mint(id, genericRead+" "+Scope(run, id, attempt), time.Hour), srv: srv}
}

// twirp posts a Twirp request with the job's token and returns the status and
// the body.
func (j *tjob) twirp(t *testing.T, method, body string) (int, []byte) {
	t.Helper()
	return j.twirpWith(t, j.token, method, body)
}

func (j *tjob) twirpWith(t *testing.T, token, method, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, j.srv.URL+TwirpPath+method, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type %q", method, ct)
	}
	return res.StatusCode, b
}

// do sends a request and returns the response with its body read.
func do(t *testing.T, method, url string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// create calls CreateArtifact and returns the signed upload URL.
func (j *tjob) create(t *testing.T, name, mime string) string {
	t.Helper()
	body := `{"workflow_run_backend_id":"x","workflow_job_run_backend_id":"y","name":"` + name + `","version":7`
	if mime != "" {
		body += `,"mime_type":"` + mime + `"`
	}
	st, b := j.twirp(t, "CreateArtifact", body+"}")
	var out struct {
		OK  bool   `json:"ok"`
		URL string `json:"signedUploadUrl"`
	}
	if st != http.StatusOK || json.Unmarshal(b, &out) != nil || !out.OK {
		t.Fatalf("CreateArtifact: %d %s", st, b)
	}
	return out.URL
}

// upload uploads content as blocks of blockSize and finalizes it; it returns
// the artifact id.
func (j *tjob) upload(t *testing.T, name, mime string, content []byte, blockSize int) string {
	t.Helper()
	u := j.create(t, name, mime)
	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><BlockList>`)
	for i := 0; i == 0 || i*blockSize < len(content); i++ {
		part := content[i*blockSize : min(len(content), (i+1)*blockSize)]
		id := base64.StdEncoding.EncodeToString([]byte(name + "-block-" + strconv.Itoa(i)))
		res, b := do(t, http.MethodPut, u+"&comp=block&blockid="+id, bytes.NewReader(part))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("Put Block: %d %s", res.StatusCode, b)
		}
		list.WriteString("<Latest>" + id + "</Latest>")
	}
	list.WriteString("</BlockList>")
	res, b := do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader(list.String()))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("Put Block List: %d %s", res.StatusCode, b)
	}
	return j.finalize(t, name, content)
}

func (j *tjob) finalize(t *testing.T, name string, content []byte) string {
	t.Helper()
	st, b := j.twirp(t, "FinalizeArtifact", `{"workflow_run_backend_id":"x","workflow_job_run_backend_id":"y","name":"`+name+
		`","size":"`+strconv.Itoa(len(content))+`","hash":"`+digestOf(content)+`"}`)
	var out struct {
		OK bool   `json:"ok"`
		ID string `json:"artifactId"`
	}
	if st != http.StatusOK || json.Unmarshal(b, &out) != nil || !out.OK || out.ID == "" {
		t.Fatalf("FinalizeArtifact: %d %s", st, b)
	}
	return out.ID
}

// signedURL calls GetSignedArtifactURL.
func (j *tjob) signedURL(t *testing.T, name string) string {
	t.Helper()
	st, b := j.twirp(t, "GetSignedArtifactURL", `{"workflow_run_backend_id":"x","workflow_job_run_backend_id":"uploader","name":"`+name+`"}`)
	var out struct {
		URL string `json:"signedUrl"`
	}
	if st != http.StatusOK || json.Unmarshal(b, &out) != nil || out.URL == "" {
		t.Fatalf("GetSignedArtifactURL: %d %s", st, b)
	}
	return out.URL
}
