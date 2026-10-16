// SPDX-License-Identifier: MIT

// Package cachesvc is the GitHub Actions cache service (API v1) of the runner
// (plan CI-3, D8). Actions that use @actions/cache (setup-go, setup-node,
// actions/cache…) talk to it through ACTIONS_CACHE_URL: the toolkit takes the v1
// protocol for any server URL that is not github.com, *.ghe.com or *.localhost
// (toolkit/packages/cache/src/internal/config.ts:1-20).
//
// One Service per runner host. Entries live under <state>/cache/ (files plus a
// JSON index, atomic finalisation, LRU eviction by total size) and are scoped by
// (repository, ref, key, version). A job reaches the service with a bearer token
// (ACTIONS_RUNTIME_TOKEN): a JWT signed with HS256 under a key that exists only
// in the memory of the runner, over its job id, repository, write ref, read
// refs, expiry and scopes. The same token serves the results service of the
// job (package resultssvc), which the channel of a sandbox carries beside the
// cache (WithResults).
//
// The service never listens on a network. Each sandbox has a channel of its own
// (Serve): a Unix socket, bind-mounted into a container or reached through
// Firecracker's vsock proxy for a VM, on which the relay in the guest delivers
// the connections of the actions, which reach it on the guest's loopback
// (DefaultPort). The host opens no port and its firewall has no rule for it.
package cachesvc

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/actionscache"
)

// Defaults.
const (
	// DefaultMax is the size cap, GitHub's limit per repository.
	DefaultMax = 10 << 30
	// DefaultPort is the TCP port of the cache on the loopback of the guest
	// (ACTIONS_CACHE_URL=http://127.0.0.1:9089/), and the vsock port of the
	// cache streams of a VM. It is not a port of the host.
	DefaultPort = 9089
	// EnvMax is ERMITE_RUNNER_CACHE_MAX, the size cap (1G, 500M…).
	EnvMax = "ERMITE_RUNNER_CACHE_MAX"
	// DefaultReservationTTL is how long an upload may stay uncommitted.
	DefaultReservationTTL = time.Hour
	flushEvery            = 30 * time.Second
	// DefaultProgressTimeout is how long an upload or a download may make no
	// progress before its connection is cut.
	DefaultProgressTimeout = 60 * time.Second
	// DefaultConnsPerListener and DefaultConns bound the open connections of one
	// sandbox's listener and of the whole service. upload-artifact sends 5
	// blocks at once by default, besides downloads and the cache.
	DefaultConnsPerListener = 32
	DefaultConns            = 128
)

// Config configures a Service.
type Config struct {
	// Dir is the cache directory (<state>/cache).
	Dir string
	// Max is the size cap in bytes; 0 means DefaultMax.
	Max int64
	// ReservationTTL is the life of an uncommitted reservation; 0 means
	// DefaultReservationTTL.
	ReservationTTL time.Duration
	// ReservationMaxLife is the absolute life of a reservation from its creation
	// (the token's expiry still ends it sooner); 0 means DefaultReservationTTL.
	ReservationMaxLife time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	Log zerowrap.Logger
	// Archives, when not nil, is the host's cache of action archives, served on
	// the channels (see ArchiveURL).
	Archives *actionscache.Cache
	// The limits below take the Default… values (or those of the store) when
	// zero; tests set them.
	ProgressTimeout              time.Duration
	ConnsPerListener, Conns      int
	MinCharge                    int64
	MaxPending, MaxPendingPerJob int
	// hookChunk, when set, is called by an upload once its range is admitted and
	// before its body is read (tests: a barrier with a chunk in flight).
	hookChunk func(id int64)
}

// Service is the cache service.
type Service struct {
	store *store
	sign  *signer
	now   func() time.Time
	log   zerowrap.Logger

	mu       sync.Mutex
	channels map[string]*listener
	stop     chan struct{}
	wg       sync.WaitGroup
	closed   bool

	archives    *actionscache.Cache
	hookChunk   func(id int64)
	progress    time.Duration
	perListener int
	connsGlobal chan struct{}
}

type listener struct {
	srv  *http.Server
	done chan struct{}
	bw   *bandwidth // the channel's limiter; nil when unlimited (read by tests)
}

// New opens the store and starts the housekeeping goroutine; Close stops it.
func New(cfg Config) (*Service, error) {
	if cfg.Max <= 0 {
		cfg.Max = DefaultMax
	}
	if cfg.ReservationTTL <= 0 {
		cfg.ReservationTTL = DefaultReservationTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	sg, err := newSigner()
	if err != nil {
		return nil, err
	}
	st, err := openStore(storeConfig{dir: cfg.Dir, max: cfg.Max, ttl: cfg.ReservationTTL, now: cfg.Now,
		minCharge: cfg.MinCharge, maxPending: cfg.MaxPending, maxPerJob: cfg.MaxPendingPerJob, maxLife: cfg.ReservationMaxLife, log: cfg.Log})
	if err != nil {
		return nil, err
	}
	if cfg.ProgressTimeout <= 0 {
		cfg.ProgressTimeout = DefaultProgressTimeout
	}
	if cfg.ConnsPerListener <= 0 {
		cfg.ConnsPerListener = DefaultConnsPerListener
	}
	if cfg.Conns <= 0 {
		cfg.Conns = DefaultConns
	}
	s := &Service{store: st, sign: sg, now: cfg.Now, log: cfg.Log, channels: map[string]*listener{}, stop: make(chan struct{}),
		archives: cfg.Archives, hookChunk: cfg.hookChunk, progress: cfg.ProgressTimeout, perListener: cfg.ConnsPerListener, connsGlobal: make(chan struct{}, cfg.Conns)}
	s.wg.Add(1)
	go s.housekeeping()
	return s, nil
}

// housekeeping writes the usage times of the index and drops stale reservations.
func (s *Service) housekeeping() {
	defer s.wg.Done()
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.store.expire()
			if err := s.store.flush(); err != nil {
				s.log.Warn().Err(err).Msg("cache index")
			}
		}
	}
}

