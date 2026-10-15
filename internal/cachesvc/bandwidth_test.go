// SPDX-License-Identifier: MIT

package cachesvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

const (
	bwRate = 200_000 // bytes per second of a limited channel
	bwSize = 100_000 // bytes per transfer: 0.5 s at bwRate
)

// bwLocation looks up key k for the job's token on channel c and returns the
// archive location.
func bwLocation(t *testing.T, c *http.Client, token string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://cache"+APIPath+"cache?keys=k&version="+v1, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var e struct{ ArchiveLocation string }
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || resp.StatusCode != 200 {
		t.Fatalf("lookup: %d %v", resp.StatusCode, err)
	}
	return e.ArchiveLocation
}

// hangGuard is the generous upper bound of a transfer: it only catches a hang,
// never a slow machine.
const hangGuard = 8 * time.Second

// limiterOf returns the limiter installed on the channel id (nil: unlimited).
func limiterOf(t *testing.T, svc *Service, id string) *bandwidth {
	t.Helper()
	svc.mu.Lock()
	defer svc.mu.Unlock()
	l := svc.channels[id]
	if l == nil {
		t.Fatalf("no channel %s", id)
	}
	return l.bw
}

// bucketUsed reports whether any byte was ever taken from b.
func bucketUsed(b *bandwidth) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.free.IsZero()
}

// bwDownload fetches the archive and returns how long the whole transfer took.
func bwDownload(t *testing.T, c *http.Client, loc string) time.Duration {
	t.Helper()
	start := time.Now()
	code, body := archiveStatus(t, c, "GET", loc)
	if code != 200 || len(body) != bwSize {
		t.Errorf("download: %d, %d bytes", code, len(body))
	}
	return time.Since(start)
}

// bwRig has one stored archive of bwSize bytes (job-a, key k).
func bwRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t, 1<<22)
	j := r.job("job-a", repoA, main_, main_)
	// One chunk (save would send 4-byte ones).
	code, id := j.reserve("k", v1, bwSize)
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d", code)
	}
	if code := j.chunk(id, bytes.Repeat([]byte("x"), bwSize), 0); code != http.StatusNoContent {
		t.Fatalf("chunk: %d", code)
	}
	if code := j.commit(id, bwSize); code != http.StatusNoContent {
		t.Fatalf("commit: %d", code)
	}
	return r, j.token
}

// A channel with a rate serves a download at about that rate: neither much
// faster (the limit holds) nor stalled (the transfer keeps going to the end).
func TestBandwidthLimitsADownload(t *testing.T) {
	r, tok := bwRig(t)
	c := channel(t, r.svc, "er0000000a", "job-a", withRate(bwRate))
	loc := bwLocation(t, c, tok)
	got := bwDownload(t, c, loc)
	want := time.Second * bwSize / bwRate
	if got < want*8/10 || got > want+hangGuard {
		t.Fatalf("download took %v at %d B/s, want at least %v", got, bwRate, want*8/10)
	}
}

// The request bodies are limited too.
func TestBandwidthLimitsAnUpload(t *testing.T) {
	r := newRig(t, 1<<22)
	c := channel(t, r.svc, "er0000000a", "job-a", withRate(bwRate))
	j := r.job("job-a", repoA, main_, main_)
	code, id := j.reserve("k", v1, bwSize)
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d", code)
	}
	req, _ := http.NewRequest("PATCH", "http://cache"+APIPath+fmt.Sprintf("caches/%d", id), bytes.NewReader(bytes.Repeat([]byte("x"), bwSize)))
	req.Header.Set("Authorization", "Bearer "+j.token)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", bwSize-1))
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := time.Since(start)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	want := time.Second * bwSize / bwRate
	if got < want*8/10 || got > want+hangGuard {
		t.Fatalf("upload took %v at %d B/s, want at least %v", got, bwRate, want*8/10)
	}
	if code := j.commit(id, bwSize); code != http.StatusNoContent {
		t.Fatalf("commit: %d", code)
	}
}

