// SPDX-License-Identifier: MIT

package cachesvc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/registryproxy"
)

const registryProgress = 300 * time.Millisecond

// registryRig serves a fake registry behind the service's real HTTP server, so
// that the connection deadlines apply.
func registryRig(t *testing.T, h http.HandlerFunc) *rig {
	t.Helper()
	return newRigWith(t, Config{Max: 1 << 20, MinCharge: 1, ProgressTimeout: registryProgress, Registry: h})
}

// A registry download that keeps moving may last longer than the progress
// period set when the request started.
func TestRegistryProgressingTransferSucceeds(t *testing.T) {
	const chunks = 14
	r := registryRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(chunks*1024))
		for range chunks {
			if _, err := w.Write([]byte(strings.Repeat("x", 1024))); err != nil {
				return
			}
			_ = http.NewResponseController(w).Flush()
			time.Sleep(registryProgress / 6)
		}
	})
	start := time.Now()
	resp, err := http.Get(r.srv.URL + RegistryPath + "library/alpine/blobs/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || len(b) != chunks*1024 {
		t.Fatalf("read %d bytes: %v", len(b), err)
	}
	if d := time.Since(start); d < 2*registryProgress {
		t.Fatalf("transfer took %v, not longer than the progress period", d)
	}
}

// The proxy keeps a client alive while it waits for a shared upstream
// download, before the first byte; the read deadline moves too, so the request
// context stays alive.
func TestRegistryKeepAliveWhileWaiting(t *testing.T) {
	r := registryRig(t, func(w http.ResponseWriter, req *http.Request) {
		k, ok := w.(registryproxy.KeepAliver)
		if !ok {
			t.Error("the registry writer is no KeepAliver")
			return
		}
		for range 12 { // two progress periods
			time.Sleep(registryProgress / 6)
			k.KeepAlive()
			if req.Context().Err() != nil {
				t.Error("request context ended while kept alive")
				return
			}
		}
		_, _ = io.WriteString(w, "verified")
	})
	resp, err := http.Get(r.srv.URL + RegistryPath + "library/alpine/blobs/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if b, err := io.ReadAll(resp.Body); err != nil || string(b) != "verified" {
		t.Fatalf("%q %v", b, err)
	}
}

// A client that stops reading is cut after one progress period.
func TestRegistryStalledReaderIsCut(t *testing.T) {
	result := make(chan error, 1)
	r := registryRig(t, func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := w.Write(chunk); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	if _, err := fmt.Fprint(conn, "GET "+RegistryPath+"library/alpine/blobs/x HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	// Read nothing more.
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a stalled reader was not cut")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler still blocked")
	}
}
