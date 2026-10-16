// SPDX-License-Identifier: MIT

package cachesvc

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
)

// chunkReq sends a PATCH whose body the test feeds through the returned writer;
// the status arrives on the channel. The body is streamed, so the server has
// the Content-Range and a body that stays open until the test closes it.
func (j *job) chunkReq(id int64, lo, hi int) (w *io.PipeWriter, done <-chan int) {
	pr, pw := io.Pipe()
	res := make(chan int, 1)
	req, _ := http.NewRequest("PATCH", j.r.srv.URL+APIPath+fmt.Sprintf("caches/%d", id), pr)
	req.Header.Set("Authorization", "Bearer "+j.token)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", lo, hi-1))
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			res <- -1
			return
		}
		_ = resp.Body.Close()
		res <- resp.StatusCode
	}()
	return pw, res
}

// wait receives from c; the timeout only bounds a failing test.
func wait[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		panic("unreachable")
	}
}

func dataFiles(t *testing.T, r *rig) int {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(r.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return len(des)
}

// A6: a retry of a range that fails halfway must not leave the earlier bytes
// counted as valid.
func TestFailedRetryRemovesCoverage(t *testing.T) {
	r := newRig(t, 1<<20)
	j := r.job("j1", repoA, main_, main_)
	_, id := j.reserve("k", v1, 4)
	if j.chunk(id, []byte("abcd"), 0) != 204 {
		t.Fatal("first chunk")
	}
	// The same range again, with a body one byte short of its Content-Range.
	code, _, _ := j.do("PATCH", fmt.Sprintf("caches/%d", id), []byte("x"), map[string]string{"Content-Range": "bytes 0-3/*"})
	if code != 400 {
		t.Fatalf("short retry: %d", code)
	}
	if c := j.commit(id, 4); c != 400 {
		t.Fatalf("commit after a failed retry must fail, got %d", c)
	}
	if j.chunk(id, []byte("wxyz"), 0) != 204 || j.commit(id, 4) != 204 {
		t.Fatal("a good retry works")
	}
	if _, data, _ := j.restore(v1, "k"); string(data) != "wxyz" {
		t.Fatalf("content %q", data)
	}
}

// B4: with a body held open by the test (no sleeps) commit is refused, an
// overlapping range is refused, a disjoint one is accepted, an expiry pass does
// not drop the upload, and the upload then completes and restores.
func TestConcurrentChunks(t *testing.T) {
	admitted := make(chan int64, 4)
	r := newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, ReservationMaxLife: 24 * time.Hour, hookChunk: func(id int64) { admitted <- id }})
	j := r.job("j1", repoA, main_, main_)
	_, id := j.reserve("k", v1, 8)

	pw, done := j.chunkReq(id, 0, 4)
	wait(t, admitted, "the first chunk to be admitted")

	if c := j.commit(id, 8); c != 400 {
		t.Errorf("commit with a chunk in flight: %d", c)
	}
	if c := j.chunk(id, []byte("cdef"), 2); c != http.StatusConflict {
		t.Errorf("overlapping range: %d", c)
	}
	if c := j.chunk(id, []byte("efgh"), 4); c != 204 {
		t.Errorf("disjoint range: %d", c)
	}
	wait(t, admitted, "the second chunk to be admitted")

	// Far past the idle ttl (but inside the absolute life): the next reserve runs
	// the expiry, which must keep an upload with a chunk in flight.
	r.clk.add(2 * time.Hour)
	if code, _ := j.reserve("other", v1, 4); code != http.StatusCreated {
		t.Fatalf("reserve: %d", code)
	}
	if n := r.svc.store.pendingCount(); n != 2 {
		t.Fatalf("pending reservations %d, want the upload and the new one", n)
	}

	if _, err := pw.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if code := wait(t, done, "the first chunk"); code != 204 {
		t.Fatalf("first chunk: %d", code)
	}
	if c := j.commit(id, 8); c != 204 {
		t.Fatalf("commit: %d", c)
	}
	if _, data, _ := j.restore(v1, "k"); string(data) != "abcdefgh" {
		t.Fatalf("content %q", data)
	}
}

