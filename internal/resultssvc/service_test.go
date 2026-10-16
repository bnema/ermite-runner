// SPDX-License-Identifier: MIT

package resultssvc

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestJobBackendIDAndScope(t *testing.T) {
	sum := sha256.Sum256([]byte("job-1/2"))
	if got, want := JobBackendID("job-1", 2), hex.EncodeToString(sum[:])[:32]; got != want {
		t.Fatalf("JobBackendID = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(JobBackendID("a:b", 1)) {
		t.Fatal("not 32 lowercase hex characters")
	}
	if JobBackendID("job-1", 1) == JobBackendID("job-1", 2) {
		t.Fatal("attempts must differ")
	}
	if got, want := Scope(42, "job-1", 2), "Actions.Results:42:"+JobBackendID("job-1", 2); got != want {
		t.Fatalf("Scope = %q, want %q", got, want)
	}
	// The toolkit splits the scope on ':' and wants three parts.
	if n := len(strings.Split(Scope(42, "a:b", 1), ":")); n != 3 {
		t.Fatalf("scope has %d parts", n)
	}
}

// Golden request bodies, as the real upload-artifact v4.6.2 / v7.0.2 and
// download-artifact v4.3.0 sent them in the C0 spike.
const (
	goldenCreateV4   = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"build","name":"single","version":4}`
	goldenCreateV7   = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"build","name":"report.html","version":7,"mime_type":"text/html","expires_at":"2026-11-14T12:00:00.000Z"}`
	goldenFinalize   = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"build","name":"single","size":"146","hash":"%s"}`
	goldenListName   = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"test","name_filter":"single"}`
	goldenListID     = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"test","id_filter":"%s"}`
	goldenListAll    = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"test"}`
	goldenSignedURL  = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"build","name":"single"}`
	goldenDelete     = `{"workflow_run_backend_id":"11111111-aaaa-4000-8000-000000000001","workflow_job_run_backend_id":"build","name":"single"}`
	goldenBlockList1 = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><BlockList><Latest>M2VhNWQ3MWYtZGEyYi00YmQ5LThiNDAtY2Y4MGM3OTBmY2NjMDAwMDAwMDAwMDAw</Latest></BlockList>`
	goldenBlockID1   = "M2VhNWQ3MWYtZGEyYi00YmQ5LThiNDAtY2Y4MGM3OTBmY2NjMDAwMDAwMDAwMDAw"
)

