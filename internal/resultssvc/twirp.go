// SPDX-License-Identifier: MIT

package resultssvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite-api/gen/ermite"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxTwirpBody bounds a Twirp request body: the largest the toolkit sends is a
// few hundred bytes.
const maxTwirpBody = 1 << 20

// msgExists is the message of an already_exists reply when ermite gives none:
// the toolkit's own wording for it.
const msgExists = "an artifact with this name already exists on the workflow run"

// twirpError is an error reply: a status and the {"code","msg"} body of Twirp.
// The toolkit retries 5xx and 429 (ArtifactHttpClient.isRetryableHttpStatusCode,
// upload-artifact v4.6.2 dist/upload/index.js:2827-2837) and fails at once on
// anything else, with msg in the job log (:2787-2793). The body must be JSON
// whatever the status: a body that does not parse is retried as a network
// error (:2795-2806).
type twirpError struct {
	status int
	code   string
	msg    string
}

func (e *twirpError) Error() string { return e.code + ": " + e.msg }

// twirpRequest is the union of the request messages of the methods. The
// toolkit's generated client sends proto field names (useProtoFieldName),
// wrapper types (StringValue, Int64Value) as bare values and int64 as strings;
// the camelCase names that protojson also accepts are read too. Unknown fields
// are ignored. The run and job backend ids are not read: everything comes from
// the Job of the channel.
type twirpRequest struct {
	Name         string          `json:"name"`
	MimeType     string          `json:"mime_type"`
	MimeTypeCC   string          `json:"mimeType"`
	ExpiresAt    string          `json:"expires_at"`
	ExpiresAtCC  string          `json:"expiresAt"`
	Size         json.RawMessage `json:"size"`
	Hash         string          `json:"hash"`
	NameFilter   string          `json:"name_filter"`
	NameFilterCC string          `json:"nameFilter"`
	IDFilter     json.RawMessage `json:"id_filter"`
	IDFilterCC   json.RawMessage `json:"idFilter"`
}

func firstOf[T string | json.RawMessage](a, b T) T {
	if len(a) > 0 {
		return a
	}
	return b
}

// int64Value reads an int64 sent as a JSON string (protojson) or a number. An
// absent or null value is 0.
func int64Value(raw json.RawMessage, field string) (int64, *twirpError) {
	v := strings.TrimSpace(string(raw))
	if v == "" || v == "null" {
		return 0, nil
	}
	if s, err := strconv.Unquote(v); err == nil {
		v = s
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, &twirpError{http.StatusBadRequest, "invalid_argument", field + " is not an int64"}
	}
	return n, nil
}

// artifactJSON is an item of ListArtifactsResponse, in the camelCase of the
// toolkit's fromJson with int64 as strings.
type artifactJSON struct {
	WorkflowRunBackendID    string `json:"workflowRunBackendId"`
	WorkflowJobRunBackendID string `json:"workflowJobRunBackendId"`
	DatabaseID              string `json:"databaseId"`
	Name                    string `json:"name"`
	Size                    string `json:"size"`
	CreatedAt               string `json:"createdAt,omitempty"`
	Digest                  string `json:"digest,omitempty"`
}

