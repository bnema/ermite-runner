// SPDX-License-Identifier: MIT

// Package registryproxy provides an anonymous, pull-only Docker Hub mirror.
// Its handler belongs on a sandbox's private runtime channel, not a public port.
package registryproxy

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zerowrap"
)

const manifestLimit = 4 << 20

// Config configures a Proxy. Max must be positive. The directory is exclusively
// owned by one Proxy; opening it clears incomplete downloads from tmp/.
type Config struct {
	Dir      string
	Max      int64
	Upstream string // default https://registry-1.docker.io
	AuthURL  string // default https://auth.docker.io/token
	Client   *http.Client
	Now      func() time.Time
	Log      zerowrap.Logger
	// RequestTimeout bounds auth, manifests and HEAD (default 10s).
	RequestTimeout time.Duration
	// BlobIdleTimeout bounds time without upstream body progress (default 30s).
	// A blob body has no total timeout.
	BlobIdleTimeout time.Duration
	// KeepAliveInterval is how often a client waiting for a cold blob is kept
	// alive while the download progresses (default 1s), when its
	// ResponseWriter has a KeepAlive method.
	KeepAliveInterval time.Duration
	// AbandonGrace is how long a download nobody waits for any more keeps
	// running in case another request joins it; then it is cancelled and its
	// temporary file removed (default 5s).
	AbandonGrace time.Duration
	// MaxDownloads caps the concurrent cold blob downloads across all jobs
	// (default 16). A request may also carry a per-client Quota (WithQuota).
	MaxDownloads int
	// SlotWait is how long a request that would start a download waits for a
	// free slot, global or of its quota, before it gets 503 (default 30s).
	SlotWait time.Duration
}

// Proxy implements http.Handler for GET and HEAD under /v2/.
type Proxy struct {
	s         *store
	u         *upstream
	log       zerowrap.Logger
	keepEvery time.Duration
}