// A body that stalls loses its range: the progress deadline cuts it, the range
// is free again and is not counted as received.
func TestStalledUploadIsCut(t *testing.T) {
	r := newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, ProgressTimeout: 100 * time.Millisecond})
	j := r.job("j1", repoA, main_, main_)
	_, id := j.reserve("k", v1, 8)

	conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Eight bytes announced, four sent, then silence.
	_, err = fmt.Fprintf(conn, "PATCH %scaches/%d HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Range: bytes 0-7/*\r\nContent-Length: 8\r\n\r\nabcd", APIPath, id, j.token)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("stalled upload: %d", resp.StatusCode)
	}
	if c := j.commit(id, 8); c != 400 {
		t.Fatalf("commit: %d", c)
	}
	// Not in flight any more: the same range can be sent again.
	if j.chunk(id, []byte("abcdefgh"), 0) != 204 || j.commit(id, 8) != 204 {
		t.Fatal("the range is free again")
	}
}

// The write deadline of a download moves with every write.
type deadlineWriter struct {
	httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestProgressWriterExtendsDeadline(t *testing.T) {
	d := &deadlineWriter{ResponseRecorder: *httptest.NewRecorder()}
	pw := &progressWriter{ResponseWriter: d, rc: http.NewResponseController(d), idle: time.Minute}
	for i := 0; i < 3; i++ {
		if _, err := pw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.deadlines) != 3 || d.deadlines[0].IsZero() {
		t.Fatalf("deadlines %v", d.deadlines)
	}
}

// GO-010: slots are taken by accepted connections only. Idle listeners hold
// none, so many sandboxes cannot starve one another; a connection over a cap is
// closed at once, and a slot comes back when a connection closes.
func TestLimitListener(t *testing.T) {
	const listeners, global = 70, 64
	slots := make(chan struct{}, global)
	var ls []*limitListener
	for i := 0; i < listeners; i++ {
		ln := listenLoopbackT(t)
		l := newLimitListener(ln, 2, slots)
		t.Cleanup(func() { _ = l.Close() })
		ls = append(ls, l)
	}
	if len(slots) != 0 {
		t.Fatalf("idle listeners hold %d global slots", len(slots))
	}
	dial := func(l *limitListener) net.Conn {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// Every accepted connection is closed at cleanup, whatever happens (Close is
	// idempotent and releases its slots once).
	accepted := func(c net.Conn) net.Conn {
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// Each of the first 64 listeners still accepts its connection.
	var held []net.Conn
	for _, l := range ls[:global] {
		dial(l)
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, accepted(c))
	}
	if len(slots) != global {
		t.Fatalf("global slots %d", len(slots))
	}
	// The 65th is over the global cap: closed at once. Its Accept goes on to the
	// next connection, so a later one proves the loop is alive.
	over := dial(ls[global])
	next := make(chan net.Conn, 1)
	go func() {
		if c, err := ls[global].Accept(); err == nil {
			next <- accepted(c)
		}
	}()
	_ = over.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("over-cap connection not closed by the server: %v", err)
	}
	// Free one slot: the next connection is served.
	_ = held[0].Close()
	_ = held[0].Close() // idempotent: no slot is released twice
	if len(slots) != global-1 {
		t.Fatalf("after one close: %d slots", len(slots))
	}
	dial(ls[global])
	c := wait(t, next, "a connection once a slot is free")
	held = append(held[1:], c)

	// The local cap: two on one listener, the third is closed.
	for _, c := range held[:10] {
		_ = c.Close()
	}
	l := ls[0]
	var mine []net.Conn
	for i := 0; i < 2; i++ {
		dial(l)
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, accepted(c))
	}
	third := dial(l)
	go func() {
		if c, err := l.Accept(); err == nil {
			_ = accepted(c).Close()
		}
	}()
	_ = third.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := third.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("connection over the listener's share not closed: %v", err)
	}
	for _, c := range append(held[10:], mine...) {
		_ = c.Close()
	}
	if len(slots) != 0 || len(l.local) != 0 {
		t.Fatalf("slots left: global %d local %d", len(slots), len(l.local))
	}
}

