// SPDX-License-Identifier: MIT

package registryproxy

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bnema/zerowrap"
)

var errTooLarge = errors.New("registry cache capacity exceeded")
var errDigest = errors.New("registry digest mismatch")

// errBusy: no download slot came free within the slot wait. The proxy answers
// 503, and dockerd retries the layer (about 50 s) before pulling from Docker Hub.
var errBusy = errors.New("registry downloads busy")

const tagCharge int64 = 4096

// lowWater bounds the extra room an eviction frees (with 5% of Max), so that a
// full store evicts once per that many bytes, not on every write.
const lowWater int64 = 64 << 20

type entry struct {
	key    string
	size   int64
	used   time.Time
	pins   int
	digest string        // tag target, empty for content
	elem   *list.Element // in store.lru; nil once removed
}

// flight is one download, shared by every request for its digest. Its fields
// below written are guarded by store.mu.
type flight struct {
	key     string
	done    chan struct{}
	err     error
	written atomic.Int64 // bytes received so far, for the waiters' keepalive

	waiters  int
	gen      int // bumped each time waiters drops to zero
	finished bool
	pinned   bool   // the committed entry holds a pin for the waiters
	blob     bool   // takes a download slot
	quota    *Quota // the starter's quota, held until the flight ends
	cancel   context.CancelFunc
}

type store struct {
	dir                 string
	max, total, pending int64
	now                 func() time.Time
	// grace is how long a flight with no waiter left keeps downloading in
	// case another request joins it; then it is cancelled.
	grace time.Duration
	// maxDownloads caps the concurrent blob downloads (0: no cap).
	maxDownloads, downloads int
	// slotWait is how long a request waits for a download slot before errBusy.
	slotWait time.Duration
	// slotFreed is closed and replaced each time a download slot frees.
	slotFreed chan struct{}
	log       zerowrap.Logger

	mu      sync.Mutex
	entries map[string]*entry
	lru     *list.List                     // *entry, least recently used first
	tagsOf  map[string]map[string]struct{} // manifest digest -> tag keys
	flights map[string]*flight

	hookCommitted func(key string) // tests: after a commit, before waking waiters
	hookBug       func(msg string) // tests: called on an accounting bug
}

// defaultBugHook is each new store's hookBug. Tests set it in TestMain, before
// any store exists, so that an accounting bug fails the run.
var defaultBugHook func(msg string)

// storedFile pins its entry until the descriptor is closed. In particular, an
// unlinked-but-open blob cannot silently escape the disk accounting.
type storedFile struct {
	*os.File
	s    *store
	key  string
	once sync.Once
	err  error
}

func (f *storedFile) Close() error {
	f.once.Do(func() {
		f.err = f.File.Close()
		f.s.mu.Lock()
		defer f.s.mu.Unlock()
		f.s.unpinLocked(f.key)
	})
	return f.err
}

