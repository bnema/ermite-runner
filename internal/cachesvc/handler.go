// SPDX-License-Identifier: MIT

package cachesvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite-runner/internal/registryproxy"
)

// APIPath is the prefix of the cache API: the toolkit appends "_apis/
// artifactcache/<resource>" to ACTIONS_CACHE_URL
// (toolkit/packages/cache/src/internal/cacheHttpClient.ts:47).
const APIPath = "/_apis/artifactcache/"

// Limits of the requests, from the toolkit (cache.ts:97-115: a key is at most
// 512 characters and has no comma; restore keys are at most 10) and of this
// service (the version is a sha256 hex string for the toolkit).
const (
	maxKeyLen     = 512
	maxVersionLen = 128
	maxKeys       = 11
	maxJSONBody   = 1 << 20
)

// Messages the toolkit looks for (cache.ts:50, constants.ts:45): a denial that
// starts with them is logged as a warning, not as a failure.
const (
	writeDenied = "cache write denied: "
	readDenied  = "cache read denied: "
)

// Handler returns the HTTP handler of the cache service (API v1), for any job.
func (s *Service) Handler() http.Handler { return s.handler("") }

// handler is Handler for the channel of one job: with a job id, a token made
// for another job is refused, so a token that left its sandbox is useless on
// any other sandbox's channel.
func (s *Service) handler(job string) http.Handler {
	mux := http.NewServeMux()
	if s.registry != nil {
		registry := s.serveRegistry(job, make(chan struct{}, s.registryPerChannel), registryproxy.NewQuota(RegistryDownloadsPerChannel))
		mux.HandleFunc("GET "+RegistryPath, registry)
		mux.HandleFunc("HEAD "+RegistryPath, registry)
	}
	// The routes are those of the toolkit's v1 client (cacheHttpClient.ts):
	//   GET   cache?keys=k1,k2&version=v  :89  lookup
	//   GET   caches?key=k                :132 diagnostics (core.isDebug())
	//   POST  caches {key,version,cacheSize} :190 reserve -> {cacheId}
	//   PATCH caches/{id} Content-Range   :230 chunk upload
	//   POST  caches/{id} {size}          :330 commit
	//   GET   <archiveLocation>           downloadUtils.ts:168 (no bearer token)
	mux.HandleFunc("GET "+APIPath+"cache", s.auth(job, s.lookup))
	mux.HandleFunc("GET "+APIPath+"caches", s.auth(job, s.listCaches))
	mux.HandleFunc("POST "+APIPath+"caches", s.auth(job, s.reserve))
	mux.HandleFunc("PATCH "+APIPath+"caches/{id}", s.auth(job, s.upload))
	mux.HandleFunc("POST "+APIPath+"caches/{id}", s.auth(job, s.commit))
	mux.HandleFunc("GET "+APIPath+"artifacts/{id}", s.download(job))
	mux.HandleFunc("HEAD "+APIPath+"artifacts/{id}", s.download(job))
	mux.HandleFunc("GET "+ArchivePath+"{key}", s.serveActionArchive(job))
	// net/http resets the write deadline per request only when the server has a
	// WriteTimeout, which would cut long uploads and downloads: set a fresh
	// progress deadline for each request here, so that a deadline left by the
	// previous request of a kept-alive connection never applies to this one.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		d := time.Now().Add(s.progress)
		_ = rc.SetReadDeadline(d)
		_ = rc.SetWriteDeadline(d)
		mux.ServeHTTP(w, r)
	})
}

// channelHandler is the handler of a sandbox's channel: the cache API, the
// action archives and the registry, and when results is not nil everything else goes to it (the results service of the
// job: Twirp and blob paths). The results service gets the same progress
// deadlines as the cache: its bodies and replies must keep moving.
func (s *Service) channelHandler(job string, results http.Handler) http.Handler {
	cache := s.handler(job)
	if results == nil {
		return cache
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, APIPath) || strings.HasPrefix(r.URL.Path, ArchivePath) || strings.HasPrefix(r.URL.Path, RegistryPath) {
			cache.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		pr := newProgressReader(r.Body, rc, s.progress, nil)
		defer pr.done()
		pr.set()
		r.Body = readCloser{pr, r.Body}
		results.ServeHTTP(&progressWriter{ResponseWriter: w, rc: rc, idle: s.progress}, r)
	})
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, c Claims)