func listenLoopbackT(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// A2: a reservation without cacheSize is charged for the bytes it writes, before
// they are accepted, and keeps the charge until commit.
func TestSizelessUploadIsCharged(t *testing.T) {
	r := newRig(t, 100)
	a, b := r.job("ja", repoA, main_, main_), r.job("jb", repoA, feat, feat)
	_, ida := a.reserve("a", v1, 0)
	_, idb := b.reserve("b", v1, 0)
	if c := a.chunk(ida, make([]byte, 60), 0); c != 204 {
		t.Fatalf("first: %d", c)
	}
	if _, _, held := r.svc.store.stats(); held != 61 { // 60 + the 1 of b
		t.Fatalf("held %d", held)
	}
	if c := b.chunk(idb, make([]byte, 50), 0); c != http.StatusServiceUnavailable {
		t.Fatalf("over the budget: %d", c)
	}
	// Refused before the bytes were taken: the file is empty, nothing is covered.
	if st, _ := os.Stat(r.svc.store.tmp(idb)); st.Size() != 0 {
		t.Fatalf("refused chunk wrote %d bytes", st.Size())
	}
	if c := b.chunk(idb, make([]byte, 39), 0); c != 204 {
		t.Fatalf("what fits: %d", c)
	}
	// Retrying a smaller or equal range later costs nothing more.
	if c := b.chunk(idb, make([]byte, 39), 0); c != 204 {
		t.Fatalf("retry: %d", c)
	}
	if _, _, held := r.svc.store.stats(); held != 99 {
		t.Fatalf("held %d", held)
	}
	if c := a.commit(ida, 60); c != 204 {
		t.Fatalf("commit: %d", c)
	}
	if n, used, held := r.svc.store.stats(); n != 1 || used != 60 || held != 39 {
		t.Fatalf("after commit: entries %d used %d held %d", n, used, held)
	}
	// Above the whole limit.
	_, idc := a.reserve("c", v1, 0)
	if c := a.chunk(idc, make([]byte, 101), 0); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit: %d", c)
	}
}

// A size-less upload makes room by evicting the least recently used entries.
func TestSizelessUploadEvicts(t *testing.T) {
	r := newRig(t, 100)
	j := r.job("j", repoA, main_, main_)
	for _, k := range []string{"old", "new"} {
		if c := j.save(k, v1, bytes.Repeat([]byte(k[:1]), 40)); c != 204 {
			t.Fatalf("%s: %d", k, c)
		}
		r.clk.add(time.Minute)
	}
	_, id := j.reserve("big", v1, 0)
	if c := j.chunk(id, make([]byte, 50), 0); c != 204 {
		t.Fatalf("chunk: %d", c)
	}
	if code, _ := j.lookup(v1, "old"); code != 204 {
		t.Fatalf("old must be evicted: %d", code)
	}
	if code, _ := j.lookup(v1, "new"); code != 200 {
		t.Fatalf("new must stay: %d", code)
	}
}

// A3/B1: uploads in progress are bounded in all and per job, before any file is
// created; idle ones are reclaimed with the clock.
func TestPendingReservationLimits(t *testing.T) {
	r := newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, MaxPending: 3, MaxPendingPerJob: 2})
	j1, j2, j3 := r.job("j1", repoA, main_, main_), r.job("j2", repoA, main_, main_), r.job("j3", repoA, main_, main_)
	for _, k := range []string{"a", "b"} {
		if code, _ := j1.reserve(k, v1, 1); code != http.StatusCreated {
			t.Fatalf("%s: %d", k, code)
		}
	}
	if code, _ := j1.reserve("c", v1, 1); code != http.StatusServiceUnavailable {
		t.Fatalf("per job: %d", code)
	}
	if code, _ := j2.reserve("d", v1, 1); code != http.StatusCreated {
		t.Fatalf("other job: %d", code)
	}
	if code, _ := j3.reserve("e", v1, 1); code != http.StatusServiceUnavailable {
		t.Fatalf("global: %d", code)
	}
	if n := dataFiles(t, r); n != 3 {
		t.Fatalf("%d files for 3 reservations: refused ones must create none", n)
	}
	// Idle reservations are reclaimed once the ttl passes.
	r.clk.add(2 * time.Hour)
	if code, _ := j3.reserve("e", v1, 1); code != http.StatusCreated {
		t.Fatalf("after expiry: %d", code)
	}
	if n, f := r.svc.store.pendingCount(), dataFiles(t, r); n != 1 || f != 1 {
		t.Fatalf("pending %d files %d", n, f)
	}
}

