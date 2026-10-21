// SPDX-License-Identifier: MIT

package cachesvc

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/actionscache"
)

func archiveRig(t *testing.T) (*rig, *actionscache.Cache, string) {
	t.Helper()
	ac, err := actionscache.New(actionscache.Config{Dir: filepath.Join(t.TempDir(), "a"), Max: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	r := newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, Archives: ac})
	key, _ := actionscache.Key("https://github.com", "o/r", strings.Repeat("a", 40))
	if _, _, err := ac.Ensure(context.Background(), key, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "tarball")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r, ac, key
}

// The guest reads the archive the host fetched on the channel of its job, with
// the signed URL and nothing else; a URL for another job, another key, a stale
// expiry or a forged signature gets nothing.
func TestActionArchiveRoute(t *testing.T) {
	r, ac, key := archiveRig(t)
	a := channel(t, r.svc, "er0000000a", "job-a")
	b := channel(t, r.svc, "er0000000b", "job-b")

	loc := r.svc.ArchiveURL("job-a", key, time.Hour)
	if !strings.HasPrefix(loc, "http://127.0.0.1:9089"+ArchivePath+key+"?") {
		t.Fatalf("url %q", loc)
	}
	if code, body := archiveStatus(t, a, "GET", loc); code != http.StatusOK || body != "tarball" {
		t.Fatalf("own channel: %d %q", code, body)
	}
	if code, _ := archiveStatus(t, b, "GET", loc); code != http.StatusForbidden {
		t.Errorf("another job's channel: %d", code)
	}
	u, _ := url.Parse(loc)
	otherKey, _ := actionscache.Key("https://github.com", "o/private", strings.Repeat("a", 40))
	if code, _ := archiveStatus(t, a, "GET", strings.Replace(loc, key, otherKey, 1)); code != http.StatusForbidden {
		t.Errorf("a signature moved to another key: %d", code)
	}
	q := u.Query()
	q.Set("sig", "AAAA")
	u.RawQuery = q.Encode()
	if code, _ := archiveStatus(t, a, "GET", u.String()); code != http.StatusForbidden {
		t.Errorf("forged signature: %d", code)
	}
	r.clk.t = r.clk.t.Add(2 * time.Hour)
	if code, _ := archiveStatus(t, a, "GET", loc); code != http.StatusForbidden {
		t.Errorf("expired: %d", code)
	}
	r.clk.t = r.clk.t.Add(-2 * time.Hour)

	// A corrupt archive is never served.
	files, _ := filepath.Glob(filepath.Join(ac.Dir(), "*.tgz"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	if err := os.WriteFile(files[0], []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := archiveStatus(t, a, "GET", loc); code != http.StatusNotFound || strings.Contains(body, "tampered") {
		t.Errorf("corrupt: %d %q", code, body)
	}
}

func TestActionArchiveURLNeedsACache(t *testing.T) {
	r := newRig(t, 1<<20)
	key, _ := actionscache.Key("https://github.com", "o/r", strings.Repeat("a", 40))
	if u := r.svc.ArchiveURL("j", key, time.Hour); u != "" {
		t.Errorf("url without a cache: %q", u)
	}
}

// A channel that also serves the results service still sends the action archive
// route to the cache: the results handler takes only the paths of neither.
func TestActionArchiveRouteWithResults(t *testing.T) {
	r, _, key := archiveRig(t)
	results := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "results", http.StatusNotFound)
	})
	a := channel(t, r.svc, "er0000000a", "job-a", WithResults(results))
	if code, body := archiveStatus(t, a, "GET", r.svc.ArchiveURL("job-a", key, time.Hour)); code != http.StatusOK || body != "tarball" {
		t.Fatalf("archive with results: %d %q", code, body)
	}
}
