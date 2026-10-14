// SPDX-License-Identifier: MIT

package cachesvc

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
)

// Entry is a committed cache entry. Reservations are never persisted: a restart
// ends every job, so an unfinished upload is dropped.
type Entry struct {
	ID         int64     `json:"id"`
	Repository string    `json:"repo"`
	Ref        string    `json:"ref"`
	Key        string    `json:"key"`
	Version    string    `json:"version"`
	Size       int64     `json:"size"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used"`
}

// reservation is an upload in progress (POST caches, PATCH, POST caches/{id}).
type reservation struct {
	id        int64
	job       string
	repo, ref string
	key, ver  string
	declared  int64 // cacheSize of the reservation; 0 when the client gave none
	// charged is what the reservation holds of the budget: the declared size
	// (up front), or for a client that gave none the high-water mark of the
	// ranges it has started to write; never less than the minimum charge. It is
	// kept until the reservation is committed or dropped.
	charged int64
	bound   int64 // largest size accepted: declared, or the cache limit
	touched time.Time
	// expires is the absolute end of the reservation: the expiry of the token
	// that made it, or an hour after it was made, whichever comes first. It is
	// enforced besides the idle ttl; a reservation with a chunk in flight is
	// reclaimed once that chunk ends (the transfer deadlines are capped at the
	// token's expiry, so it ends).
	expires time.Time
	ranges  []span // validated byte ranges: fully written, body checked
	active  []span // ranges being written now; they never overlap each other
	// committing is set while the commit renames the file.
	committing bool
}

// span is the half-open byte range [lo, hi).
type span struct{ lo, hi int64 }

// add merges s into the sorted, disjoint list.
func addSpan(list []span, s span) []span {
	out := make([]span, 0, len(list)+1)
	placed := false
	for _, x := range list {
		switch {
		case x.hi < s.lo:
			out = append(out, x)
		case s.hi < x.lo:
			if !placed {
				out, placed = append(out, s), true
			}
			out = append(out, x)
		default: // overlap or touch
			s = span{min(s.lo, x.lo), max(s.hi, x.hi)}
		}
	}
	if !placed {
		out = append(out, s)
	}
	return out
}

// subSpan removes s from the sorted, disjoint list.
func subSpan(list []span, s span) []span {
	var out []span
	for _, x := range list {
		if x.hi <= s.lo || s.hi <= x.lo {
			out = append(out, x)
			continue
		}
		if x.lo < s.lo {
			out = append(out, span{x.lo, s.lo})
		}
		if s.hi < x.hi {
			out = append(out, span{s.hi, x.hi})
		}
	}
	return out
}

func overlaps(list []span, s span) bool {
	for _, x := range list {
		if x.lo < s.hi && s.lo < x.hi {
			return true
		}
	}
	return false
}

// complete reports whether list covers exactly [0, size).
func complete(list []span, size int64) bool {
	return len(list) == 1 && list[0].lo == 0 && list[0].hi == size
}

// Sentinel errors of the store, mapped to HTTP statuses by the handler.
var (
	errConflict = errors.New("a cache entry or reservation with this key and version exists in this ref")
	errTooBig   = errors.New("the cache is larger than the limit of the cache service")
	errNoRoom   = errors.New("no room in the cache: other uploads hold the space")
	errNotFound = errors.New("no such cache reservation")
	errDenied   = errForbidden
	errBadSize  = errors.New("the uploaded data does not match the size")
	errState    = errors.New("the cache reservation is already being committed")
	errOverlap  = errors.New("this range is being uploaded already")
	errTooMany  = errors.New("too many cache uploads in progress")
)

// Limits of the store (storeConfig zero values take these).
const (
	// DefaultMinCharge is charged against the budget for every committed entry
	// and reservation however small: a million one-byte entries are not free
	// (index, inodes). With the 10 GiB default it caps the entries at ~2.6M.
	DefaultMinCharge = 4 << 10
	// DefaultMaxPending bounds the uploads in progress, in all and per job.
	DefaultMaxPending       = 256
	DefaultMaxPendingPerJob = 32
)

type storeConfig struct {
	dir        string
	max        int64
	ttl        time.Duration // idle life of a reservation that is never committed
	now        func() time.Time
	minCharge  int64
	maxPending int
	maxPerJob  int
	// maxLife bounds the life of a reservation from its creation.
	maxLife time.Duration
	log     zerowrap.Logger
}

