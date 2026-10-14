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
// (ACTIONS_RUNTIME_TOKEN) that is an HMAC, made with a key that exists only in
// the memory of the runner, over its job id, repository, write ref, read refs
// and expiry. The service listens on the gateway address of each sandbox, on one
// port that netpolicy opens to that sandbox only.
package cachesvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"
)

// Defaults.
const (
	// DefaultMax is the size cap, GitHub's limit per repository.
	DefaultMax = 10 << 30
	// DefaultPort is the TCP port on the gateway addresses.
	DefaultPort = 8089
	// EnvMax is ERMITE_RUNNER_CACHE_MAX, the size cap (1G, 500M…).
	EnvMax = "ERMITE_RUNNER_CACHE_MAX"
	// DefaultReservationTTL is how long an upload may stay uncommitted.
	DefaultReservationTTL = time.Hour
	flushEvery            = 30 * time.Second
	// DefaultProgressTimeout is how long an upload or a download may make no
	// progress before its connection is cut.
	DefaultProgressTimeout = 60 * time.Second
	// DefaultConnsPerListener and DefaultConns bound the open connections of one
	// sandbox's listener and of the whole service.
	DefaultConnsPerListener = 16
	DefaultConns            = 64
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

	mu        sync.Mutex
	listeners map[netip.Addr]*listener
	stop      chan struct{}
	wg        sync.WaitGroup
	closed    bool

	hookChunk   func(id int64)
	progress    time.Duration
	perListener int
	connsGlobal chan struct{}
}

type listener struct {
	srv  *http.Server
	done chan struct{}
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
	s := &Service{store: st, sign: sg, now: cfg.Now, log: cfg.Log, listeners: map[netip.Addr]*listener{}, stop: make(chan struct{}),
		hookChunk: cfg.hookChunk, progress: cfg.ProgressTimeout, perListener: cfg.ConnsPerListener, connsGlobal: make(chan struct{}, cfg.Conns)}
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
// should be the job timeout plus a margin.
func (s *Service) Token(jobID string, sc Scope, ttl time.Duration) string {
	return s.sign.token(Claims{JobID: jobID, Scope: sc, Expiry: s.now().Add(ttl).Unix()})
}

// Listen serves the cache on addr:port, an address the caller has configured
// (the gateway of a sandbox). freeBind lets the bind precede the address
// (podman configures the bridge address late, like dnsfwd.Forwarder.FreeBind).
func (s *Service) Listen(ctx context.Context, addr netip.Addr, port uint16, freeBind bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("cache: service closed")
	}
	if _, ok := s.listeners[addr]; ok {
		return fmt.Errorf("cache: already listening on %s", addr)
	}
	var lc net.ListenConfig
	if freeBind {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1) }); err != nil {
				return err
			}
			return serr
		}
	}
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(int(port))))
	if err != nil {
		return fmt.Errorf("cache: listen %s: %w", addr, err)
	}
	return s.serveLocked(addr, ln)
}

func (s *Service) serveLocked(addr netip.Addr, ln net.Listener) error {
	ln = newLimitListener(ln, s.perListener, s.connsGlobal)
	l := &listener{
		srv: &http.Server{
			Handler:           s.Handler(),
			ReadHeaderTimeout: 30 * time.Second,
			// Bodies are chunks of up to tens of MiB and archives of GiB: only
			// idle connections time out.
			IdleTimeout: 2 * time.Minute,
		},
		done: make(chan struct{}),
	}
	s.listeners[addr] = l
	go func() {
		defer close(l.done)
		if err := l.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error().Err(err).Str("addr", addr.String()).Msg("cache listener")
		}
	}()
	return nil
}

// Close stops listening on addr. Unknown addresses are ignored.
func (s *Service) Close(addr netip.Addr) {
	s.mu.Lock()
	l := s.listeners[addr]
	delete(s.listeners, addr)
	s.mu.Unlock()
	if l != nil {
		_ = l.srv.Close()
		<-l.done
	}
}

// CloseAll stops every listener and the housekeeping, and writes the index.
func (s *Service) CloseAll() error {
	s.mu.Lock()
	var addrs []netip.Addr
	for a := range s.listeners {
		addrs = append(addrs, a)
	}
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	for _, a := range addrs {
		s.Close(a)
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
