// SPDX-License-Identifier: MIT

// Package actionscache is the host-side cache of action archives (plan CI-4,
// D8). The host downloads the tarball of an action once per (repository,
// resolved commit) and every job, in any sandbox, is served the same bytes
// through the cache channel (see cachesvc): the forge is hit once, not once per
// job (codeload answered 429 during the CI-3 compare).
//
// An entry is keyed by Key: a hash of the forge, the repository and the resolved
// commit sha. A mutable ref (a tag, a branch) is never part of it, so a moved tag
// cannot serve stale bytes: the server resolves the ref to a sha at each job.
//
// The bytes come from the host's own download, never from a sandbox: the guest
// runs untrusted code and must not be able to plant an archive that other jobs
// would run. Each entry is two files in Dir, <key>.tgz and <key>.sum (the
// sha256 and the size of the archive). The sum is written after the archive, so
// an archive without one is a crash leftover and is removed at start. Every read
// re-hashes the archive; a mismatch drops the entry and the caller downloads it
// again.
//
// The total size is capped; the least recently used entries go first. An entry
// handed out is pinned for a while (PinFor), so that it cannot be evicted
// between the host fetching it and the sandbox reading it.
package actionscache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
)

// Defaults and names.
const (
	// DefaultMax is the size cap when ERMITE_RUNNER_ACTIONS_CACHE_MAX is unset.
	DefaultMax = 2 << 30
	// EnvMax is ERMITE_RUNNER_ACTIONS_CACHE_MAX: a size (2G, 500M…); 0 turns the
	// cache off.
	EnvMax = "ERMITE_RUNNER_ACTIONS_CACHE_MAX"
	// PinFor is how long a served entry is protected from eviction.
	PinFor = 30 * time.Minute

	archiveExt = ".tgz"
	sumExt     = ".sum"
	tmpPrefix  = "tmp-"
)

// ErrNotFound is an entry that is absent, corrupt or evicted.
var ErrNotFound = errors.New("actionscache: no such entry")

// ErrTooLarge is an archive that does not fit under the cap on its own.
var ErrTooLarge = errors.New("actionscache: archive larger than the cache")

// Config configures a Cache.
type Config struct {
	Dir string
	// Max is the size cap in bytes, > 0.
	Max int64
	Now func() time.Time
	Log zerowrap.Logger
}

type entry struct {
	size     int64
	used     time.Time
	pinUntil time.Time
}

type flight struct {
	done chan struct{}
	err  error
}

// Cache is the archive cache.
type Cache struct {
	dir string
	max int64
	now func() time.Time
	log zerowrap.Logger

	mu      sync.Mutex
	entries map[string]*entry
	total   int64
	pending int64 // bytes of downloads in progress, charged as they are written
	flights map[string]*flight
}

// New opens (and creates) dir, forgetting what a crash left half written.
func New(cfg Config) (*Cache, error) {
	if cfg.Max <= 0 {
		return nil, errors.New("actionscache: the cap must be positive")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("actionscache: %w", err)
	}
	c := &Cache{dir: cfg.Dir, max: cfg.Max, now: cfg.Now, log: cfg.Log, entries: map[string]*entry{}, flights: map[string]*flight{}}
	des, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("actionscache: %w", err)
	}
	for _, de := range des {
		n := de.Name()
		switch {
		case strings.HasPrefix(n, tmpPrefix):
			_ = os.Remove(filepath.Join(cfg.Dir, n))
		case strings.HasSuffix(n, sumExt):
			key := strings.TrimSuffix(n, sumExt)
			sfi, err1 := de.Info()
			afi, err2 := os.Stat(c.archivePath(key))
			if !validKey(key) || err1 != nil || err2 != nil {
				c.remove(key)
				continue
			}
			c.entries[key] = &entry{size: afi.Size(), used: sfi.ModTime()}
			c.total += afi.Size()
		case strings.HasSuffix(n, archiveExt):
			if _, err := os.Stat(filepath.Join(cfg.Dir, strings.TrimSuffix(n, archiveExt)+sumExt)); err != nil {
				_ = os.Remove(filepath.Join(cfg.Dir, n))
			}
		}
	}
	c.mu.Lock()
	c.evictLocked("", 0)
	c.mu.Unlock()
	return c, nil
}