// The limit is the sandbox's, not the connection's: two simultaneous downloads
// on one channel take as long as the two in a row at that rate would, and a
// channel of another sandbox, with no limit, is not slowed by them.
func TestBandwidthIsSharedByTheConnectionsOfASandboxOnly(t *testing.T) {
	r, tok := bwRig(t)
	a := channel(t, r.svc, "er0000000a", "job-a", withRate(bwRate))
	b := channel(t, r.svc, "er0000000b", "job-a") // same job id, so the same URL; no limit
	loc := bwLocation(t, b, tok)

	if limiterOf(t, r.svc, "er0000000a") == nil || limiterOf(t, r.svc, "er0000000b") != nil {
		t.Fatal("A must be limited and B must have no limiter")
	}

	start := time.Now()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); bwDownload(t, a, loc) }()
	}
	bwDownload(t, b, loc) // B has no limiter, so it never waits on A's bucket
	wg.Wait()
	total := time.Since(start)

	want := time.Second * 2 * bwSize / bwRate // 1 s for the two; 0.5 s if each had the rate
	if total < want*8/10 || total > want+hangGuard {
		t.Fatalf("two downloads took %v in all, want at least %v", total, want*8/10)
	}
}

// Two channels with a rate each do not draw on one bucket: the limit is per
// Serve, so two sandboxes at once take the time of one.
func TestBandwidthIsPerChannel(t *testing.T) {
	r, tok := bwRig(t)
	a := channel(t, r.svc, "er0000000a", "job-a", withRate(bwRate))
	b := channel(t, r.svc, "er0000000b", "job-a", withRate(bwRate))
	loc := bwLocation(t, a, tok)
	la, lb := limiterOf(t, r.svc, "er0000000a"), limiterOf(t, r.svc, "er0000000b")
	if la == nil || lb == nil || la == lb {
		t.Fatal("each channel must have its own limiter")
	}
	// Consuming from A leaves B's bucket untouched.
	bwDownload(t, a, loc)
	if !bucketUsed(la) || bucketUsed(lb) {
		t.Fatalf("after a download on A only: A used=%v, B used=%v", bucketUsed(la), bucketUsed(lb))
	}
	bwDownload(t, b, loc)
	if !bucketUsed(lb) {
		t.Fatal("B's own download did not draw on B's bucket")
	}
}

// Without a rate nothing is limited or wrapped, and a number of Mbit/s is
// bytes per second the way the VM interface counts it.
func TestBandwidthUnsetIsUnlimited(t *testing.T) {
	if newBandwidth(mbitToBytes(0)) != nil || newBandwidth(mbitToBytes(-1)) != nil {
		t.Fatal("0 must mean unlimited")
	}
	if got := mbitToBytes(1000); got != 125_000_000 {
		t.Fatalf("1000 Mbit/s = %d B/s", got)
	}
	r, tok := bwRig(t)
	c := channel(t, r.svc, "er0000000a", "job-a", WithBandwidth(0))
	loc := bwLocation(t, c, tok)
	if limiterOf(t, r.svc, "er0000000a") != nil {
		t.Fatal("WithBandwidth(0) installed a limiter")
	}
	if got := bwDownload(t, c, loc); got > hangGuard {
		t.Fatalf("unlimited download took %v", got)
	}
}

// A throttled transfer is cut when its connection is, not left sleeping.
func TestBandwidthSleepEndsWithTheConnection(t *testing.T) {
	r, tok := bwRig(t)
	c := channel(t, r.svc, "er0000000a", "job-a", withRate(5_000)) // 20 s for the archive
	loc := bwLocation(t, c, tok)
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://cache"+u.RequestURI(), nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1000)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("the throttled transfer does not make progress: %v", err)
	}
	_ = resp.Body.Close()
	done := make(chan struct{})
	go func() { r.svc.Stop("er0000000a"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waits for a throttled transfer to finish")
	}
}