func openStore(dir string, max int64, now func() time.Time) (*store, error) {
	if max <= 0 {
		return nil, errors.New("registry cache cap must be positive")
	}
	s := &store{dir: dir, max: max, now: now, entries: map[string]*entry{}, lru: list.New(), tagsOf: map[string]map[string]struct{}{}, flights: map[string]*flight{}, slotFreed: make(chan struct{}), hookBug: defaultBugHook}
	for _, kind := range []string{"blobs", "manifests", "tags", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, kind), 0700); err != nil {
			return nil, err
		}
	}
	temps, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil {
		return nil, err
	}
	for _, de := range temps {
		if err := os.Remove(filepath.Join(dir, "tmp", de.Name())); err != nil {
			return nil, err
		}
	}
	var types []string
	for _, kind := range []string{"blobs", "manifests", "tags"} {
		ds, err := os.ReadDir(filepath.Join(dir, kind))
		if err != nil {
			return nil, err
		}
		for _, de := range ds {
			if base, ok := strings.CutSuffix(de.Name(), ".type"); ok && kind == "manifests" {
				types = append(types, base)
				continue
			}
			if !validDigest("sha256:" + de.Name()) {
				continue
			}
			fi, err := de.Info()
			if err != nil {
				return nil, err
			}
			if !fi.Mode().IsRegular() {
				continue
			}
			key := kind + "/" + de.Name()
			e := &entry{key: key, size: fi.Size(), used: fi.ModTime()}
			if kind == "manifests" {
				info, err := os.Stat(filepath.Join(dir, key) + ".type")
				if err != nil {
					_ = os.Remove(filepath.Join(dir, key))
					continue
				}
				e.size += info.Size()
			}
			if kind == "tags" {
				digest := s.readTagFile(key)
				if !validDigest(digest) {
					_ = os.Remove(filepath.Join(dir, key))
					continue
				}
				if _, ok := s.entries["manifests/"+digest[7:]]; !ok {
					_ = os.Remove(filepath.Join(dir, key))
					continue
				}
				e.size = tagCharge
				e.digest = digest
			}
			s.addLocked(e)
		}
	}
	// A .type whose manifest is gone (a crash between the two writes).
	for _, base := range types {
		if _, ok := s.entries["manifests/"+base]; !ok {
			_ = os.Remove(filepath.Join(dir, "manifests", base+".type"))
		}
	}
	// Order the LRU by the access times kept in the mtimes.
	all := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].used.Before(all[j].used) })
	s.lru.Init()
	for _, e := range all {
		e.elem = s.lru.PushBack(e)
	}
	s.evictLocked(0)
	return s, nil
}

func (s *store) addLocked(e *entry) {
	s.entries[e.key] = e
	e.elem = s.lru.PushBack(e)
	s.total += e.size
	if e.digest != "" {
		s.indexTagLocked(e.key, e.digest)
	}
}

func (s *store) indexTagLocked(key, digest string) {
	set := s.tagsOf[digest]
	if set == nil {
		set = map[string]struct{}{}
		s.tagsOf[digest] = set
	}
	set[key] = struct{}{}
}

func (s *store) unindexTagLocked(key, digest string) {
	if set := s.tagsOf[digest]; set != nil {
		delete(set, key)
		if len(set) == 0 {
			delete(s.tagsOf, digest)
		}
	}
}

func (s *store) touchLocked(e *entry) {
	e.used = s.now()
	s.lru.MoveToBack(e.elem)
	_ = os.Chtimes(filepath.Join(s.dir, e.key), e.used, e.used)
}

func (s *store) unpinLocked(key string) {
	e, ok := s.entries[key]
	if !ok {
		return
	}
	if e.pins <= 0 {
		// A bug: the entry could have been evicted while open.
		s.log.Error().Str("key", key).Msg("registry cache unpin of an unpinned entry")
		if s.hookBug != nil {
			s.hookBug("unpin of an unpinned entry " + key)
		}
		return
	}
	e.pins--
}

// removeLocked only releases a charge after the corresponding file is gone.
// Failures leave the entry charged so a full or unwritable disk fails closed.
func (s *store) removeLocked(key string) bool {
	e, ok := s.entries[key]
	if !ok {
		return true
	}
	if e.pins > 0 {
		return false
	}
	if hexDigest, ok := strings.CutPrefix(key, "manifests/"); ok {
		// Prune references first. If a tag cannot be removed, retain its target too.
		for k := range s.tagsOf["sha256:"+hexDigest] {
			if !s.removeLocked(k) {
				return false
			}
		}
		if err := os.Remove(filepath.Join(s.dir, key) + ".type"); err != nil && !os.IsNotExist(err) {
			return false
		}
	}
	if err := os.Remove(filepath.Join(s.dir, key)); err != nil && !os.IsNotExist(err) {
		return false
	}
	s.total -= e.size
	delete(s.entries, key)
	s.lru.Remove(e.elem)
	e.elem = nil
	if e.digest != "" {
		s.unindexTagLocked(key, e.digest)
	}
	return true
}