// store keeps the index and the files under dir.
//
// Budget: used (the charge of the committed entries) + held (the charge of the
// reservations) never exceeds max once admitted. Committing moves a
// reservation's charge to its entry, so it needs no room of its own. Entries
// with a download in progress (pins) are not evicted: their bytes cannot be
// reclaimed until the last reader is done, and reservations fail with errNoRoom
// meanwhile.
type store struct {
	storeConfig

	mu      sync.Mutex
	entries map[int64]*Entry
	pending map[int64]*reservation
	pins    map[int64]int // open downloads per entry
	used    int64
	held    int64
	dirty   bool // lastUsed changed since the index was written
}

func (s *store) chargeOf(size int64) int64 { return max(size, s.minCharge) }

const indexName = "index.json"

func openStore(c storeConfig) (*store, error) {
	if c.minCharge <= 0 {
		c.minCharge = DefaultMinCharge
	}
	if c.maxPending <= 0 {
		c.maxPending = DefaultMaxPending
	}
	if c.maxPerJob <= 0 {
		c.maxPerJob = DefaultMaxPendingPerJob
	}
	if c.maxLife <= 0 {
		c.maxLife = DefaultReservationTTL
	}
	dir := c.dir
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	s := &store{storeConfig: c, entries: map[int64]*Entry{}, pending: map[int64]*reservation{}, pins: map[int64]int{}}
	b, err := os.ReadFile(filepath.Join(dir, indexName))
	if err == nil {
		var list []*Entry
		if err := json.Unmarshal(b, &list); err != nil {
			// A corrupt index is not worth refusing to start for: the cache is a
			// cache. Start empty; the orphan files are removed below.
			list = nil
		}
		for _, e := range list {
			if st, err := os.Stat(s.file(e.ID)); err == nil && st.Mode().IsRegular() && st.Size() == e.Size && s.entries[e.ID] == nil {
				s.entries[e.ID] = e
				s.used += s.chargeOf(e.Size)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cache: %w", err)
	}
	// Anything in data/ that the index does not know (temporary files of
	// reservations, files of entries lost from the index) goes.
	des, err := os.ReadDir(filepath.Join(dir, "data"))
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	for _, de := range des {
		id, perr := strconv.ParseInt(strings.TrimSuffix(de.Name(), ".tmp"), 16, 64)
		if perr != nil || strings.HasSuffix(de.Name(), ".tmp") || s.entries[id] == nil {
			_ = os.Remove(filepath.Join(dir, "data", de.Name()))
		}
	}
	s.evictLocked(0)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) file(id int64) string {
	return filepath.Join(s.dir, "data", strconv.FormatInt(id, 16))
}
func (s *store) tmp(id int64) string { return s.file(id) + ".tmp" }

// saveLocked writes the index atomically and durably: the temporary file is
// synced before the rename and the directory after it.
func (s *store) saveLocked() error {
	list := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, indexName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cache index: %w", err)
	}
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cache index: %w", werr)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, indexName)); err != nil {
		return fmt.Errorf("cache index: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("cache index: %w", err)
	}
	s.dirty = false
	return nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// flush writes the index when only usage times changed.
func (s *store) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

// newID is a random positive integer below 2^53: the toolkit reads cacheId as a
// JavaScript number, and ids that cannot be guessed keep one job from writing
// into another job's reservation.
func (s *store) newID() int64 {
	for {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := int64(binary.BigEndian.Uint64(b[:]) & (1<<53 - 1))
		if id == 0 || s.entries[id] != nil || s.pending[id] != nil {
			continue
		}
		return id
	}
}

// expireLocked drops reservations that saw no activity for the ttl and have no
// chunk in flight.
func (s *store) expireLocked() {
	now := s.now()
	for _, r := range s.pending {
		if !r.committing && len(r.active) == 0 && (now.Sub(r.touched) > s.ttl || !now.Before(r.expires)) {
			s.dropLocked(r)
		}
	}
}

func (s *store) dropLocked(r *reservation) {
	s.held -= r.charged
	delete(s.pending, r.id)
	_ = os.Remove(s.tmp(r.id))
}

// evictLocked removes least recently used committed entries (not those being
// downloaded) until need more bytes fit next to what is used and held. It
// reports whether they fit and how many entries it removed.
func (s *store) evictLocked(need int64) (fits bool, evicted int) {
	if s.used+s.held+need <= s.max {
		return true, 0
	}
	list := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if s.pins[e.ID] == 0 {
			list = append(list, e)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].LastUsed.Equal(list[j].LastUsed) {
			return list[i].LastUsed.Before(list[j].LastUsed)
		}
		return list[i].ID < list[j].ID
	})
	for _, e := range list {
		if s.used+s.held+need <= s.max {
			break
		}
		delete(s.entries, e.ID)
		s.used -= s.chargeOf(e.Size)
		_ = os.Remove(s.file(e.ID))
		evicted++
		// The index still names it until it is saved; a failed save leaves this
		// set so that the housekeeping tries again.
		s.dirty = true
	}
	return s.used+s.held+need <= s.max, evicted
}

