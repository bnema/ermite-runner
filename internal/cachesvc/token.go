// SPDX-License-Identifier: MIT

package cachesvc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scope is what a job may do in the cache, as GitHub scopes it: entries belong
// to a repository and a ref. A job writes to one ref only and reads from a
// list of refs in priority order (its own ref, then the base branch of a pull
// request, then the default branch).
//
// The scope of a job comes from Job.cache (see internal/host/cache.go, which
// checks it). An empty WriteRef makes the job read-only; no ReadRefs, write-only.
type Scope struct {
	Repository string   `json:"r"`
	WriteRef   string   `json:"w,omitempty"`
	ReadRefs   []string `json:"rd,omitempty"`
}

// Claims are what a runtime token (ACTIONS_RUNTIME_TOKEN) vouches for: the
// job, its cache scope and its other scopes. The token is shared by the cache
// and the results service of the job's channel.
type Claims struct {
	JobID string `json:"j"`
	Scope
	// Expiry is a Unix time in seconds.
	Expiry int64 `json:"x"`
	// ExpiresAt and IssuedAt are the JWT's exp and iat; exp is Expiry.
	ExpiresAt int64 `json:"exp"`
	IssuedAt  int64 `json:"iat"`
	// Scopes is the scp claim: space-separated scopes, as GitHub's runtime
	// token has them. @actions/artifact decodes the token (without checking
	// it) and takes the run and job ids from its Actions.Results scope
	// (toolkit packages/artifact/src/internal/shared/util.ts:21-75).
	Scopes string `json:"scp"`
}

// genericReadScope is the first scope of GitHub's runtime tokens.
const genericReadScope = "Actions.GenericRead:00000000-0000-0000-0000-000000000000"

// errForbidden is every authentication failure: the caller learns nothing about
// why a token was refused.
var errForbidden = errors.New("cache: forbidden")

// signer makes and checks the HMACs of the service: job tokens
// (ACTIONS_RUNTIME_TOKEN) and the signed archive URLs. The key is random, made
// at start and never written to disk: a restart invalidates every token, which
// is right since it also ends every job.
type signer struct{ key [32]byte }

func newSigner() (*signer, error) {
	s := &signer{}
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, fmt.Errorf("cache: random key: %w", err)
	}
	return s, nil
}

// mac separates the uses of the key by a prefix, so that a signature of one kind
// can never pass for another.
func (s *signer) mac(domain string, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, s.key[:])
	h.Write([]byte(domain))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write(p)
	}
	return h.Sum(nil)
}

var b64 = base64.RawURLEncoding

// maxTokenLen bounds what verify parses.
const maxTokenLen = 16 << 10

// jwtHeader is the header of every runtime token, as base64url.
var jwtHeader = b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// jwtKey is the HS256 key of the tokens: derived from the signer's key under
// the "token" domain, so that no other signature of the signer (archive or
// blob URLs) can ever pass for a token, and the other way round.
func (s *signer) jwtKey() []byte { return s.mac("token") }

// token is a three-segment JWT (header, claims, HS256 signature), which is what
// @actions/artifact requires of ACTIONS_RUNTIME_TOKEN; @actions/cache sends it
// as an opaque bearer token.
func (s *signer) token(c Claims) string {
	payload, _ := json.Marshal(c) // plain strings and ints: cannot fail
	signed := jwtHeader + "." + b64.EncodeToString(payload)
	h := hmac.New(sha256.New, s.jwtKey())
	h.Write([]byte(signed))
	return signed + "." + b64.EncodeToString(h.Sum(nil))
}

// verifyAny returns the claims of a token that this runner made and that has
// not expired, whatever its scopes. The comparison is constant time.
func (s *signer) verifyAny(tok string, now time.Time) (Claims, error) {
	var c Claims
	if len(tok) > maxTokenLen {
		return c, errForbidden
	}
	head, rest, ok1 := strings.Cut(tok, ".")
	p, m, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 || head != jwtHeader {
		return c, errForbidden
	}
	sum, err := b64.DecodeString(m)
	if err != nil {
		return c, errForbidden
	}
	h := hmac.New(sha256.New, s.jwtKey())
	h.Write([]byte(head + "." + p))
	if !hmac.Equal(sum, h.Sum(nil)) {
		return c, errForbidden
	}
	payload, err := b64.DecodeString(p)
	if err != nil || json.Unmarshal(payload, &c) != nil || c.JobID == "" || now.Unix() >= c.Expiry {
		return Claims{}, errForbidden
	}
	return c, nil
}

// verify is verifyAny for the cache: the token must carry a cache scope (a
// token made for the results service only has none).
func (s *signer) verify(tok string, now time.Time) (Claims, error) {
	c, err := s.verifyAny(tok, now)
	if err != nil || c.Repository == "" {
		return Claims{}, errForbidden
	}
	return c, nil
}

// archiveSig signs the download URL of one entry. The archive is fetched by a
// client without the bearer token (downloadUtils.ts:168 uses a bare HttpClient),
// so the URL is the credential; it is bound to the entry, its repository, the
// job it was issued to and an expiry. The job is what keeps a URL that left its
// sandbox useless on the channel of any other.
func (s *signer) archiveSig(id int64, repo, job string, exp int64) string {
	return b64.EncodeToString(s.mac("archive", []byte(strconv.FormatInt(id, 10)), []byte(repo), []byte(job), []byte(strconv.FormatInt(exp, 10))))
}

func (s *signer) archiveOK(id int64, repo, job string, exp int64, sig string, now time.Time) bool {
	got, err := b64.DecodeString(sig)
	if err != nil || now.Unix() >= exp {
		return false
	}
	want, _ := b64.DecodeString(s.archiveSig(id, repo, job, exp))
	return hmac.Equal(got, want)
}