// New opens the persistent cache. It starts no goroutines or listeners.
func New(c Config) (*Proxy, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Upstream == "" {
		c.Upstream = "https://registry-1.docker.io"
	}
	if c.AuthURL == "" {
		c.AuthURL = "https://auth.docker.io/token"
	}
	for _, s := range []string{c.Upstream, c.AuthURL} {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("invalid registry upstream URL")
		}
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 10 * time.Second
	}
	if c.BlobIdleTimeout <= 0 {
		c.BlobIdleTimeout = 30 * time.Second
	}
	if c.KeepAliveInterval <= 0 {
		c.KeepAliveInterval = time.Second
	}
	if c.AbandonGrace <= 0 {
		c.AbandonGrace = 5 * time.Second
	}
	if c.MaxDownloads <= 0 {
		c.MaxDownloads = 16
	}
	if c.SlotWait <= 0 {
		c.SlotWait = 30 * time.Second
	}
	client, blobs := upstreamClients(c)

	st, err := openStore(c.Dir, c.Max, c.Now)
	if err != nil {
		return nil, err
	}
	st.grace, st.maxDownloads, st.slotWait, st.log = c.AbandonGrace, c.MaxDownloads, c.SlotWait, c.Log
	return &Proxy{s: st, u: &upstream{base: strings.TrimRight(c.Upstream, "/"), auth: c.AuthURL, client: client, blobClient: blobs, requestTimeout: c.RequestTimeout, blobIdle: c.BlobIdleTimeout, now: c.Now, tokens: map[string]bearer{}}, log: c.Log, keepEvery: c.KeepAliveInterval}, nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func validDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(s[7:])
	return err == nil && s == strings.ToLower(s)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.URL.Path == "/v2/" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "2")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "{}")
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v2/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	i := strings.LastIndex(path, "/")
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	ref := path[i+1:]
	path = path[:i]
	i = strings.LastIndex(path, "/")
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	name, kind := path[:i], path[i+1:]
	// dockerd's legacy client sends one Accept line per media type.
	accept := strings.Join(r.Header.Values("Accept"), ",")
	if len(name) > 255 || !namePattern.MatchString(name) || len(accept) > 8192 {
		http.NotFound(w, r)
		return
	}
	if kind == "manifests" && (validDigest(ref) || tagPattern.MatchString(ref)) {
		p.manifest(w, r, name, ref, normalizeAccept(accept))
		return
	}
	if kind == "blobs" && validDigest(ref) {
		p.blob(w, r, name, ref)
		return
	}
	http.NotFound(w, r)
}
func serveFile(w http.ResponseWriter, r *http.Request, f *storedFile, media, digest string) {
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "registry cache read failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Docker-Content-Digest", digest)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// manifest serves a manifest by tag or digest. accept is normalized.
func (p *Proxy) manifest(w http.ResponseWriter, r *http.Request, name, ref, accept string) {
	start := time.Now()
	hit := false
	defer func() {
		p.log.Info().Str("repo", name).Str("ref", ref).Bool("hit", hit).Int64("ms", time.Since(start).Milliseconds()).Msg("registry manifest")
	}()
	ctx, cancel := context.WithTimeout(r.Context(), p.u.requestTimeout)
	defer cancel()
	digest := ref
	tag := !validDigest(ref)
	if tag {
		resp, err := p.u.request(ctx, http.MethodHead, name, "manifests", ref, accept)
		if err != nil {
			if unavailable(err) {
				digest = p.s.tag(name, ref, accept)
				if validDigest(digest) {
					if f, media, e := p.s.open("manifests", digest); e == nil {
						hit = true
						serveFile(w, r, f, media, digest)
						return
					}
				}
			}
			http.Error(w, "registry upstream unavailable", proxyStatus(err))
			return
		}
		digest = resp.Header.Get("Docker-Content-Digest")
		_ = resp.Body.Close()
		if !validDigest(digest) {
			http.Error(w, "invalid upstream digest", http.StatusBadGateway)
			return
		}
	}
	// Shared flight, as for blobs: it must not end with the initiator's request.
	f, media, cached, err := p.s.ensure(ctx, "manifests", digest, func(flightCtx context.Context, dst io.Writer) (string, error) {
		fillCtx, fillCancel := context.WithTimeout(flightCtx, p.u.requestTimeout)
		defer fillCancel()
		resp, err := p.u.request(fillCtx, http.MethodGet, name, "manifests", digest, accept)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(resp.Body, manifestLimit+1))
		if err != nil {
			return "", err
		}
		if len(b) > manifestLimit {
			return "", errTooLarge
		}
		_, err = dst.Write(b)
		return resp.Header.Get("Content-Type"), err
	}, keepalive{})
	hit = cached
	if err != nil {
		if tag && unavailable(err) {
			previous := p.s.tag(name, ref, accept)
			if validDigest(previous) {
				if old, oldMedia, e := p.s.open("manifests", previous); e == nil {
					hit = true
					serveFile(w, r, old, oldMedia, previous)
					return
				}
			}
		}
		http.Error(w, "registry fetch failed", proxyStatus(err))
		return
	}
	if tag {
		_ = p.s.setTag(name, ref, accept, digest)
	}
	serveFile(w, r, f, media, digest)
}
func (p *Proxy) blob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if f, media, err := p.s.open("blobs", digest); err == nil {
		serveFile(w, r, f, media, digest)
		return
	}
	// A cold HEAD is metadata-only and retains the short total timeout.
	if r.Method == http.MethodHead {
		ctx, cancel := context.WithTimeout(r.Context(), p.u.requestTimeout)
		defer cancel()
		resp, err := p.u.request(ctx, http.MethodHead, name, "blobs", digest, "")
		if err != nil {
			http.Error(w, "registry fetch failed", proxyStatus(err))
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.ContentLength < 0 {
			http.Error(w, "unknown blob length", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		w.Header().Set("Docker-Content-Digest", digest)
		return
	}
	// The flight is shared by every client that asks for this blob, so no client
	// may own it: it downloads on a context of its own, into the temporary file
	// only, and is bounded by Max and the upstream idle timeout. Every client,
	// the initiator included, waits for the verified commit and then reads the
	// stored file (simplest correct option: no client streams unverified bytes,
	// and a client that disconnects fails only itself). The flight is cancelled
	// when no client has waited for it for AbandonGrace. At most MaxDownloads
	// run at once, and at most its Quota per client: a request over either
	// waits up to SlotWait for a slot, then gets 503, which dockerd retries
	// (about 50 s) before pulling from Docker Hub. While it waits, a writer
	// with KeepAlive (the cache channel's progress deadline) is kept alive as
	// long as the download moves.
	keep := keepalive{every: p.keepEvery}
	if k, ok := w.(interface{ KeepAlive() }); ok {
		keep.fn = k.KeepAlive
	}
	f, media, _, err := p.s.ensure(r.Context(), "blobs", digest, func(downloadCtx context.Context, dst io.Writer) (string, error) {
		start := time.Now()
		var size int64
		defer func() {
			p.log.Debug().Str("digest", digest).Int64("bytes", size).Int64("ms", time.Since(start).Milliseconds()).Msg("registry blob")
		}()
		resp, err := p.u.blob(downloadCtx, name, digest)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.ContentLength > p.s.max {
			return "", errTooLarge
		}
		size, err = io.Copy(dst, resp.Body)
		return "application/octet-stream", err
	}, keep)
	if errors.Is(err, errBusy) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "registry downloads busy", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "registry fetch failed", proxyStatus(err))
		return
	}
	serveFile(w, r, f, media, digest)
}