// expire drops the reservations that were idle for too long.
func (s *store) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
}

// roomLocked makes need more bytes fit, evicting if it must, and writes the index when
// it evicted (the files are gone). Timestamps alone wait for the housekeeping.
func (s *store) roomLocked(need int64) bool {
	fits, n := s.evictLocked(need)
	if n > 0 {
		if err := s.saveLocked(); err != nil {
			s.log.Error().Err(err).Msg("cache index (after evicting); will retry")
		}
	}
	return fits
}

// reserve starts an upload for (repo, ref, key, version). declared is the
// cacheSize of the client, 0 if none.
func (s *store) reserve(job, repo, ref, key, version string, declared int64, tokenExp time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if declared > s.max {
		return 0, errTooBig
	}
	for _, e := range s.entries {
		if e.Repository == repo && e.Ref == ref && e.Key == key && e.Version == version {
			return 0, errConflict
		}
	}
	perJob := 0
	for _, r := range s.pending {
		if r.repo == repo && r.ref == ref && r.key == key && r.ver == version {
			return 0, errConflict
		}
		if r.job == job {
			perJob++
		}
	}
	// Before anything is created.
	if len(s.pending) >= s.maxPending || perJob >= s.maxPerJob {
		return 0, errTooMany
	}
	// A client that announces its size is charged for it now; one that does not
	// is charged as its chunks arrive (beginChunk).
	bound, charge := declared, s.chargeOf(declared)
	if declared == 0 {
		bound = s.max
	}
	if !s.roomLocked(charge) {
		return 0, errNoRoom
	}
	id := s.newID()
	f, err := os.OpenFile(s.tmp(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("cache: %w", err)
	}
	_ = f.Close()
	s.pending[id] = &reservation{id: id, job: job, repo: repo, ref: ref, key: key, ver: version, declared: declared, charged: charge, bound: bound, touched: s.now(), expires: s.reservationEnd(tokenExp)}
	s.held += charge
	return id, nil
}

// reservationEnd is the absolute end of a reservation made now with a token that
// expires at tokenExp.
func (s *store) reservationEnd(tokenExp time.Time) time.Time {
	end := s.now().Add(s.maxLife)
	if tokenExp.Before(end) {
		end = tokenExp
	}
	return end
}

// owned returns the reservation id if it belongs to this job: same job, same
// repository, and the ref is the job's write ref.
func (s *store) ownedLocked(id int64, c Claims) (*reservation, error) {
	r := s.pending[id]
	if r == nil {
		return nil, errNotFound
	}
	if r.job != c.JobID || r.repo != c.Repository || c.WriteRef == "" || r.ref != c.WriteRef {
		return nil, errDenied
	}
	if !s.now().Before(r.expires) {
		// Over: nothing more is accepted. It is dropped as soon as no chunk is in
		// flight (expireLocked).
		return nil, errNotFound
	}
	return r, nil
}

// beginChunk admits a chunk [lo, hi) of reservation id and returns the path to
// write it to. The chunk is charged first: a reservation without a declared size
// is charged up to hi (evicting to make room, or failing with errNoRoom). The
// range must not be in flight already, and any coverage it had is removed: a
// retry that fails halfway must not leave the old bytes counted as valid.
// endChunk must follow.
func (s *store) beginChunk(id int64, c Claims, lo, hi int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	r, err := s.ownedLocked(id, c)
	if err != nil {
		return "", err
	}
	if r.committing {
		return "", errState
	}
	if hi > r.bound {
		return "", errTooBig
	}
	sp := span{lo, hi}
	if overlaps(r.active, sp) {
		return "", errOverlap
	}
	if extra := s.chargeOf(hi) - r.charged; extra > 0 {
		if !s.roomLocked(extra) {
			return "", errNoRoom
		}
		r.charged += extra
		s.held += extra
	}
	r.ranges = subSpan(r.ranges, sp)
	r.active = append(r.active, sp)
	r.touched = s.now()
	return s.tmp(id), nil
}

// endChunk ends the chunk begun by beginChunk; only when the whole body was
// written and checked does [lo, hi) count as received.
func (s *store) endChunk(id int64, lo, hi int64, written bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.pending[id]
	if r == nil {
		return
	}
	r.active = subSpan(r.active, span{lo, hi})
	r.touched = s.now()
	if written {
		r.ranges = addSpan(r.ranges, span{lo, hi})
	}
	// A reservation that ran out while this chunk was in flight is reclaimed now
	// that its last write is over.
	s.expireLocked()
}

// commit makes reservation id an entry once exactly [0, size) was received.
func (s *store) commit(id int64, c Claims, size int64) error {
	s.mu.Lock()
	s.expireLocked()
	r, err := s.ownedLocked(id, c)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if r.committing || len(r.active) > 0 {
		s.mu.Unlock()
		return errState
	}
	if size <= 0 || (r.declared != 0 && size != r.declared) || !complete(r.ranges, size) {
		s.mu.Unlock()
		return errBadSize
	}
	r.committing = true
	s.mu.Unlock()

	// The data is synced and the file is the right length before it takes its
	// final name, and the directory is synced before the index names the file:
	// readers only ever see whole entries, and the index never points at a file
	// that a crash can lose.
	err = syncTruncate(s.tmp(id), size)
	if err == nil {
		err = os.Rename(s.tmp(id), s.file(id))
	}
	if err == nil {
		err = syncDir(filepath.Join(s.dir, "data"))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPendingKeepFileLocked(r)
	if err != nil {
		_ = os.Remove(s.tmp(id))
		_ = os.Remove(s.file(id))
		return fmt.Errorf("cache: %w", err)
	}
	now := s.now()
	s.entries[id] = &Entry{ID: id, Repository: r.repo, Ref: r.ref, Key: r.key, Version: r.ver, Size: size, Created: now, LastUsed: now}
	// The reservation's charge becomes the entry's: size == hwm of the chunks.
	s.used += s.chargeOf(size)
	s.dirty = true // stays set if the save fails: the housekeeping retries
	return s.saveLocked()
}

// dropPendingKeepFileLocked releases a reservation without touching its file.
func (s *store) dropPendingKeepFileLocked(r *reservation) {
	s.held -= r.charged
	delete(s.pending, r.id)
}

func syncTruncate(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// find is the cache lookup, as GitHub documents it ("Matching a cache key"):
// the read refs are searched in priority order, and in each ref the keys in
// order: the first (primary) key exactly, the restore keys as an exact match or
// else the newest entry whose key starts with them. The match counts as a use.
func (s *store) find(repo string, refs []string, keys []string, version string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range refs {
		for i, k := range keys {
			var best *Entry
			for _, e := range s.entries {
				if e.Repository != repo || e.Ref != ref || e.Version != version {
					continue
				}
				if e.Key == k {
					best = e
					break
				}
				if i == 0 || !strings.HasPrefix(e.Key, k) {
					continue
				}
				if best == nil || e.Created.After(best.Created) || (e.Created.Equal(best.Created) && e.ID > best.ID) {
					best = e
				}
			}
			if best != nil {
				best.LastUsed = s.now()
				s.dirty = true
				return *best, true
			}
		}
	}
	return Entry{}, false
}

// list returns the entries of the repository in the refs whose key starts with
// prefix, newest first.
func (s *store) list(repo string, refs []string, prefix string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries {
		if e.Repository == repo && strings.HasPrefix(e.Key, prefix) && contains(refs, e.Ref) {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// open returns the committed entry id for reading, counts it as a use and pins
// it until release is called: a pinned entry is not evicted.
func (s *store) open(id int64, repo string) (f *os.File, e Entry, release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ent := s.entries[id]
	if ent == nil || ent.Repository != repo {
		return nil, Entry{}, nil, errNotFound
	}
	f, err = os.Open(s.file(id))
	if err != nil {
		return nil, Entry{}, nil, errNotFound
	}
	ent.LastUsed = s.now()
	s.dirty = true
	s.pins[id]++
	var once sync.Once
	return f, *ent, func() {
		once.Do(func() {
			s.mu.Lock()
			if s.pins[id]--; s.pins[id] <= 0 {
				delete(s.pins, id)
			}
			s.mu.Unlock()
		})
	}, nil
}

// get returns a committed entry without counting a use.
func (s *store) get(id int64) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		return *e, true
	}
	return Entry{}, false
}

// stats are for tests and logs.
func (s *store) stats() (entries int, used, held int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), s.used, s.held
}

func (s *store) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}