func TestTwirpGolden(t *testing.T) {
	r := newRig(t)
	build := r.job(7, "build", 1)
	test := r.job(7, "test", 1)
	content := bytes.Repeat([]byte("zip!"), 37)[:146]

	st, b := build.twirp(t, "CreateArtifact", goldenCreateV4)
	var created map[string]any
	if st != http.StatusOK || json.Unmarshal(b, &created) != nil {
		t.Fatalf("create: %d %s", st, b)
	}
	if len(created) != 2 || created["ok"] != true {
		t.Fatalf("create reply: %s", b)
	}
	u, err := url.Parse(created["signedUploadUrl"].(string))
	if err != nil || u.Scheme != "http" || u.Host != strings.TrimPrefix(build.srv.URL, "http://") || u.Path != BlobPath+"upload/1" ||
		u.Query().Get("se") != strconv.FormatInt(r.clk.now().Add(time.Hour).Unix(), 10) || u.Query().Get("sig") == "" {
		t.Fatalf("signedUploadUrl: %v", created["signedUploadUrl"])
	}
	res, _ := do(t, http.MethodPut, u.String()+"&comp=block&blockid="+url.QueryEscape(goldenBlockID1), bytes.NewReader(content))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("block: %d", res.StatusCode)
	}
	res, _ = do(t, http.MethodPut, u.String()+"&comp=blocklist", strings.NewReader(goldenBlockList1))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("blocklist: %d", res.StatusCode)
	}
	st, b = build.twirp(t, "FinalizeArtifact", fmt.Sprintf(goldenFinalize, digestOf(content)))
	if st != http.StatusOK || string(b) != `{"artifactId":"1","ok":true}`+"\n" {
		t.Fatalf("finalize: %d %s", st, b)
	}

	wantItem := `{"workflowRunBackendId":"7","workflowJobRunBackendId":"` + JobBackendID("build", 1) +
		`","databaseId":"1","name":"single","size":"146","createdAt":"2026-10-15T12:00:00Z","digest":"` + digestOf(content) + `"}`
	for _, body := range []string{goldenListName, fmt.Sprintf(goldenListID, "1"), strings.Replace(fmt.Sprintf(goldenListID, "1"), `"1"`, "1", 1), goldenListAll} {
		st, b = test.twirp(t, "ListArtifacts", body)
		if st != http.StatusOK || string(b) != `{"artifacts":[`+wantItem+`]}`+"\n" {
			t.Fatalf("list %s: %d %s", body, st, b)
		}
	}
	st, b = test.twirp(t, "ListArtifacts", fmt.Sprintf(goldenListID, "2"))
	if st != http.StatusOK || string(b) != `{"artifacts":[]}`+"\n" {
		t.Fatalf("empty list: %d %s", st, b)
	}

	// The body names the uploader's job: ignored, the URL is for test's channel.
	st, b = test.twirp(t, "GetSignedArtifactURL", goldenSignedURL)
	var signed struct {
		URL string `json:"signedUrl"`
	}
	if st != http.StatusOK || json.Unmarshal(b, &signed) != nil || !strings.HasPrefix(signed.URL, test.srv.URL+BlobPath+"download/1?se=") {
		t.Fatalf("signed URL: %d %s", st, b)
	}
	res, got := do(t, http.MethodGet, signed.URL, nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("download: %d %d bytes", res.StatusCode, len(got))
	}

	st, b = build.twirp(t, "DeleteArtifact", goldenDelete)
	if st != http.StatusOK || string(b) != `{"artifactId":"1","ok":true}`+"\n" {
		t.Fatalf("delete: %d %s", st, b)
	}
	st, b = test.twirp(t, "GetSignedArtifactURL", goldenSignedURL)
	if st != http.StatusNotFound || !strings.Contains(string(b), `"code":"not_found"`) {
		t.Fatalf("signed URL of a deleted artifact: %d %s", st, b)
	}

	// v7: mime_type and expires_at reach ermite.
	st, b = build.twirp(t, "CreateArtifact", goldenCreateV7)
	if st != http.StatusOK {
		t.Fatalf("create v7: %d %s", st, b)
	}
	r.fake.mu.Lock()
	a := r.fake.arts[2].a
	r.fake.mu.Unlock()
	if a.GetMediaType() != "text/html" || !a.GetExpiresAt().AsTime().Equal(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)) || a.GetJobId() != "build" || a.GetRunId() != 7 {
		t.Fatalf("created: %v", a)
	}
	st, b = build.twirp(t, "CreateArtifact", `{"name":"x","expires_at":"tomorrow"}`)
	if st != http.StatusBadRequest || !strings.Contains(string(b), `"invalid_argument"`) {
		t.Fatalf("bad expires_at: %d %s", st, b)
	}
	st, b = build.twirp(t, "FinalizeArtifact", `{"name":"x","size":"many"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("bad size: %d %s", st, b)
	}
	st, b = build.twirp(t, "ListArtifacts", `not json`)
	if st != http.StatusBadRequest || !strings.Contains(string(b), `"malformed"`) {
		t.Fatalf("bad JSON: %d %s", st, b)
	}
}

func TestTwirpErrorMapping(t *testing.T) {
	r := newRig(t)
	j := r.job(1, "job", 1)
	cases := []struct {
		code   connect.Code
		msg    string
		status int
		twirp  string
		want   string
	}{
		{connect.CodeInvalidArgument, "bad name", 400, "invalid_argument", "bad name"},
		{connect.CodeNotFound, "gone", 404, "not_found", "gone"},
		{connect.CodeAlreadyExists, "", 409, "already_exists", msgExists},
		{connect.CodeAlreadyExists, "taken", 409, "already_exists", "taken"},
		{connect.CodeFailedPrecondition, "job ended", 412, "failed_precondition", "job ended"},
		{connect.CodeResourceExhausted, "over the 10 GiB quota", 412, "failed_precondition", "over the 10 GiB quota"},
		{connect.CodeUnauthenticated, "token", 401, "unauthenticated", "token"},
		{connect.CodePermissionDenied, "denied", 401, "unauthenticated", "denied"},
		{connect.CodeUnavailable, "down", 503, "unavailable", "down"},
		{connect.CodeDeadlineExceeded, "slow", 503, "unavailable", "slow"},
		{connect.CodeInternal, "bug", 503, "unavailable", "bug"},
		{connect.CodeUnknown, "what", 503, "unavailable", "what"},
		{connect.CodeCanceled, "stop", 503, "unavailable", "stop"},
		{connect.CodeUnimplemented, "old", 503, "unavailable", "old"},
	}
	for _, c := range cases {
		t.Run(c.code.String()+"/"+c.msg, func(t *testing.T) {
			r.fake.mu.Lock()
			r.fake.failNext["CreateJobArtifact"] = connect.NewError(c.code, errors.New(c.msg))
			r.fake.mu.Unlock()
			st, b := j.twirp(t, "CreateArtifact", goldenCreateV4)
			var e map[string]string
			if err := json.Unmarshal(b, &e); err != nil || st != c.status || e["code"] != c.twirp || e["msg"] != c.want || len(e) != 2 {
				t.Fatalf("got %d %s, want %d %s %q", st, b, c.status, c.twirp, c.want)
			}
		})
	}
}

func TestTwirpAuth(t *testing.T) {
	r := newRig(t)
	j := r.job(1, "job", 1)
	other := r.job(1, "other", 1)
	nextAttempt := Job{ID: "job", RunID: 1, Attempt: 2}
	for name, tok := range map[string]string{
		"no bearer":            "",
		"garbage":              "abc",
		"other job's token":    other.token,
		"no results scope":     r.auth.mint("job", genericRead, time.Hour),
		"scope of another run": r.auth.mint("job", Scope(2, "job", 1), time.Hour),
		"scope of next try":    r.auth.mint("job", Scope(nextAttempt.RunID, nextAttempt.ID, nextAttempt.Attempt), time.Hour),
		"scope as a prefix":    r.auth.mint("job", Scope(1, "job", 1)+"0", time.Hour),
		"forged":               (&jwtAuth{key: []byte("other"), now: r.clk.now}).mint("job", Scope(1, "job", 1), time.Hour),
		"expired":              r.auth.mint("job", Scope(1, "job", 1), -time.Second),
	} {
		st, b := j.twirpWith(t, tok, "ListArtifacts", goldenListAll)
		if st != http.StatusUnauthorized || !strings.Contains(string(b), `"code":"unauthenticated"`) {
			t.Errorf("%s: %d %s", name, st, b)
		}
	}
	if st, b := j.twirp(t, "ListArtifacts", goldenListAll); st != http.StatusOK {
		t.Fatalf("own token: %d %s", st, b)
	}
	if st, b := j.twirp(t, "Nope", "{}"); st != http.StatusNotFound || !strings.Contains(string(b), `"bad_route"`) {
		t.Errorf("unknown method: %d %s", st, b)
	}
	res, b := do(t, http.MethodGet, j.srv.URL+TwirpPath+"ListArtifacts", nil)
	if res.StatusCode != http.StatusMethodNotAllowed || !strings.Contains(string(b), `"bad_route"`) {
		t.Errorf("GET: %d %s", res.StatusCode, b)
	}
	if res, _ := do(t, http.MethodGet, j.srv.URL+"/_apis/artifactcache/cache", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("other path: %d", res.StatusCode)
	}
	st, b := j.twirp(t, "ListArtifacts", `{"name_filter":"`+strings.Repeat("x", maxTwirpBody)+`"}`)
	if st != http.StatusBadRequest {
		t.Errorf("huge body: %d %.100s", st, b)
	}
}

func checkBlobError(t *testing.T, what string, res *http.Response, body []byte, status int, code string) {
	t.Helper()
	want := `<?xml version="1.0" encoding="utf-8"?><Error><Code>` + code + `</Code><Message>` + code + `</Message></Error>`
	if res.StatusCode != status || res.Header.Get("x-ms-error-code") != code || string(body) != want {
		t.Errorf("%s: %d %s %s, want %d %s", what, res.StatusCode, res.Header.Get("x-ms-error-code"), body, status, code)
	}
}

func TestBlobUpload(t *testing.T) {
	r := newRig(t)
	j := r.job(1, "job", 1)
	other := r.job(1, "other", 1)

	t.Run("blocks", func(t *testing.T) {
		u := j.create(t, "blocks", "")
		res, b := do(t, http.MethodPut, u+"&comp=block&blockid="+url.QueryEscape(base64.StdEncoding.EncodeToString([]byte("b1"))), strings.NewReader("hello "))
		if res.StatusCode != http.StatusCreated || len(b) != 0 || res.Header.Get("Content-Length") != "0" {
			t.Fatalf("block: %d %q", res.StatusCode, b)
		}
		for _, h := range []string{"ETag", "Last-Modified", "x-ms-request-id", "x-ms-version", "x-ms-request-server-encrypted"} {
			if res.Header.Get(h) == "" {
				t.Errorf("no %s", h)
			}
		}
		// A retry of a block replaces it.
		do(t, http.MethodPut, u+"&comp=block&blockid="+base64.StdEncoding.EncodeToString([]byte("b2")), strings.NewReader("wrong"))
		do(t, http.MethodPut, u+"&comp=block&blockid="+base64.StdEncoding.EncodeToString([]byte("b2")), strings.NewReader("world"))
		list := `<BlockList><Uncommitted>` + base64.StdEncoding.EncodeToString([]byte("b1")) + `</Uncommitted><Latest>` +
			base64.StdEncoding.EncodeToString([]byte("b2")) + `</Latest><Committed>` + base64.StdEncoding.EncodeToString([]byte("b1")) + `</Committed></BlockList>`
		if res, b := do(t, http.MethodPut, u+"&comp=BlockList", strings.NewReader(list)); res.StatusCode != http.StatusCreated {
			t.Fatalf("blocklist: %d %s", res.StatusCode, b)
		}
		id, _ := strconv.ParseInt(j.finalize(t, "blocks", []byte("hello worldhello ")), 10, 64)
		if c, _ := r.fake.content(id); string(c) != "hello worldhello " {
			t.Fatalf("content %q", c)
		}
	})

	t.Run("empty block list", func(t *testing.T) {
		u := j.create(t, "empty", "")
		if res, b := do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><BlockList/>`)); res.StatusCode != http.StatusCreated {
			t.Fatalf("blocklist: %d %s", res.StatusCode, b)
		}
		id, _ := strconv.ParseInt(j.finalize(t, "empty", nil), 10, 64)
		if c, ok := r.fake.content(id); !ok || len(c) != 0 {
			t.Fatalf("content %q %v", c, ok)
		}
	})

	t.Run("put blob", func(t *testing.T) {
		u := j.create(t, "single-shot", "")
		if res, b := do(t, http.MethodPut, u, strings.NewReader("all at once")); res.StatusCode != http.StatusCreated {
			t.Fatalf("put blob: %d %s", res.StatusCode, b)
		}
		id, _ := strconv.ParseInt(j.finalize(t, "single-shot", []byte("all at once")), 10, 64)
		if c, _ := r.fake.content(id); string(c) != "all at once" {
			t.Fatalf("content %q", c)
		}
		u = j.create(t, "single-shot-empty", "")
		if res, b := do(t, http.MethodPut, u, nil); res.StatusCode != http.StatusCreated {
			t.Fatalf("empty put blob: %d %s", res.StatusCode, b)
		}
		j.finalize(t, "single-shot-empty", nil)
	})

	t.Run("refusals", func(t *testing.T) {
		u := j.create(t, "refused", "")
		pu, _ := url.Parse(u)
		q := pu.Query()
		blk := "&comp=block&blockid=" + base64.StdEncoding.EncodeToString([]byte("b"))
		tamper := func(k, v string) string {
			q2 := url.Values{"se": {q.Get("se")}, "sig": {q.Get("sig")}}
			q2.Set(k, v)
			return j.srv.URL + pu.Path + "?" + q2.Encode()
		}
		sig := []byte(q.Get("sig"))
		sig[3] ^= 1
		se, _ := strconv.ParseInt(q.Get("se"), 10, 64)
		for name, target := range map[string]string{
			"tampered signature": tamper("sig", string(sig)) + blk,
			"no signature":       j.srv.URL + pu.Path + "?se=" + q.Get("se") + blk,
			"later expiry":       tamper("se", strconv.FormatInt(se+1, 10)) + blk,
			"another artifact":   j.srv.URL + BlobPath + "upload/99?" + q.Encode() + blk,
			"other job's URL":    other.srv.URL + pu.Path + "?" + q.Encode() + blk,
			"as a download URL":  j.srv.URL + BlobPath + "download/" + strings.TrimPrefix(pu.Path, BlobPath+"upload/") + "?" + q.Encode(),
		} {
			method := http.MethodPut
			if strings.Contains(target, "/download/") {
				method = http.MethodGet
			}
			res, b := do(t, method, target, strings.NewReader("x"))
			checkBlobError(t, name, res, b, http.StatusForbidden, "AuthenticationFailed")
		}
		res, b := do(t, http.MethodPut, u+"&comp=block&blockid=%%%", strings.NewReader("x"))
		checkBlobError(t, "bad block id", res, b, http.StatusBadRequest, "InvalidQueryParameterValue")
		res, b = do(t, http.MethodPut, u+"&comp=block&blockid="+base64.StdEncoding.EncodeToString(make([]byte, 65)), strings.NewReader("x"))
		checkBlobError(t, "long block id", res, b, http.StatusBadRequest, "InvalidQueryParameterValue")
		res, b = do(t, http.MethodPut, u+"&comp=block", strings.NewReader("x"))
		checkBlobError(t, "no block id", res, b, http.StatusBadRequest, "InvalidQueryParameterValue")
		res, b = do(t, http.MethodDelete, u, nil)
		checkBlobError(t, "DELETE", res, b, http.StatusMethodNotAllowed, "UnsupportedHttpVerb")
		res, b = do(t, http.MethodPut, u+"&comp=appendblock", strings.NewReader("x"))
		checkBlobError(t, "append block", res, b, http.StatusMethodNotAllowed, "UnsupportedHttpVerb")
		res, b = do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader(`<BlockList><Latest>`+base64.StdEncoding.EncodeToString([]byte("never"))+`</Latest></BlockList>`))
		checkBlobError(t, "unknown block", res, b, http.StatusBadRequest, "InvalidBlockList")
		// The large bodies go straight to the handler: over the loopback, a
		// megabyte that the server reads in small parts can take seconds.
		res, b = direct(t, r.svc.Handler(j.Job), u+"&comp=blocklist", strings.NewReader(`<BlockList><Latest>`+strings.Repeat(`AA==</Latest><Latest>`, maxBlocks)+`AA==</Latest></BlockList>`))
		checkBlobError(t, "too many blocks", res, b, http.StatusBadRequest, "InvalidBlockList")
		res, b = do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader(`<Other/>`))
		checkBlobError(t, "not a block list", res, b, http.StatusBadRequest, "InvalidXmlDocument")
		res, b = do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader(`<BlockList><Latest>AA==`))
		checkBlobError(t, "truncated XML", res, b, http.StatusBadRequest, "InvalidXmlDocument")
		res, b = direct(t, r.svc.Handler(j.Job), u+"&comp=blocklist", io.MultiReader(strings.NewReader(`<BlockList><!--`), bytes.NewReader(make([]byte, maxBlockListBody))))
		checkBlobError(t, "huge block list", res, b, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge")
		res, b = do(t, http.MethodGet, j.srv.URL+BlobPath+"elsewhere/1", nil)
		checkBlobError(t, "unknown op", res, b, http.StatusNotFound, "ResourceNotFound")

		// The token's expiry ends the URL.
		r.clk.add(time.Hour)
		res, b = do(t, http.MethodPut, u+blk, strings.NewReader("x"))
		checkBlobError(t, "expired", res, b, http.StatusForbidden, "AuthenticationFailed")
		r.clk.add(-time.Hour)
	})

	t.Run("ermite errors", func(t *testing.T) {
		u := j.create(t, "quota", "")
		r.fake.mu.Lock()
		r.fake.maxArtifact = 3 << 20
		r.fake.mu.Unlock()
		defer func() {
			r.fake.mu.Lock()
			r.fake.maxArtifact = 0
			r.fake.mu.Unlock()
		}()
		res, b := do(t, http.MethodPut, u+"&comp=block&blockid=AA==", bytes.NewReader(make([]byte, 5<<20)))
		checkBlobError(t, "over quota", res, b, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge")
		for _, c := range []struct {
			code   connect.Code
			status int
			azure  string
		}{
			{connect.CodeNotFound, 404, "BlobNotFound"},
			{connect.CodeFailedPrecondition, 400, "InvalidBlobOrBlock"},
			{connect.CodeInvalidArgument, 400, "InvalidBlobOrBlock"},
			{connect.CodeResourceExhausted, 413, "RequestBodyTooLarge"},
			{connect.CodePermissionDenied, 403, "AuthenticationFailed"},
			{connect.CodeUnauthenticated, 403, "AuthenticationFailed"},
			{connect.CodeUnavailable, 503, "ServerBusy"},
			{connect.CodeInternal, 503, "ServerBusy"},
		} {
			r.fake.mu.Lock()
			r.fake.failNext["UploadJobArtifactBlock"] = connect.NewError(c.code, errors.New("injected"))
			r.fake.mu.Unlock()
			res, b := do(t, http.MethodPut, u+"&comp=block&blockid=AA==", strings.NewReader("x"))
			checkBlobError(t, "block "+c.code.String(), res, b, c.status, c.azure)
		}
		r.fake.mu.Lock()
		r.fake.failNext["CommitJobArtifact"] = connect.NewError(connect.CodeInvalidArgument, errors.New("injected"))
		r.fake.mu.Unlock()
		res, b = do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader("<BlockList/>"))
		checkBlobError(t, "commit invalid_argument", res, b, http.StatusBadRequest, "InvalidBlockList")
		// A different list after a commit: failed_precondition.
		if res, _ := do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader("<BlockList/>")); res.StatusCode != http.StatusCreated {
			t.Fatalf("commit: %d", res.StatusCode)
		}
		res, b = do(t, http.MethodPut, u+"&comp=blocklist", strings.NewReader("<BlockList><Latest>AA==</Latest></BlockList>"))
		checkBlobError(t, "commit failed_precondition", res, b, http.StatusBadRequest, "InvalidBlockList")
		res, b = do(t, http.MethodPut, u+"&comp=block&blockid=AA==", strings.NewReader("x"))
		checkBlobError(t, "block after commit", res, b, http.StatusBadRequest, "InvalidBlobOrBlock")
	})
}