// evictLocked makes room for need bytes. Once over the cap, it walks the LRU
// (no sort) and evicts unpinned entries until max(need, low water) bytes are
// free, so a full store evicts once per low-water bytes, not on every write.
func (s *store) evictLocked(need int64) {
	if s.total+s.pending+need <= s.max {
		return
	}
	target := s.max - max(need, min(lowWater, s.max/20))
	for el := s.lru.Front(); el != nil && s.total+s.pending > target; {
		e := el.Value.(*entry)
		next := el.Next()
		if e.pins == 0 {
			removed := s.removeLocked(e.key)
			if strings.HasPrefix(e.key, "manifests/") {
				// Removing a manifest removes its tags first, even when it then
				// fails: next may be gone. Start over; only pinned or
				// unremovable entries are walked again.
				next = s.lru.Front()
				if !removed {
					// Do not walk into it again: it stays where it is, and
					// the walk resumes after it.
					next = e.elem.Next()
				}
			}
		}
		el = next
	}
}

func (s *store) openLocked(key string) (*storedFile, string, error) {
	e, ok := s.entries[key]
	if !ok {
		return nil, "", os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.dir, key))
	if err != nil {
		return nil, "", err
	}
	media := "application/octet-stream"
	if strings.HasPrefix(key, "manifests/") {
		b, err := os.ReadFile(filepath.Join(s.dir, key) + ".type")
		if err != nil {
			_ = f.Close()
			return nil, "", err
		}
		media = string(b)
	}
	s.touchLocked(e)
	e.pins++
	return &storedFile{File: f, s: s, key: key}, media, nil
}
func (s *store) open(kind, digest string) (*storedFile, string, error) {
	if !validDigest(digest) || (kind != "blobs" && kind != "manifests") {
		return nil, "", errDigest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(kind + "/" + digest[7:])
}

// fetch writes the upstream content to w. ctx belongs to the flight: it ends
// when the last waiter has been gone for the grace period.
type fetch func(ctx context.Context, w io.Writer) (string, error)

type keepalive struct {
	every time.Duration
	fn    func()
}

// ensure returns the stored content, pinned for the caller; hit reports that it
// was already stored. Otherwise the request joins or starts the flight of that
// digest. The flight runs on its own goroutine and context, so no single
// request, the initiator included, can cancel it while another one waits; it is
// cancelled once no request has waited for it for the grace period. Every
// waiter waits for the verified commit and then reads the stored file. While
// waiting, keep.fn is called every keep.every when the download has made
// progress, so that the waiter's connection deadlines can follow it.
//
// Starting a blob flight takes a global download slot and one of the quota
// carried by ctx (WithQuota), both held until the flight really ends; joining
// one takes neither. When a slot is missing, the request waits for one, kept
// alive, for at most the slot wait, and then fails with errBusy.
func (s *store) ensure(ctx context.Context, kind, digest string, fetch fetch, keep keepalive) (f *storedFile, media string, hit bool, err error) {
	if !validDigest(digest) || (kind != "blobs" && kind != "manifests") {
		return nil, "", false, errDigest
	}
	key := kind + "/" + digest[7:]
	blob := kind == "blobs"
	quota := quotaFrom(ctx)
	var slotDeadline <-chan time.Time
	var fl *flight
	for {
		s.mu.Lock()
		if f, media, err := s.openLocked(key); err == nil {
			s.mu.Unlock()
			return f, media, true, nil
		}
		var ok bool
		if fl, ok = s.flights[key]; ok {
			break // join: no slot needed
		}
		if !blob || s.takeSlotLocked(quota) {
			dctx, cancel := context.WithCancel(context.Background())
			fl = &flight{key: key, done: make(chan struct{}), cancel: cancel, blob: blob}
			if blob {
				fl.quota = quota
			}
			s.flights[key] = fl
			go s.run(dctx, fl, digest, fetch)
			break
		}
		freed := s.slotFreed
		s.mu.Unlock()
		if slotDeadline == nil {
			t := time.NewTimer(s.slotWait)
			defer t.Stop()
			slotDeadline = t.C
		}
		if err := waitSlot(ctx, freed, slotDeadline, keep); err != nil {
			return nil, "", false, err
		}
	}
	fl.waiters++
	s.mu.Unlock()

	err = waitFlight(ctx, fl, keep)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		f, media, err = s.openLocked(key)
	}
	s.leaveLocked(fl)
	return f, media, false, err
}