// A3: every entry and reservation costs at least the minimum charge, so tiny
// entries cannot pile up without bound.
func TestMinimumCharge(t *testing.T) {
	r := newRigWith(t, Config{Max: 10, MinCharge: 4})
	j := r.job("j", repoA, main_, main_)
	for _, k := range []string{"a", "b", "c"} {
		if c := j.save(k, v1, []byte("x")); c != 204 {
			t.Fatalf("%s: %d", k, c)
		}
		r.clk.add(time.Minute)
	}
	if n, used, _ := r.svc.store.stats(); n != 2 || used != 8 {
		t.Fatalf("entries %d used %d: 10 bytes hold two entries of 4", n, used)
	}
	// Reservations are charged too, and nothing is evictable under them.
	r2 := newRigWith(t, Config{Max: 8, MinCharge: 4})
	j2 := r2.job("j", repoA, main_, main_)
	for _, k := range []string{"a", "b"} {
		if code, _ := j2.reserve(k, v1, 1); code != http.StatusCreated {
			t.Fatalf("%s: %d", k, code)
		}
	}
	if code, _ := j2.reserve("c", v1, 1); code != http.StatusServiceUnavailable {
		t.Fatalf("third reservation: %d", code)
	}
}

// A4: an entry being downloaded is not evicted; with nothing else to reclaim a
// reservation fails, and succeeds when the reader is done.
func TestPinnedEntryIsNotEvicted(t *testing.T) {
	r := newRig(t, 30)
	j := r.job("j", repoA, main_, main_)
	for _, k := range []string{"a", "b", "c"} {
		if c := j.save(k, v1, bytes.Repeat([]byte(k), 10)); c != 204 {
			t.Fatalf("%s: %d", k, c)
		}
		r.clk.add(time.Minute)
	}
	e, _ := r.svc.store.find(repoA, []string{main_}, []string{"a"}, v1) // "a" is now the most recent
	r.clk.add(time.Minute)
	f, _, release, err := r.svc.store.open(e.ID, repoA)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever happens: the file first, then the pin (both are idempotent).
	t.Cleanup(func() { _ = f.Close(); release() })
	r.clk.add(time.Minute)
	// "b" is the least recently used unpinned entry; "a" survives although it is
	// the one a plain LRU would pick after more use.
	for _, k := range []string{"b", "c"} {
		r.clk.add(time.Minute)
		kf, _, rel, err := r.svc.store.open(idOf(t, r, k), repoA)
		if err != nil {
			t.Fatal(err)
		}
		_ = kf.Close() // files before pins
		rel()
	}
	// Everything but "a" is more recent than "a" now: LRU order puts "a" first.
	if c := j.save("d", v1, bytes.Repeat([]byte("d"), 10)); c != 204 {
		t.Fatalf("d: %d", c)
	}
	if _, err := os.Stat(r.svc.store.file(e.ID)); err != nil {
		t.Fatalf("the pinned entry was evicted: %v", err)
	}
	if got, _ := io.ReadAll(f); string(got) != strings.Repeat("a", 10) {
		t.Fatalf("reader got %q", got)
	}
	// Reclaiming everything else is not enough for 25 bytes while "a" is pinned.
	if code, _ := j.reserve("big", v1, 25); code != http.StatusServiceUnavailable {
		t.Fatalf("reserve with nothing reclaimable: %d", code)
	}
	_ = f.Close()
	release()
	release() // idempotent
	if code, _ := j.reserve("big", v1, 25); code != http.StatusCreated {
		t.Fatalf("after the reader is done: %d", code)
	}
	r.svc.store.mu.Lock()
	pins := len(r.svc.store.pins)
	r.svc.store.mu.Unlock()
	if pins != 0 {
		t.Fatalf("%d pins left", pins)
	}
}

func idOf(t *testing.T, r *rig, key string) int64 {
	t.Helper()
	e, ok := r.svc.store.find(repoA, []string{main_}, []string{key}, v1)
	if !ok {
		t.Fatalf("no entry %q", key)
	}
	return e.ID
}

// A downloaded archive is unpinned when the response is done.
func TestDownloadReleasesPin(t *testing.T) {
	r := newRig(t, 100)
	j := r.job("j", repoA, main_, main_)
	if c := j.save("k", v1, []byte("data")); c != 204 {
		t.Fatal(c)
	}
	if _, data, ok := j.restore(v1, "k"); !ok || string(data) != "data" {
		t.Fatalf("%v %q", ok, data)
	}
	r.svc.store.mu.Lock()
	pins := len(r.svc.store.pins)
	r.svc.store.mu.Unlock()
	if pins != 0 {
		t.Fatalf("%d pins after the download", pins)
	}
}