// twirp serves one Twirp method. The routes are those of the toolkit's
// ArtifactServiceClientJSON: CreateArtifact, FinalizeArtifact, ListArtifacts,
// GetSignedArtifactURL and DeleteArtifact, all POST with a JSON body.
func (s *Service) twirp(job Job, w http.ResponseWriter, r *http.Request, method string) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, &twirpError{http.StatusMethodNotAllowed, "bad_route", "unsupported method " + r.Method})
		return
	}
	var call func(Job, *http.Request, *twirpRequest, time.Time) (any, error)
	switch method {
	case "CreateArtifact":
		call = s.createArtifact
	case "FinalizeArtifact":
		call = s.finalizeArtifact
	case "ListArtifacts":
		call = s.listArtifacts
	case "GetSignedArtifactURL":
		call = s.signedArtifactURL
	case "DeleteArtifact":
		call = s.deleteArtifact
	default:
		writeTwirpError(w, &twirpError{http.StatusNotFound, "bad_route", "no such method: " + method})
		return
	}
	expiry, ok := s.authorize(job, r)
	if !ok {
		writeTwirpError(w, &twirpError{http.StatusUnauthorized, "unauthenticated", "invalid runtime token"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTwirpBody))
	if err != nil {
		writeTwirpError(w, &twirpError{http.StatusBadRequest, "malformed", "the request body could not be read"})
		return
	}
	var req twirpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeTwirpError(w, &twirpError{http.StatusBadRequest, "malformed", "the request body is not a JSON " + method + " request"})
		return
	}
	s.log.Debug().Str("method", method).Str("job", job.ID).Str("name", req.Name).Msg("results twirp")
	out, err := call(job, r, &req, expiry)
	if err != nil {
		var te *twirpError
		if !errors.As(err, &te) {
			te = twirpFromConnect(err)
			s.log.Warn().Str("method", method).Str("job", job.ID).Str("code", connect.CodeOf(err).String()).Str("error", te.msg).Msg("results: ermite refused")
		}
		writeTwirpError(w, te)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// authorize checks the bearer token: a runtime token of this channel's job
// whose scopes hold the job's Actions.Results scope. It returns the token's
// expiry, which is the expiry of the blob URLs. Every refusal looks the same.
func (s *Service) authorize(job Job, r *http.Request) (time.Time, bool) {
	const p = "Bearer "
	a := r.Header.Get("Authorization")
	if len(a) <= len(p) || !strings.EqualFold(a[:len(p)], p) || s.auth == nil {
		return time.Time{}, false
	}
	jobID, scopes, expiry, err := s.auth.RuntimeToken(strings.TrimSpace(a[len(p):]))
	if err != nil || jobID != job.ID || !s.now().Before(expiry) {
		return time.Time{}, false
	}
	want := Scope(job.RunID, job.ID, job.Attempt)
	for _, sc := range strings.Fields(scopes) {
		if sc == want {
			return expiry, true
		}
	}
	return time.Time{}, false
}

func (s *Service) createArtifact(job Job, r *http.Request, req *twirpRequest, expiry time.Time) (any, error) {
	in := &ermite.CreateJobArtifactRequest{JobId: job.ID, Name: req.Name, MediaType: firstOf(req.MimeType, req.MimeTypeCC)}
	if v := firstOf(req.ExpiresAt, req.ExpiresAtCC); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, &twirpError{http.StatusBadRequest, "invalid_argument", "expires_at is not an RFC 3339 time"}
		}
		in.ExpiresAt = timestamppb.New(t)
	}
	res, err := job.Client.CreateJobArtifact(r.Context(), connect.NewRequest(in))
	if err != nil {
		return nil, err
	}
	id := res.Msg.GetArtifactId()
	return map[string]any{"ok": true, "signedUploadUrl": s.blobURL(job, r, opUpload, id, expiry)}, nil
}