// leaveLocked drops a waiter. The last one releases the commit pin, or, before
// the commit, starts the grace period after which the flight is cancelled.
func (s *store) leaveLocked(fl *flight) {
	fl.waiters--
	if fl.waiters > 0 {
		return
	}
	if fl.finished {
		if fl.pinned {
			fl.pinned = false
			s.unpinLocked(fl.key)
		}
		return
	}
	fl.gen++
	gen := fl.gen
	// A manifest is small and bounded by the request timeout: it is cancelled at
	// once, so its flights never outnumber the requests in progress. A blob
	// holds a download slot until it really ends, which bounds the abandoned
	// ones to MaxDownloads.
	if s.grace <= 0 || !fl.blob {
		s.abandonLocked(fl)
		return
	}
	time.AfterFunc(s.grace, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if fl.waiters == 0 && !fl.finished && fl.gen == gen {
			s.abandonLocked(fl)
		}
	})
}

// abandonLocked cancels a flight nobody waits for. A later request for the
// digest starts a new one.
func (s *store) abandonLocked(fl *flight) {
	fl.cancel()
	if s.flights[fl.key] == fl {
		delete(s.flights, fl.key)
	}
}

// takeSlotLocked takes a global download slot and one of quota (nil: none),
// both or neither.
func (s *store) takeSlotLocked(quota *Quota) bool {
	if s.maxDownloads > 0 && s.downloads >= s.maxDownloads {
		return false
	}
	if quota != nil && !quota.tryAcquire() {
		return false
	}
	s.downloads++
	return true
}

// waitSlot waits until a slot may have freed (freed is closed), keeping the
// client alive meanwhile. It fails with errBusy at the deadline.
func waitSlot(ctx context.Context, freed <-chan struct{}, deadline <-chan time.Time, keep keepalive) error {
	var tick <-chan time.Time
	if keep.every > 0 && keep.fn != nil {
		t := time.NewTicker(keep.every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-freed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errBusy
		case <-tick:
			keep.fn() // bounded by the slot wait
		}
	}
}

// finishLocked ends a flight and releases its slots. A committed entry is
// pinned on behalf of the waiters, so that no eviction removes it before they
// open it.
func (s *store) finishLocked(fl *flight, err error, committed *entry) {
	fl.err = err
	fl.finished = true
	if err == nil && committed != nil && fl.waiters > 0 {
		committed.pins++
		fl.pinned = true
	}
	if s.flights[fl.key] == fl {
		delete(s.flights, fl.key)
	}
	if fl.blob {
		s.downloads--
		if fl.quota != nil {
			fl.quota.release()
		}
		close(s.slotFreed)
		s.slotFreed = make(chan struct{})
	}
}

// run downloads and finishes the flight. A panic in fetch fails the flight
// (and frees its slots) rather than leave its waiters hanging.
func (s *store) run(ctx context.Context, fl *flight, digest string, fetch fetch) {
	var err error
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("registry download panicked: %v", v)
			s.log.Error().Str("key", fl.key).Err(err).Msg("registry download")
		}
		fl.cancel()
		s.mu.Lock()
		if !fl.finished {
			if err == nil {
				err = errors.New("registry download ended without a commit")
			}
			s.finishLocked(fl, err, nil)
		}
		s.mu.Unlock()
		if fl.err == nil && s.hookCommitted != nil {
			s.hookCommitted(fl.key)
		}
		close(fl.done)
	}()
	err = s.fill(ctx, fl, digest, fetch)
}

func waitFlight(ctx context.Context, fl *flight, keep keepalive) error {
	var tick <-chan time.Time
	if keep.every > 0 && keep.fn != nil {
		t := time.NewTicker(keep.every)
		defer t.Stop()
		tick = t.C
	}
	last := fl.written.Load()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fl.done:
			return fl.err
		case <-tick:
			if n := fl.written.Load(); n != last {
				last = n
				keep.fn()
			}
		}
	}
}