// B2: a reserve writes the index only when it evicted something; usage times
// wait for the housekeeping.
func TestReserveWritesIndexOnlyOnEviction(t *testing.T) {
	r := newRig(t, 10)
	j := r.job("j", repoA, main_, main_)
	if c := j.save("k", v1, bytes.Repeat([]byte("x"), 8)); c != 204 {
		t.Fatal(c)
	}
	idx := filepath.Join(r.dir, indexName)
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	if code, _ := j.lookup(v1, "k"); code != 200 { // dirties the usage time
		t.Fatal(code)
	}
	if code, _ := j.reserve("small", v1, 2); code != http.StatusCreated { // fits: no eviction
		t.Fatal(code)
	}
	if _, err := os.Stat(idx); !os.IsNotExist(err) {
		t.Fatalf("the index was written by a reserve that evicted nothing: %v", err)
	}
	if code, _ := j.reserve("big", v1, 8); code != http.StatusCreated { // evicts "k"
		t.Fatal(code)
	}
	if _, err := os.Stat(idx); err != nil {
		t.Fatalf("an eviction must be persisted: %v", err)
	}
	// And the housekeeping flush still writes timestamps.
	if err := r.svc.store.flush(); err != nil {
		t.Fatal(err)
	}
}

// A8: the total of a Content-Range is "*" or a positive size that covers the
// range.
func TestParseRange(t *testing.T) {
	for h, want := range map[string]bool{
		"bytes 0-3/*": true, "bytes 0-3/4": true, "bytes 0-3/100": true, "bytes 5-5/*": true,
		"bytes 0-3/": false, "bytes 0-3/0": false, "bytes 0-3/-1": false, "bytes 0-3/3": false, "bytes 0-3/x": false,
		"bytes 0-3": false, "bytes 3-0/*": false, "bytes -1-3/*": false, "bytes a-b/*": false, "items 0-1/*": false, "": false,
		"bytes 0-4611686018427387904/*": false,
	} {
		if _, _, ok := parseRange(h); ok != want {
			t.Errorf("parseRange(%q) = %v", h, ok)
		}
	}
	if lo, hi, _ := parseRange("bytes 4-7/*"); lo != 4 || hi != 8 {
		t.Errorf("bounds %d %d", lo, hi)
	}
}

// syncBuf is a log sink that handler goroutines and the test may use together.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// GO-012: an eviction marks the index dirty before it is saved; a failed save
// leaves it dirty, logs, and the housekeeping flush writes it later.
func TestFailedIndexSaveStaysDirtyAndIsLogged(t *testing.T) {
	var logs syncBuf
	r := newRigWith(t, Config{Max: 10, MinCharge: 1, Log: zerowrap.New(zerowrap.Config{Output: &logs, Format: "json", Level: "debug"})})
	j := r.job("j", repoA, main_, main_)
	if c := j.save("k", v1, bytes.Repeat([]byte("x"), 8)); c != 204 {
		t.Fatal(c)
	}
	// A directory where the temporary index goes: every save fails.
	blocker := filepath.Join(r.dir, indexName+".tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _ := j.reserve("big", v1, 8); code != http.StatusCreated { // evicts "k"
		t.Fatalf("reserve: %d", code)
	}
	r.svc.store.mu.Lock()
	dirty := r.svc.store.dirty
	r.svc.store.mu.Unlock()
	if !dirty {
		t.Fatal("a failed save must leave the index dirty")
	}
	if !strings.Contains(logs.String(), "cache index") {
		t.Fatalf("the failure was not logged: %q", logs.String())
	}
	if err := r.svc.store.flush(); err == nil {
		t.Fatal("flush must report the failure")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.store.flush(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	r.svc.store.mu.Lock()
	dirty = r.svc.store.dirty
	r.svc.store.mu.Unlock()
	if dirty {
		t.Fatal("dirty after a successful flush")
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, indexName))
	if strings.Contains(string(b), `"key":"k"`) || !strings.HasPrefix(string(b), "[") {
		t.Fatalf("the evicted entry is still in the index: %s", b)
	}
}

// GO-011: a reservation also ends at an absolute time (the token's expiry, or an
// hour after it was made), however recently it was touched.
func TestReservationAbsoluteExpiry(t *testing.T) {
	r := newRig(t, 1<<20)
	// Touched every 31 minutes, which keeps it from idling out, but it is over
	// one hour after its creation.
	j := r.job("j", repoA, main_, main_)
	_, id := j.reserve("k", v1, 8)
	r.clk.add(31 * time.Minute)
	if c := j.chunk(id, []byte("abcd"), 0); c != 204 {
		t.Fatalf("chunk: %d", c)
	}
	r.clk.add(31 * time.Minute)
	if c := j.chunk(id, []byte("efgh"), 4); c != http.StatusNotFound {
		t.Fatalf("chunk past the absolute end: %d", c)
	}
	if code, _ := j.reserve("other", v1, 1); code != http.StatusCreated {
		t.Fatal(code)
	}
	if n, f := r.svc.store.pendingCount(), dataFiles(t, r); n != 1 || f != 1 {
		t.Fatalf("pending %d files %d: the old reservation must be reclaimed", n, f)
	}

	// A token that ends sooner ends the reservation sooner.
	short := &job{r: r, token: r.svc.Token("js", Scope{Repository: repoA, WriteRef: main_, ReadRefs: []string{main_}}, 10*time.Minute)}
	_, ids := short.reserve("s", v1, 4)
	r.svc.store.mu.Lock()
	end := r.svc.store.pending[ids].expires
	r.svc.store.mu.Unlock()
	if want := r.clk.now().Add(10 * time.Minute); end.Sub(want) > time.Second || want.Sub(end) > time.Second {
		t.Fatalf("expires %v, want about %v", end, want)
	}
}

// GO-011: a reservation with a chunk in flight when it runs out is not dropped
// under the writer; it is reclaimed when the chunk ends, and cannot be committed.
func TestExpiredReservationIsReclaimedAfterActiveWrite(t *testing.T) {
	admitted := make(chan int64, 2)
	r := newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, hookChunk: func(id int64) { admitted <- id }})
	j := r.job("j", repoA, main_, main_)
	_, id := j.reserve("k", v1, 8)
	pw, done := j.chunkReq(id, 0, 8)
	wait(t, admitted, "the chunk to be admitted")

	r.clk.add(2 * time.Hour)
	if code, _ := j.reserve("other", v1, 1); code != http.StatusCreated { // runs the expiry
		t.Fatal(code)
	}
	if n := r.svc.store.pendingCount(); n != 2 {
		t.Fatalf("pending %d: the reservation with a write in flight must stay", n)
	}
	if _, err := pw.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	wait(t, done, "the chunk to end")
	if n, f := r.svc.store.pendingCount(), dataFiles(t, r); n != 1 || f != 1 {
		t.Fatalf("pending %d files %d: reclaimed once the write is over", n, f)
	}
	if c := j.commit(id, 8); c != http.StatusNotFound {
		t.Fatalf("commit of a reclaimed reservation: %d", c)
	}
	if _, _, held := r.svc.store.stats(); held != 1 {
		t.Fatalf("held %d", held)
	}
}