// auth checks the bearer token (ACTIONS_RUNTIME_TOKEN), and when job is not
// empty that it was made for that job. Every refusal is a bare 403 whatever the
// reason.
func (s *Service) auth(job string, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const p = "Bearer "
		a := r.Header.Get("Authorization")
		if len(a) <= len(p) || !strings.EqualFold(a[:len(p)], p) {
			reply(w, http.StatusForbidden, "forbidden")
			return
		}
		c, err := s.sign.verify(strings.TrimSpace(a[len(p):]), s.now())
		if err != nil || (job != "" && c.JobID != job) {
			reply(w, http.StatusForbidden, "forbidden")
			return
		}
		// Every body, JSON or chunk, must keep moving: a client that stalls
		// loses its connection (and an upload its in-flight range).
		pr := newProgressReader(r.Body, http.NewResponseController(w), s.progress, s.remaining(c.Expiry))
		defer pr.done()
		r.Body = readCloser{pr, r.Body}
		h(w, r, c)
	}
}

type readCloser struct {
	io.Reader
	io.Closer
}

// reply sends the {"message": …} error body that @actions/http-client turns into
// the error message.
func reply(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func replyJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ArtifactCacheEntry is the lookup answer (toolkit contracts.ts:10-16).
type artifactCacheEntry struct {
	CacheKey        string `json:"cacheKey"`
	Scope           string `json:"scope"`
	CacheVersion    string `json:"cacheVersion"`
	CreationTime    string `json:"creationTime"`
	ArchiveLocation string `json:"archiveLocation"`
}

func (s *Service) entryJSON(r *http.Request, c Claims, e Entry) artifactCacheEntry {
	exp := c.Expiry
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "job": {c.JobID}, "sig": {s.sign.archiveSig(e.ID, e.Repository, c.JobID, exp)}}
	host := r.Host
	return artifactCacheEntry{
		CacheKey: e.Key, Scope: e.Ref, CacheVersion: e.Version,
		CreationTime:    e.Created.UTC().Format("2006-01-02T15:04:05Z"),
		ArchiveLocation: "http://" + host + APIPath + "artifacts/" + strconv.FormatInt(e.ID, 10) + "?" + q.Encode(),
	}
}

func validKey(k string) bool {
	return k != "" && len(k) <= maxKeyLen && !strings.Contains(k, ",")
}

func validVersion(v string) bool { return v != "" && len(v) <= maxVersionLen }

func (s *Service) lookup(w http.ResponseWriter, r *http.Request, c Claims) {
	if len(c.ReadRefs) == 0 {
		reply(w, http.StatusForbidden, readDenied+"this job may not read the cache")
		return
	}
	q := r.URL.Query()
	keys := strings.Split(q.Get("keys"), ",")
	version := q.Get("version")
	if len(keys) > maxKeys || !validVersion(version) {
		reply(w, http.StatusBadRequest, "bad keys or version")
		return
	}
	for _, k := range keys {
		if !validKey(k) {
			reply(w, http.StatusBadRequest, "bad keys or version")
			return
		}
	}
	e, ok := s.store.find(c.Repository, c.ReadRefs, keys, version)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	replyJSON(w, http.StatusOK, s.entryJSON(r, c, e))
}

func (s *Service) listCaches(w http.ResponseWriter, r *http.Request, c Claims) {
	if len(c.ReadRefs) == 0 {
		reply(w, http.StatusForbidden, readDenied+"this job may not read the cache")
		return
	}
	list := s.store.list(c.Repository, c.ReadRefs, r.URL.Query().Get("key"))
	out := struct {
		TotalCount     int                  `json:"totalCount"`
		ArtifactCaches []artifactCacheEntry `json:"artifactCaches"`
	}{TotalCount: len(list), ArtifactCaches: []artifactCacheEntry{}}
	for _, e := range list {
		out.ArtifactCaches = append(out.ArtifactCaches, s.entryJSON(r, c, e))
	}
	replyJSON(w, http.StatusOK, out)
}