// Key is the entry key of an archive: the sha256 of the forge origin, the
// repository and the resolved commit. ok is false when sha is not a full git
// object name (40 or 64 hex digits): such an archive is not cached, whatever the
// ref said.
func Key(origin, repo, sha string) (key string, ok bool) {
	if origin == "" || repo == "" || (len(sha) != 40 && len(sha) != 64) {
		return "", false
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", false
	}
	h := sha256.New()
	for _, p := range []string{origin, strings.ToLower(repo), strings.ToLower(sha)} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func validKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	_, err := hex.DecodeString(k)
	return err == nil
}

// ValidKey reports whether k has the shape of a Key.
func ValidKey(k string) bool { return validKey(k) }

func (c *Cache) archivePath(key string) string { return filepath.Join(c.dir, key+archiveExt) }
func (c *Cache) sumPath(key string) string     { return filepath.Join(c.dir, key+sumExt) }

func (c *Cache) remove(key string) {
	_ = os.Remove(c.sumPath(key))
	_ = os.Remove(c.archivePath(key))
}

// Fetch writes an archive to w; it is called at most once at a time per key.
type Fetch func(ctx context.Context, w io.Writer) error

// Ensure makes sure the entry of key exists and is intact, calling fetch to
// download it when it does not (a miss: hit is false). Concurrent calls for the
// same key share one download; the others report a hit once it is in place. The
// entry is pinned for PinFor.
func (c *Cache) Ensure(ctx context.Context, key string, fetch Fetch) (hit bool, verify time.Duration, err error) {
	if !validKey(key) {
		return false, verify, ErrNotFound
	}
	for {
		duration, err := c.verify(key)
		verify += duration
		if err == nil {
			c.touch(key, true)
			return true, verify, nil
		}
		c.mu.Lock()
		if f, ok := c.flights[key]; ok {
			c.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return false, verify, ctx.Err()
			}
			if f.err != nil {
				return false, verify, f.err
			}
			continue
		}
		f := &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.mu.Unlock()
		f.err = c.fill(ctx, key, fetch)
		c.mu.Lock()
		delete(c.flights, key)
		c.mu.Unlock()
		close(f.done)
		if f.err != nil {
			return false, verify, f.err
		}
		c.touch(key, true)
		return false, verify, nil
	}
}

// fill downloads into a temporary file, then renames it into place: a reader
// sees a whole archive with its sum or nothing.
func (c *Cache) fill(ctx context.Context, key string, fetch Fetch) error {
	f, err := os.CreateTemp(c.dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h), c: c}
	// The bytes of the temporary file are charged while they are written and
	// released when it is gone (renamed into an entry, or removed).
	defer func() {
		c.mu.Lock()
		c.pending -= cw.n
		c.mu.Unlock()
	}()
	err = fetch(ctx, cw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil)) + " " + strconv.FormatInt(cw.n, 10)
	// A corrupt older entry of this key is replaced.
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.total -= old.size
		delete(c.entries, key)
	}
	c.remove(key)
	if err := os.Rename(tmp, c.archivePath(key)); err != nil {
		return err
	}
	stmp := c.sumPath(key) + ".new"
	if err := os.WriteFile(stmp, []byte(sum), 0o600); err != nil {
		_ = os.Remove(c.archivePath(key))
		return err
	}
	if err := os.Rename(stmp, c.sumPath(key)); err != nil {
		_ = os.Remove(stmp)
		_ = os.Remove(c.archivePath(key))
		return err
	}
	c.entries[key] = &entry{size: cw.n, used: c.now()}
	c.total += cw.n
	c.pending -= cw.n // now counted in total
	cw.n = 0
	return nil
}