// direct serves a PUT with h, without a network.
func direct(t *testing.T, h http.Handler, target string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, target, body))
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// failingBody yields data, then fails.
type failingBody struct {
	data io.Reader
}

func (f *failingBody) Read(p []byte) (int, error) {
	n, err := f.data.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset by sandbox")
	}
	return n, err
}

func TestBlockBodyErrorCancelsStream(t *testing.T) {
	r := newRig(t)
	j := r.job(1, "job", 1)
	u, _ := url.Parse(j.create(t, "cut", ""))
	id, _ := strconv.ParseInt(strings.TrimPrefix(u.Path, BlobPath+"upload/"), 10, 64)
	h := r.svc.Handler(j.Job)
	put := func(size int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, u.String()+"&comp=block&blockid=AA==", &failingBody{bytes.NewReader(make([]byte, size))})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// The body fails after 2.5 MiB: two messages went to ermite, then the
	// stream is cancelled.
	if rec := put(5 << 19); rec.Code == http.StatusCreated {
		t.Fatalf("a failed body got %d", rec.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.fake.streams() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("ermite never saw the stream end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := r.fake.pendingBlocks(id); n != 0 {
		t.Fatalf("ermite stored %d blocks of a broken upload", n)
	}
	// A body that fails before any byte never opens a stream.
	if rec := put(0); rec.Code == http.StatusCreated {
		t.Fatalf("a failed body got %d", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if n := r.fake.streams(); n != 1 {
		t.Fatalf("%d streams", n)
	}
	// And a good block is stored.
	res, _ := do(t, http.MethodPut, u.String()+"&comp=block&blockid=AA==", bytes.NewReader(make([]byte, 5<<19)))
	if res.StatusCode != http.StatusCreated || r.fake.pendingBlocks(id) != 1 {
		t.Fatalf("good block: %d, %d blocks", res.StatusCode, r.fake.pendingBlocks(id))
	}
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDownload(t *testing.T) {
	r := newRig(t)
	up := r.job(1, "up", 1)
	down := r.job(1, "down", 1)
	big := random(t, 3<<20+12345)
	up.upload(t, "big", "", big, 8<<20)
	up.upload(t, "report é.html", "text/html", []byte("<h1>raw</h1>"), 8<<20)
	up.upload(t, "zero", "", nil, 8<<20)

	t.Run("zip", func(t *testing.T) {
		res, b := do(t, http.MethodGet, down.signedURL(t, "big"), nil)
		if res.StatusCode != http.StatusOK || !bytes.Equal(b, big) || res.Header.Get("Content-Type") != "application/zip" ||
			res.Header.Get("Content-Length") != strconv.Itoa(len(big)) || res.Header.Get("Content-Disposition") != "" {
			t.Fatalf("%d %d bytes %v", res.StatusCode, len(b), res.Header)
		}
	})
	t.Run("archive false", func(t *testing.T) {
		res, b := do(t, http.MethodGet, down.signedURL(t, "report é.html"), nil)
		if res.StatusCode != http.StatusOK || string(b) != "<h1>raw</h1>" || res.Header.Get("Content-Type") != "text/html" ||
			res.Header.Get("Content-Disposition") != "attachment; filename*=UTF-8''report%20%C3%A9.html" {
			t.Fatalf("%d %q %v", res.StatusCode, b, res.Header)
		}
	})
	t.Run("empty", func(t *testing.T) {
		res, b := do(t, http.MethodGet, down.signedURL(t, "zero"), nil)
		if res.StatusCode != http.StatusOK || len(b) != 0 || res.Header.Get("Content-Length") != "0" {
			t.Fatalf("%d %q %v", res.StatusCode, b, res.Header)
		}
	})
	t.Run("not found", func(t *testing.T) {
		u := down.signedURL(t, "big")
		down.twirp(t, "DeleteArtifact", `{"name":"big"}`)
		res, b := do(t, http.MethodGet, u, nil)
		checkBlobError(t, "deleted", res, b, http.StatusNotFound, "BlobNotFound")
		up.upload(t, "big", "", big, 8<<20)
	})
	t.Run("resume", func(t *testing.T) {
		var cuts atomic.Int32
		r.fake.mu.Lock()
		r.fake.cut = func(open, sent int64) error {
			// Each of the first two streams breaks after a chunk.
			if sent >= 1<<20 && cuts.Load() < 2 {
				cuts.Add(1)
				return connect.NewError(connect.CodeUnavailable, errors.New("cut"))
			}
			return nil
		}
		r.fake.mu.Unlock()
		res, b := do(t, http.MethodGet, down.signedURL(t, "big"), nil)
		if res.StatusCode != http.StatusOK || !bytes.Equal(b, big) || cuts.Load() != 2 {
			t.Fatalf("%d %d bytes of %d, %d cuts", res.StatusCode, len(b), len(big), cuts.Load())
		}
	})
	t.Run("resume exhausted", func(t *testing.T) {
		r.fake.mu.Lock()
		r.fake.cut = func(open, sent int64) error {
			if open > 0 || sent >= 1<<20 {
				return connect.NewError(connect.CodeUnavailable, errors.New("cut"))
			}
			return nil
		}
		r.fake.mu.Unlock()
		defer func() {
			r.fake.mu.Lock()
			r.fake.cut = nil
			r.fake.mu.Unlock()
		}()
		res, err := http.Get(down.signedURL(t, "big"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || err == nil || len(b) != 1<<20 {
			t.Fatalf("%d, %d bytes, %v: want a truncated body", res.StatusCode, len(b), err)
		}
	})
	t.Run("other run", func(t *testing.T) {
		intruder := r.job(2, "intruder", 1)
		st, b := intruder.twirp(t, "GetSignedArtifactURL", `{"name":"big"}`)
		if st != http.StatusNotFound {
			t.Fatalf("%d %s", st, b)
		}
		if st, b := intruder.twirp(t, "ListArtifacts", `{}`); st != http.StatusOK || string(b) != `{"artifacts":[]}`+"\n" {
			t.Fatalf("%d %s", st, b)
		}
		// A URL of the run's artifact, signed for the intruder's channel (as if
		// it had guessed the id), is still refused by ermite.
		se := r.clk.now().Add(time.Hour).Unix()
		u := intruder.srv.URL + BlobPath + "download/2?se=" + strconv.FormatInt(se, 10) + "&sig=" + r.svc.blobSig(intruder.Job, opDownload, 2, se)
		res, b := do(t, http.MethodGet, u, nil)
		checkBlobError(t, "other run", res, b, http.StatusNotFound, "BlobNotFound")
	})
}
