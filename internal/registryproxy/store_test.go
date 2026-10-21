// SPDX-License-Identifier: MIT
package registryproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sum(b string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(b))) }
func put(t *testing.T, s *store, b string) {
	t.Helper()
	f, _, _, err := s.ensure(context.Background(), "blobs", sum(b), func(_ context.Context, w io.Writer) (string, error) { _, err := io.WriteString(w, b); return "", err }, keepalive{})
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}
func TestStoreIntegrityReopenEviction(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	s, err := openStore(dir, 8, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "aaaa")
	now = now.Add(time.Second)
	put(t, s, "bbbb")
	now = now.Add(time.Second)
	f, _, err := s.open("blobs", sum("aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	now = now.Add(time.Second)
	put(t, s, "cccc")
	if f, _, err := s.open("blobs", sum("bbbb")); err == nil {
		_ = f.Close()
		t.Fatal("LRU survived eviction")
	}
	_, _, _, err = s.ensure(context.Background(), "blobs", sum("dddd"), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, "evil")
		return "", err
	}, keepalive{})
	if err != errDigest {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sum("dddd")[7:])); !os.IsNotExist(err) {
		t.Fatalf("bad commit: %v", err)
	}
	put(t, s, "eeee")
	if err := os.WriteFile(filepath.Join(dir, "tmp", "leftover"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(dir, 8, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if f, _, err := s.open("blobs", sum("eeee")); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
	ds, _ := os.ReadDir(filepath.Join(dir, "tmp"))
	if len(ds) != 0 {
		t.Fatal("tmp not cleared")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total+s.pending > 8 {
		t.Fatal("cap exceeded")
	}
}
func TestStoreConcurrentEnsure(t *testing.T) {
	s, err := openStore(t.TempDir(), 1024, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			f, _, _, err := s.ensure(context.Background(), "blobs", sum("content"), func(_ context.Context, w io.Writer) (string, error) {
				calls.Add(1)
				_, err := io.WriteString(w, "content")
				return "", err
			}, keepalive{})
			if err != nil {
				t.Error(err)
				return
			}
			_ = f.Close()
		})
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetches %d", calls.Load())
	}
}
func TestStoreOversize(t *testing.T) {
	s, err := openStore(t.TempDir(), 3, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = s.ensure(context.Background(), "blobs", sum("large"), func(_ context.Context, w io.Writer) (string, error) {
		_, err := io.WriteString(w, "large")
		return "", err
	}, keepalive{})
	if err != errTooLarge {
		t.Fatal(err)
	}
	if s.pending != 0 || s.total != 0 {
		t.Fatalf("accounting: %d %d", s.pending, s.total)
	}
}