type chargedWriter struct {
	s       *store
	w       io.Writer
	n       int64
	written *atomic.Int64
}

func (w *chargedWriter) Write(b []byte) (int, error) {
	s := w.s
	s.mu.Lock()
	need := int64(len(b))
	s.evictLocked(need)
	if need > s.max-s.total-s.pending {
		s.mu.Unlock()
		return 0, errTooLarge
	}
	s.pending += need
	w.n += need
	s.mu.Unlock()
	n, err := w.w.Write(b)
	if n < len(b) {
		s.mu.Lock()
		s.pending -= int64(len(b) - n)
		w.n -= int64(len(b) - n)
		s.mu.Unlock()
	}
	w.written.Add(int64(n))
	return n, err
}

// fill downloads into tmp/, verifies the digest and commits. On success it has
// finished the flight (finishLocked) under the same lock as the commit.
func (s *store) fill(ctx context.Context, fl *flight, digest string, fetch fetch) error {
	key := fl.key
	f, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "download-")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	h := sha256.New()
	cw := &chargedWriter{s: s, w: io.MultiWriter(f, h), written: &fl.written}
	defer func() { s.mu.Lock(); s.pending -= cw.n; s.mu.Unlock() }()
	media, err := fetch(ctx, cw)
	if err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return errDigest
	}
	if err = f.Close(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		// Same digest, same bytes: keep the stored file and its pins rather than
		// renaming over a file that may be open (its bytes would leave the
		// accounting while still allocated).
		s.finishLocked(fl, nil, e)
		return nil
	}
	path := filepath.Join(s.dir, key)
	metadata := int64(0)
	if strings.HasPrefix(key, "manifests/") {
		if len(media) > 1024 {
			return errors.New("manifest media type too large")
		}
		metadata = int64(len(media))
		s.evictLocked(metadata)
		if metadata > s.max-s.total-s.pending {
			return errTooLarge
		}
		if err := os.WriteFile(path+".type", []byte(media), 0600); err != nil {
			return err
		}
	}
	if err := os.Rename(f.Name(), path); err != nil {
		if metadata > 0 {
			_ = os.Remove(path + ".type")
		}
		return err
	}
	e := &entry{key: key, size: cw.n + metadata, used: s.now()}
	s.addLocked(e)
	s.pending -= cw.n
	cw.n = 0
	s.finishLocked(fl, nil, e)
	return nil
}
func tagKey(name, tag, accept string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(name+"\x00"+tag+"\x00"+normalizeAccept(accept))))
}
func (s *store) readTagFile(key string) string {
	f, err := os.Open(filepath.Join(s.dir, key))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 72))
	if err != nil || !validDigest(string(b)) {
		return ""
	}
	return string(b)
}
func (s *store) tag(name, tag, accept string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries["tags/"+tagKey(name, tag, accept)]
	if !ok {
		return ""
	}
	s.touchLocked(e)
	return e.digest
}
func (s *store) setTag(name, tag, accept, digest string) error {
	if !validDigest(digest) {
		return errDigest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest, ok := s.entries["manifests/"+digest[7:]]
	if !ok {
		return os.ErrNotExist
	}
	// Protect the target while making room for its metadata.
	manifest.pins++
	defer func() { manifest.pins-- }()
	key := "tags/" + tagKey(name, tag, accept)
	old, exists := s.entries[key]
	if exists {
		// Already charged: rewriting it needs no room.
		s.touchLocked(old)
	} else {
		s.evictLocked(tagCharge)
		if tagCharge > s.max-s.total-s.pending {
			return errTooLarge
		}
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "tag-")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.WriteString(digest); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, key)); err != nil {
		return err
	}
	if old, ok := s.entries[key]; ok {
		s.unindexTagLocked(key, old.digest)
		old.digest = digest
		old.used = s.now()
		s.indexTagLocked(key, digest)
		return nil
	}
	s.addLocked(&entry{key: key, size: tagCharge, used: s.now(), digest: digest})
	return nil
}