// Token returns the ACTIONS_RUNTIME_TOKEN of a job. It is valid for ttl, which
// should be the job timeout plus a margin. sc is the job's cache scope (the
// zero Scope for a job without a cache: the cache then refuses the token);
// scopes are added to its scp claim (resultssvc.Scope for a job with the
// results service).
func (s *Service) Token(jobID string, sc Scope, ttl time.Duration, scopes ...string) string {
	now := s.now()
	exp := now.Add(ttl).Unix()
	return s.sign.token(Claims{JobID: jobID, Scope: sc, Expiry: exp, ExpiresAt: exp, IssuedAt: now.Unix(),
		Scopes: strings.Join(append([]string{genericReadScope}, scopes...), " ")})
}

// RuntimeToken checks a runtime token for the results service (resultssvc.Auth):
// made by this runner and not expired, whatever its cache scope. It returns
// the job, the scp claim and the expiry.
func (s *Service) RuntimeToken(tok string) (jobID, scopes string, expiry time.Time, err error) {
	c, err := s.sign.verifyAny(tok, s.now())
	if err != nil {
		return "", "", time.Time{}, err
	}
	return c.JobID, c.Scopes, time.Unix(c.Expiry, 0), nil
}

// BlobMAC signs the blob URLs of the results service (resultssvc.Auth), under
// a key domain of their own.
func (s *Service) BlobMAC(parts ...[]byte) []byte {
	return s.sign.mac("blob", parts...)
}

// start serves HTTP on ln, subject to the connection caps, until the listener
// is closed. job, when not empty, restricts the tokens accepted to that job's.
func (s *Service) start(name string, ln net.Listener, job string, bw *bandwidth, results http.Handler) *listener {
	if bw != nil {
		ln = throttleListener{ln, bw}
	}
	ln = newLimitListener(ln, s.perListener, s.connsGlobal)
	l := &listener{
		srv: &http.Server{
			Handler:           s.channelHandler(job, results),
			ReadHeaderTimeout: 30 * time.Second,
			// Bodies are chunks of up to tens of MiB and archives of GiB: only
			// idle connections time out.
			IdleTimeout: 2 * time.Minute,
		},
		done: make(chan struct{}),
		bw:   bw,
	}
	go func() {
		defer close(l.done)
		if err := l.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error().Err(err).Str("listener", name).Msg("cache listener")
		}
	}()
	return l
}

// Serve serves the cache on ln, the host end of the channel of one sandbox
// (id): a listener that is not on the network, such as the Unix socket on which
// the relay of that sandbox's guest delivers its connections. Each sandbox has
// its own listener and so its own handler, and nothing but that sandbox's guest
// can connect to it. job is the id of the job the sandbox runs: only tokens made
// for it are accepted on this channel, besides the checks of every token. The
// connection caps apply as on any listener, and so does WithBandwidth when given:
// one limiter per call, shared by all the connections of the channel, so that
// the sandbox's cache traffic as a whole stays under its rate. Serve takes
// ownership of ln: Stop (or CloseAll) closes it, and so does a failed Serve.
func (s *Service) Serve(id string, ln net.Listener, job string, opts ...ServeOption) error {
	var sc serveConfig
	for _, o := range opts {
		o(&sc)
	}
	if job == "" {
		_ = ln.Close()
		return errors.New("cache: a channel needs the job it serves")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = ln.Close()
		return errors.New("cache: service closed")
	}
	if _, ok := s.channels[id]; ok {
		_ = ln.Close()
		return fmt.Errorf("cache: already serving %s", id)
	}
	s.channels[id] = s.start(id, ln, job, newBandwidth(sc.bytesPerSec), sc.results)
	return nil
}

// Stop ends the service of the channel of sandbox id, closing its listener and
// every connection. Unknown ids are ignored.
func (s *Service) Stop(id string) {
	s.mu.Lock()
	l := s.channels[id]
	delete(s.channels, id)
	s.mu.Unlock()
	if l != nil {
		_ = l.srv.Close()
		<-l.done
	}
}

// CloseAll stops every channel and the housekeeping, and writes the index.
func (s *Service) CloseAll() error {
	s.mu.Lock()
	var ids []string
	for id := range s.channels {
		ids = append(ids, id)
	}
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	for _, id := range ids {
		s.Stop(id)
	}
	if already {
		return nil
	}
	close(s.stop)
	s.wg.Wait()
	return s.store.flush()
}

// ParseMax reads ERMITE_RUNNER_CACHE_MAX: a number of bytes or a size with a K,
// M, G or T suffix (optionally followed by iB or B). Empty means DefaultMax.
func ParseMax(v string) (int64, error) {
	if v == "" {
		return DefaultMax, nil
	}
	s := []byte(v)
	for len(s) > 0 && (s[len(s)-1] == 'B' || s[len(s)-1] == 'b' || s[len(s)-1] == 'i') {
		s = s[:len(s)-1]
	}
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k', 'K':
			mult, s = 1<<10, s[:n-1]
		case 'm', 'M':
			mult, s = 1<<20, s[:n-1]
		case 'g', 'G':
			mult, s = 1<<30, s[:n-1]
		case 't', 'T':
			mult, s = 1<<40, s[:n-1]
		}
	}
	n, err := strconv.ParseInt(string(s), 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("%s: %q is not a size (examples: 10G, 500M)", EnvMax, v)
	}
	return n * mult, nil
}