func (s *Service) reserve(w http.ResponseWriter, r *http.Request, c Claims) {
	if c.WriteRef == "" {
		reply(w, http.StatusForbidden, writeDenied+"this job may not write the cache")
		return
	}
	var req struct {
		Key       string `json:"key"`
		Version   string `json:"version"`
		CacheSize int64  `json:"cacheSize"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req); err != nil || !validKey(req.Key) || !validVersion(req.Version) || req.CacheSize < 0 {
		reply(w, http.StatusBadRequest, "bad reservation")
		return
	}
	id, err := s.store.reserve(c.JobID, c.Repository, c.WriteRef, req.Key, req.Version, req.CacheSize, time.Unix(c.Expiry, 0))
	switch {
	case errors.Is(err, errConflict):
		reply(w, http.StatusConflict, err.Error())
	case errors.Is(err, errTooBig):
		reply(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errNoRoom), errors.Is(err, errTooMany):
		reply(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		s.log.Error().Err(err).Msg("cache reserve")
		reply(w, http.StatusInternalServerError, "cache service error")
	default:
		replyJSON(w, http.StatusCreated, map[string]int64{"cacheId": id})
	}
}

func parseID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// parseRange reads "bytes a-b/*" (cacheHttpClient.ts:206-212): both ends
// inclusive; the total is a star or a positive size that covers the range.
func parseRange(h string) (lo, hi int64, ok bool) {
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, total, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	lo, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || lo < 0 || last < lo || last >= 1<<62 {
		return 0, 0, false
	}
	if total != "*" {
		n, err := strconv.ParseInt(total, 10, 64)
		if err != nil || n <= 0 || n < last+1 {
			return 0, 0, false
		}
	}
	return lo, last + 1, true
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request, c Claims) {
	id, ok := parseID(r)
	lo, hi, ok2 := parseRange(r.Header.Get("Content-Range"))
	if !ok || !ok2 {
		reply(w, http.StatusBadRequest, "bad cache id or Content-Range")
		return
	}
	path, err := s.store.beginChunk(id, c, lo, hi)
	if err != nil {
		s.storeError(w, err)
		return
	}
	// The range counts as received only when the whole body was written and
	// checked; on any failure its coverage stays removed.
	written := false
	defer func() { s.store.endChunk(id, lo, hi, written) }()
	if s.hookChunk != nil {
		s.hookChunk(id)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		s.log.Error().Err(err).Msg("cache chunk")
		reply(w, http.StatusInternalServerError, "cache service error")
		return
	}
	defer func() { _ = f.Close() }()
	// Exactly the announced bytes: fewer or more is a broken client or a
	// truncated connection.
	n, err := io.Copy(io.NewOffsetWriter(f, lo), io.LimitReader(r.Body, hi-lo))
	if err != nil || n != hi-lo {
		reply(w, http.StatusBadRequest, fmt.Sprintf("the body is not the %d bytes of the Content-Range", hi-lo))
		return
	}
	if extra, _ := io.CopyN(io.Discard, r.Body, 1); extra > 0 {
		reply(w, http.StatusBadRequest, "the body is longer than the Content-Range")
		return
	}
	if err := f.Close(); err != nil {
		s.log.Error().Err(err).Msg("cache chunk")
		reply(w, http.StatusInternalServerError, "cache service error")
		return
	}
	written = true
	w.WriteHeader(http.StatusNoContent)
}

// progressReader extends the read deadline of the connection before each read:
// a client that sends nothing for the progress timeout loses its upload
// (slowloris on bodies), a slow one that keeps sending does not.
type progressReader struct {
	r         io.Reader
	rc        *http.ResponseController
	idle      time.Duration
	remaining func() time.Duration
}

// remaining returns how long a token that expires at the Unix time exp (or a
// signed archive URL) is still valid, by the service clock.
func (s *Service) remaining(exp int64) func() time.Duration {
	return func() time.Duration { return time.Unix(exp, 0).Sub(s.now()) }
}

// deadline is the next deadline of a transfer: the progress period from now, but
// never later than the end of the credential that admitted it (a request
// admitted before the expiry ends at the expiry). The clock of the credential is
// the service's, so the remaining time is turned into a real-time deadline.
func deadline(idle time.Duration, remaining func() time.Duration) time.Time {
	d := idle
	if remaining != nil {
		d = max(0, min(d, remaining()))
	}
	return time.Now().Add(d)
}

func newProgressReader(r io.Reader, rc *http.ResponseController, idle time.Duration, remaining func() time.Duration) *progressReader {
	return &progressReader{r: r, rc: rc, idle: idle, remaining: remaining}
}

// set moves the deadlines. The reply to a body that stalled still gets one more
// progress period to be written, but not past the credential's end.
func (p *progressReader) set() {
	_ = p.rc.SetReadDeadline(deadline(p.idle, p.remaining))
	_ = p.rc.SetWriteDeadline(deadline(2*p.idle, p.remaining))
}

// Read: reading is progress, so the reply can wait for the body as long as the
// body keeps coming.
func (p *progressReader) Read(b []byte) (int, error) {
	p.set()
	return p.r.Read(b)
}

// done leaves a deadline in place, never none: after the handler the server
// still drains what is left of the body, and that must not wait for ever either.
// The server resets the deadlines for the next request of the connection.
func (p *progressReader) done() { p.set() }

// progressWriter is the same for downloads: the write deadline moves with
// every write.
type progressWriter struct {
	http.ResponseWriter
	rc        *http.ResponseController
	idle      time.Duration
	remaining func() time.Duration
}

func (p *progressWriter) Write(b []byte) (int, error) {
	_ = p.rc.SetWriteDeadline(deadline(p.idle, p.remaining))
	return p.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (p *progressWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

var _ registryproxy.KeepAliver = (*progressWriter)(nil)

// KeepAlive moves the deadlines without writing. The registry proxy calls it
// while the client waits for a shared upstream download that is progressing,
// before the first byte of the reply. The read deadline moves too: once it
// passes, net/http cancels the request's context and the wait would end.
func (p *progressWriter) KeepAlive() {
	d := deadline(p.idle, p.remaining)
	_ = p.rc.SetReadDeadline(d)
	_ = p.rc.SetWriteDeadline(d)
}

// serveRegistry serves the Docker Hub mirror with the same sliding progress
// deadline as the other downloads: a layer of GiB keeps going as long as it
// moves, and a client that stops reading is cut after one progress period.
// slots caps the requests of this channel in progress at once; one over the
// cap gets 503 at once, which dockerd retries or answers by pulling from
// Docker Hub, so the channel's connections stay free for the cache. quota is
// the channel's share of the proxy's cold downloads. job, when not empty, is
// the channel's job, which the registry logs.
func (s *Service) serveRegistry(job string, slots chan struct{}, quota *registryproxy.Quota) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			reply(w, http.StatusServiceUnavailable, "registry busy")
			return
		}
		rc := http.NewResponseController(w)
		defer func() { _ = rc.SetWriteDeadline(deadline(s.progress, nil)) }()
		ctx := registryproxy.WithQuota(r.Context(), quota)
		if job != "" {
			ctx = registryproxy.WithJob(ctx, job)
		}
		r = r.WithContext(ctx)
		s.registry.ServeHTTP(&progressWriter{ResponseWriter: w, rc: rc, idle: s.progress}, r)
	}
}

func (s *Service) commit(w http.ResponseWriter, r *http.Request, c Claims) {
	id, ok := parseID(r)
	var req struct {
		Size int64 `json:"size"`
	}
	if !ok || json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req) != nil {
		reply(w, http.StatusBadRequest, "bad cache id or size")
		return
	}
	if err := s.store.commit(id, c, req.Size); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// storeError maps the errors of the store. A reservation of another job, ref or
// repository is the same 403 as a bad token; an unknown or expired one is 404.
func (s *Service) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errDenied):
		reply(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, errNotFound):
		reply(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errTooBig):
		reply(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, errBadSize), errors.Is(err, errState):
		reply(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errOverlap):
		reply(w, http.StatusConflict, err.Error())
	case errors.Is(err, errNoRoom), errors.Is(err, errTooMany):
		reply(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.log.Error().Err(err).Msg("cache")
		reply(w, http.StatusInternalServerError, "cache service error")
	}
}

// download serves an archive at the location given by a lookup. The client
// fetches it without the bearer token (downloadUtils.ts:168), so the URL
// carries the proof: an HMAC over the entry, its repository, the job the URL
// was issued to and an expiry. On the channel of a job (job not empty) a URL
// issued to another job is refused, as a token of another job is.
func (s *Service) download(job string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.serveArchive(w, r, job)
	}
}

func (s *Service) serveArchive(w http.ResponseWriter, r *http.Request, channelJob string) {
	id, ok := parseID(r)
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	issued := q.Get("job")
	if !ok || err != nil || issued == "" || (channelJob != "" && issued != channelJob) {
		reply(w, http.StatusForbidden, "forbidden")
		return
	}
	e, found := s.store.get(id)
	if !found || !s.sign.archiveOK(id, e.Repository, issued, exp, q.Get("sig"), s.now()) {
		reply(w, http.StatusForbidden, "forbidden")
		return
	}
	f, e, release, err := s.store.open(id, e.Repository)
	if err != nil {
		reply(w, http.StatusNotFound, "no such cache")
		return
	}
	// The entry cannot be evicted (its bytes stay charged) while it is read.
	defer release()
	defer func() { _ = f.Close() }()
	rc := http.NewResponseController(w)
	remaining := s.remaining(exp)
	defer func() { _ = rc.SetWriteDeadline(deadline(s.progress, remaining)) }()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(&progressWriter{ResponseWriter: w, rc: rc, idle: s.progress, remaining: remaining}, r, "", e.Created, f)
}