func (s *Service) finalizeArtifact(job Job, r *http.Request, req *twirpRequest, _ time.Time) (any, error) {
	size, te := int64Value(req.Size, "size")
	if te != nil {
		return nil, te
	}
	res, err := job.Client.FinalizeJobArtifact(r.Context(), connect.NewRequest(&ermite.FinalizeJobArtifactRequest{
		JobId: job.ID, Name: req.Name, Size: size, Digest: req.Hash,
	}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "artifactId": strconv.FormatInt(res.Msg.GetArtifact().GetId(), 10)}, nil
}

func (s *Service) listArtifacts(job Job, r *http.Request, req *twirpRequest, _ time.Time) (any, error) {
	id, te := int64Value(firstOf(req.IDFilter, req.IDFilterCC), "id_filter")
	if te != nil {
		return nil, te
	}
	res, err := job.Client.ListJobArtifacts(r.Context(), connect.NewRequest(&ermite.ListJobArtifactsRequest{
		JobId: job.ID, Name: firstOf(req.NameFilter, req.NameFilterCC), ArtifactId: id,
	}))
	if err != nil {
		return nil, err
	}
	items := make([]artifactJSON, 0, len(res.Msg.GetArtifacts()))
	for _, a := range res.Msg.GetArtifacts() {
		it := artifactJSON{
			WorkflowRunBackendID: strconv.FormatInt(job.RunID, 10),
			// The job of the artifact, not the caller: download-artifact passes
			// it back in GetSignedArtifactURL, which ignores it.
			WorkflowJobRunBackendID: JobBackendID(a.GetJobId(), a.GetAttempt()),
			DatabaseID:              strconv.FormatInt(a.GetId(), 10),
			Name:                    a.GetName(),
			Size:                    strconv.FormatInt(a.GetSize(), 10),
			Digest:                  a.GetDigest(),
		}
		if a.GetCreatedAt() != nil {
			it.CreatedAt = a.GetCreatedAt().AsTime().UTC().Format(time.RFC3339Nano)
		}
		items = append(items, it)
	}
	return map[string]any{"artifacts": items}, nil
}

// signedArtifactURL answers GetSignedArtifactURL: the download URL of the
// artifact of that name of the run. The body's job backend id is the
// uploader's (download-artifact copies it from the list), so it is not read.
func (s *Service) signedArtifactURL(job Job, r *http.Request, req *twirpRequest, expiry time.Time) (any, error) {
	if req.Name == "" {
		return nil, &twirpError{http.StatusBadRequest, "invalid_argument", "name is required"}
	}
	res, err := job.Client.ListJobArtifacts(r.Context(), connect.NewRequest(&ermite.ListJobArtifactsRequest{JobId: job.ID, Name: req.Name}))
	if err != nil {
		return nil, err
	}
	var id int64
	for _, a := range res.Msg.GetArtifacts() {
		id = max(id, a.GetId())
	}
	if id == 0 {
		return nil, &twirpError{http.StatusNotFound, "not_found", fmt.Sprintf("artifact %q not found", req.Name)}
	}
	return map[string]any{"signedUrl": s.blobURL(job, r, opDownload, id, expiry)}, nil
}

func (s *Service) deleteArtifact(job Job, r *http.Request, req *twirpRequest, _ time.Time) (any, error) {
	res, err := job.Client.DeleteJobArtifact(r.Context(), connect.NewRequest(&ermite.DeleteJobArtifactRequest{JobId: job.ID, Name: req.Name}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "artifactId": strconv.FormatInt(res.Msg.GetArtifactId(), 10)}, nil
}

// twirpFromConnect maps an error of ermite to a Twirp error. ermite's message
// is kept: it reaches the job log, and it never holds the job token.
// resource_exhausted is a 412 and not Twirp's 429: the toolkit would retry a
// 429 five times, and a full quota is not transient.
func twirpFromConnect(err error) *twirpError {
	msg := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	te := &twirpError{msg: msg}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument:
		te.status, te.code = http.StatusBadRequest, "invalid_argument"
	case connect.CodeNotFound:
		te.status, te.code = http.StatusNotFound, "not_found"
	case connect.CodeAlreadyExists:
		te.status, te.code = http.StatusConflict, "already_exists"
		if te.msg == "" {
			te.msg = msgExists
		}
	case connect.CodeFailedPrecondition, connect.CodeResourceExhausted:
		te.status, te.code = http.StatusPreconditionFailed, "failed_precondition"
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		te.status, te.code = http.StatusUnauthorized, "unauthenticated"
	default:
		te.status, te.code = http.StatusServiceUnavailable, "unavailable"
	}
	if te.msg == "" {
		te.msg = te.code
	}
	return te
}

func writeTwirpError(w http.ResponseWriter, e *twirpError) {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(map[string]string{"code": e.code, "msg": e.msg})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(e.status)
	_, _ = w.Write(b.Bytes())
}

// blobURL is a signed URL of the blob of an artifact, on the host the sandbox
// used to reach the service.
func (s *Service) blobURL(job Job, r *http.Request, op string, id int64, expiry time.Time) string {
	se := expiry.Unix()
	q := url.Values{"se": {strconv.FormatInt(se, 10)}, "sig": {s.blobSig(job, op, id, se)}}
	return "http://" + r.Host + BlobPath + op + "/" + strconv.FormatInt(id, 10) + "?" + q.Encode()
}
