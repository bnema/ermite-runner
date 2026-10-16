// SPDX-License-Identifier: MIT

// Package resultssvc is the GitHub Actions results service of the runner, the
// part of it that artifacts v4 use (plan CI-4 C). actions/upload-artifact@v4+
// and actions/download-artifact@v4+ bundle @actions/artifact v2, which talks
// Twirp JSON (github.actions.results.api.v1.ArtifactService) to
// ACTIONS_RESULTS_URL and moves the bytes with Azure block blob requests to the
// signed URLs that service returns.
//
// The runner keeps no artifact: the service translates both protocols to
// ermite's JobArtifactService, with the job token of the job, and streams the
// blocks and the downloads through without spooling them. Run and job ids come
// from the Job the channel serves, never from the bodies of the requests: a
// job reaches the artifacts of its own run only, and ermite checks that too.
//
// A Handler serves the channel of one job, beside the cache (cachesvc): the
// sandbox reaches both through the same relay. The Twirp calls carry
// ACTIONS_RUNTIME_TOKEN, which must be a token of that job with the job's
// Actions.Results scope; the blob URLs carry no bearer (the Azure SDK sends
// none), so they are signed, bound to the job and expire with its token.
package resultssvc

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"github.com/bnema/zerowrap"
)

// Paths the service answers on. The toolkit appends "twirp/github.actions.
// results.api.v1.ArtifactService/<method>" to ACTIONS_RESULTS_URL
// (ArtifactHttpClient.request, upload-artifact v4.6.2 dist/upload/index.js:2753);
// the blob URLs are the service's own, given out in signedUploadUrl and
// signedUrl.
const (
	TwirpPath = "/twirp/github.actions.results.api.v1.ArtifactService/"
	// BlobPath is followed by "upload/<artifact id>" or "download/<artifact id>".
	BlobPath = "/_apis/results/blob/"
)

// Auth is what the service needs from the runner's runtime token signer
// (cachesvc.Service implements it).
type Auth interface {
	// RuntimeToken checks ACTIONS_RUNTIME_TOKEN (a JWT this runner made,
	// signature and expiry) and returns its job id ("j" claim), its
	// space-separated scp claim, and its expiry.
	RuntimeToken(token string) (jobID, scopes string, expiry time.Time, err error)
	// BlobMAC is an HMAC of the parts under a key domain reserved for blob URL
	// signatures.
	BlobMAC(parts ...[]byte) []byte
}

// Config configures a Service.
type Config struct {
	Auth Auth
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	Log zerowrap.Logger
}

// Service is the results service. It holds no state of its own: one Service
// serves the channels of every job of the runner.
type Service struct {
	auth Auth
	now  func() time.Time
	log  zerowrap.Logger
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{auth: cfg.Auth, now: cfg.Now, log: cfg.Log}
}

// Job is the job a sandbox channel serves.
type Job struct {
	// ID is the Job.job_id.
	ID string
	// RunID is the run of the job and Attempt its attempt.
	RunID   int64
	Attempt int32
	// Client calls ermite with the job token (ermitejob_...), which never
	// reaches the sandbox.
	Client ermiteconnect.JobArtifactServiceClient
}

// Handler serves TwirpPath and BlobPath for the channel of one job; any other
// path is a 404.
func (s *Service) Handler(job Job) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, TwirpPath):
			s.twirp(job, w, r, strings.TrimPrefix(r.URL.Path, TwirpPath))
		case strings.HasPrefix(r.URL.Path, BlobPath):
			s.blob(job, w, r, strings.TrimPrefix(r.URL.Path, BlobPath))
		default:
			http.NotFound(w, r)
		}
	})
}

// JobBackendID is the workflow_job_run_backend_id of a job: 32 lowercase hex
// characters of SHA-256(jobID "/" decimal attempt). It has no ':' since the
// toolkit splits the scope on ':' (getBackendIdsFromToken, upload-artifact
// v4.6.2 dist/upload/index.js:3106-3141).
func JobBackendID(jobID string, attempt int32) string {
	sum := sha256.Sum256([]byte(jobID + "/" + strconv.FormatInt(int64(attempt), 10)))
	return hex.EncodeToString(sum[:16])
}

// Scope is the scp entry of a job: "Actions.Results:<decimal run id>:
// <JobBackendID>". The toolkit reads its run and job backend ids from it.
func Scope(runID int64, jobID string, attempt int32) string {
	return "Actions.Results:" + strconv.FormatInt(runID, 10) + ":" + JobBackendID(jobID, attempt)
}
