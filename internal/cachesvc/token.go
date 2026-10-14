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

// Claims are what a cache token vouches for.
type Claims struct {
	JobID string `json:"j"`
	Scope
	// Expiry is a Unix time in seconds.
	Expiry int64 `json:"x"`
}

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

// token is base64url(JSON claims) "." base64url(HMAC-SHA256 of the JSON bytes).
func (s *signer) token(c Claims) string {
	payload, _ := json.Marshal(c) // plain strings and ints: cannot fail
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(s.mac("token", payload))
}

// verify returns the claims of a token that this runner made and that has not
// expired. The comparison is constant time.
func (s *signer) verify(tok string, now time.Time) (Claims, error) {
	var c Claims
	if len(tok) > maxTokenLen {
		return c, errForbidden
	}
	p, m, ok := strings.Cut(tok, ".")
	if !ok {
		return c, errForbidden
	}
	payload, err1 := b64.DecodeString(p)
	sum, err2 := b64.DecodeString(m)
	if err1 != nil || err2 != nil || !hmac.Equal(sum, s.mac("token", payload)) {
		return c, errForbidden
	}
	if err := json.Unmarshal(payload, &c); err != nil || c.Repository == "" || now.Unix() >= c.Expiry {
		return Claims{}, errForbidden
	}
	return c, nil
}

// archiveSig signs the download URL of one entry. The archive is fetched by a
// client without the bearer token (downloadUtils.ts:168 uses a bare HttpClient),
// so the URL is the credential; it is bound to the entry, its repository and an
// expiry.
func (s *signer) archiveSig(id int64, repo string, exp int64) string {
	return b64.EncodeToString(s.mac("archive", []byte(strconv.FormatInt(id, 10)), []byte(repo), []byte(strconv.FormatInt(exp, 10))))
}

func (s *signer) archiveOK(id int64, repo string, exp int64, sig string, now time.Time) bool {
	got, err := b64.DecodeString(sig)
	if err != nil || now.Unix() >= exp {
		return false
	}
	want, _ := b64.DecodeString(s.archiveSig(id, repo, exp))
	return hmac.Equal(got, want)
}