// GO-011: the deadline of a transfer never goes past the credential's end.
func TestTransferDeadlineIsCappedAtExpiry(t *testing.T) {
	near := func(d time.Time, want time.Duration) bool {
		got := time.Until(d)
		return got <= want && got > want-5*time.Second
	}
	left := 7 * time.Second
	rem := func() time.Duration { return left }
	if d := deadline(time.Minute, rem); !near(d, 7*time.Second) {
		t.Errorf("capped deadline in %v", time.Until(d))
	}
	if d := deadline(3*time.Second, rem); !near(d, 3*time.Second) {
		t.Errorf("progress deadline in %v", time.Until(d))
	}
	left = -time.Hour
	if d := deadline(time.Minute, rem); time.Until(d) > time.Second {
		t.Errorf("an expired credential gets %v", time.Until(d))
	}
	if d := deadline(time.Minute, nil); !near(d, time.Minute) {
		t.Errorf("no credential: %v", time.Until(d))
	}

	// The writer of a download uses it on every write.
	d := &deadlineWriter{ResponseRecorder: *httptest.NewRecorder()}
	pw := &progressWriter{ResponseWriter: d, rc: http.NewResponseController(d), idle: time.Minute, remaining: func() time.Duration { return 2 * time.Second }}
	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if len(d.deadlines) != 1 || time.Until(d.deadlines[0]) > 2*time.Second {
		t.Fatalf("deadlines %v", d.deadlines)
	}
}

// A token expiry also bounds the transfer through the service: the remaining
// time comes from the service clock.
func TestServiceRemaining(t *testing.T) {
	r := newRig(t, 1<<20)
	exp := r.clk.now().Add(90 * time.Second).Unix()
	rem := r.svc.remaining(exp)
	if got := rem(); got != 90*time.Second {
		t.Fatalf("remaining %v", got)
	}
	r.clk.add(time.Minute)
	if got := rem(); got != 30*time.Second {
		t.Fatalf("remaining %v", got)
	}
}