// countWriter charges every byte it writes to the cache before it reaches the
// disk (the size is what is written, never a declared length): when the entries
// and the downloads in progress would exceed the cap, the least recently used
// unpinned entries make room, and if that is not enough the write fails with
// ErrTooLarge, so an archive bigger than the cap is never stored and the
// cache, temporary files included, never holds more than the cap.
type countWriter struct {
	w io.Writer
	c *Cache
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	c := w.c
	c.mu.Lock()
	need := int64(len(p))
	if c.total+c.pending+need > c.max {
		c.evictLocked("", need)
	}
	if c.total+c.pending+need > c.max {
		c.mu.Unlock()
		return 0, ErrTooLarge
	}
	c.pending += need
	w.n += need
	c.mu.Unlock()
	n, err := w.w.Write(p)
	if n < len(p) { // short write: give back what did not reach the file
		c.mu.Lock()
		c.pending -= int64(len(p) - n)
		w.n -= int64(len(p) - n)
		c.mu.Unlock()
	}
	return n, err
}

// verify re-hashes the archive of key against its sum. Any mismatch (or a
// missing file) drops the entry and returns ErrNotFound.
func (c *Cache) verify(key string) (time.Duration, error) {
	start := time.Now()
	sb, err := os.ReadFile(c.sumPath(key))
	if err != nil {
		c.drop(key)
		return time.Since(start), ErrNotFound
	}
	want, sizeS, _ := strings.Cut(strings.TrimSpace(string(sb)), " ")
	size, perr := strconv.ParseInt(sizeS, 10, 64)
	f, err := os.Open(c.archivePath(key))
	if err != nil || perr != nil {
		if f != nil {
			_ = f.Close()
		}
		c.drop(key)
		return time.Since(start), ErrNotFound
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != want {
		c.log.Warn().Str("key", key).Msg("action archive failed its integrity check; dropped")
		c.drop(key)
		return time.Since(start), ErrNotFound
	}
	return time.Since(start), nil
}

func (c *Cache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.total -= e.size
		delete(c.entries, key)
	}
	if _, filling := c.flights[key]; !filling {
		c.remove(key)
	}
}

// touch records a use (and a pin) of key.
func (c *Cache) touch(key string, pin bool) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		e.used = now
		if pin {
			e.pinUntil = now.Add(PinFor)
		}
	}
	c.mu.Unlock()
	_ = os.Chtimes(c.sumPath(key), now, now)
}

// evictLocked removes the least recently used entries until the cache fits,
// leaving keep and the pinned ones alone.
func (c *Cache) evictLocked(keep string, extra int64) {
	if c.total+c.pending+extra <= c.max {
		return
	}
	now := c.now()
	var order []string
	for k, e := range c.entries {
		if k != keep && !now.Before(e.pinUntil) {
			order = append(order, k)
		}
	}
	sort.Slice(order, func(i, j int) bool { return c.entries[order[i]].used.Before(c.entries[order[j]].used) })
	for _, k := range order {
		if c.total+c.pending+extra <= c.max {
			return
		}
		c.total -= c.entries[k].size
		delete(c.entries, k)
		c.remove(k)
	}
}

// Open returns the verified archive of key. The caller closes it.
func (c *Cache) Open(key string) (*os.File, error) {
	if !validKey(key) {
		return nil, ErrNotFound
	}
	if _, err := c.verify(key); err != nil {
		return nil, err
	}
	f, err := os.Open(c.archivePath(key))
	if err != nil {
		c.drop(key)
		return nil, ErrNotFound
	}
	c.touch(key, false)
	return f, nil
}

// Size is the bytes held (tests, logs).
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Dir is the directory of the cache (tests).
func (c *Cache) Dir() string { return c.dir }
