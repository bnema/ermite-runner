// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/container"
	"bnema.dev/ermite-runner/internal/sandbox/disk"
	"bnema.dev/ermite-runner/internal/sandbox/sandboxtest"
)

// newCacheDriver is newDriver with the cache service of the runner.
func newCacheDriver(t *testing.T, image string, svc *cachesvc.Service) sandbox.Driver {
	t.Helper()
	dir := t.TempDir()
	cfg := sandbox.Config{
		Isolation: sandbox.Container, StateDir: filepath.Join(dir, "state"), Capacity: 2, Image: image,
		JobCPUs: 2, JobMemoryMiB: 1024, JobDiskMiB: 2048, UIDBase: 900000,
		Network: netPrefix("10.215.0.0/16"), Executable: buildRunner(t, dir), DNSPort: 0,
		LockFile: filepath.Join(dir, "host.lock"), JobPids: 4096, Cache: svc,
	}
	empty := filepath.Join(dir, "mountinfo")
	_ = os.WriteFile(empty, nil, 0o644)
	run := rootlessMountRunner{image: image, mountinfo: empty, mu: &sync.Mutex{}}
	d, err := container.New(context.Background(), cfg, container.Options{
		Net: sandboxtest.NewPlumbing(), UnsafeAllowRootlessForTests: true, ConnectTimeout: 90 * time.Second, Run: run,
		Mounter: &disk.Mounter{Run: run, Mountinfo: empty},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// cacheClient is what @actions/cache does against the v1 API, in python (the
// image has it): reserve, upload a chunk, commit, look up, download the archive.
// It talks to 127.0.0.1:9089, the loopback of the container, where the relay of
// the guest delivers every connection to the runner over the cache socket.
const cacheClient = `
import json, sys, urllib.request, urllib.error
base = "http://127.0.0.1:%d/_apis/artifactcache/"
tok = "%s"
def call(method, path, body=None, headers=None):
    h = {"Authorization": "Bearer " + tok, "Accept": "application/json;api-version=6.0-preview.1"}
    h.update(headers or {})
    req = urllib.request.Request(base + path, data=body, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
data = bytes(range(256)) * 4096          # 1 MiB
code, body = call("GET", "cache?keys=e2e-key&version=v1")
print("first lookup", code)
code, body = call("POST", "caches", json.dumps({"key": "e2e-key", "version": "v1", "cacheSize": len(data)}).encode(), {"Content-Type": "application/json"})
print("reserve", code)
cid = json.loads(body)["cacheId"]
half = len(data) // 2
for lo, hi in ((half, len(data)), (0, half)):     # out of order, like the toolkit may
    code, _ = call("PATCH", "caches/%%d" %% cid, data[lo:hi], {"Content-Type": "application/octet-stream", "Content-Range": "bytes %%d-%%d/*" %% (lo, hi - 1)})
    print("chunk", lo, code)
code, _ = call("POST", "caches/%%d" %% cid, json.dumps({"size": len(data)}).encode(), {"Content-Type": "application/json"})
print("commit", code)
code, body = call("GET", "cache?keys=e2e-key&version=v1")
print("second lookup", code)
loc = json.loads(body)["archiveLocation"]
print("archive host", loc.split("/")[2])
with urllib.request.urlopen(loc, timeout=20) as r:
    got = r.read()
print("download", "same" if got == data else "DIFFERENT", len(got))
code, _ = call("GET", "cache?keys=e2e-key&version=v1", None, {"Authorization": "Bearer nope"})
print("bad token", code)
`

// The cache works from inside a container: the service listens on no network,
// the guest relay turns its loopback port into the cache socket of the
// container, and a client in the job does a full save and restore through it.
// A token made for another job is refused on this container's socket.
func TestContainerCacheOnTheGuestLoopback(t *testing.T) {
	image := needE2E(t)
	svc := sandboxtest.NewCache(t)
	d := newCacheDriver(t, image, svc)

	tok := sandboxtest.CacheToken(svc, "e2e")
	spec := jobSpec(fmt.Sprintf(`on: push
jobs:
  j:
    runs-on: x
    steps:
      - name: cache client
        shell: python3 {0}
        run: |
%s
      - name: nothing listens on the gateway
        shell: python3 {0}
        run: |
          import socket, struct
          gw = ""
          for line in open("/proc/net/route").read().splitlines()[1:]:
              f = line.split()
              if f[1] == "00000000":
                  gw = socket.inet_ntoa(struct.pack("<L", int(f[2], 16)))
          print("default gateway", gw)
          for port in (8089, 9089):
              s = socket.socket(); s.settimeout(2)
              try:
                  s.connect((gw, port)); print("gateway", gw, port, "OPEN")
              except Exception as e:
                  print("gateway", port, "closed")
              s.close()
`, indent(fmt.Sprintf(cacheClient, cachesvc.DefaultPort, tok), 10)))
	spec.CachePort, spec.CacheToken = cachesvc.DefaultPort, tok

	s := runJob(t, d, spec, nil)
	all := strings.Join(s.Logs, "\n")
	if s.Result == nil || s.Result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || s.Exit.Code != 0 {
		t.Fatalf("result %v exit %v\n%s", s.Result, s.Exit, all)
	}
	for _, want := range []string{
		"first lookup 204", "reserve 201", "chunk 524288 204", "chunk 0 204", "commit 204", "second lookup 200",
		"archive host 127.0.0.1:9089", "download same 1048576", "bad token 403",
		"default gateway 10.215.", "gateway 8089 closed", "gateway 9089 closed",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q\n%s", want, all)
		}
	}
	// (The log also echoes the script, which contains the word: match the output.)
	for _, port := range []string{"8089", "9089"} {
		if strings.Contains(all, "10.215.0.1 "+port+" OPEN") {
			t.Errorf("the cache port %s answers on the gateway:\n%s", port, all)
		}
	}
}

// A job whose token was made for another job gets 403 on its own socket.
func TestContainerCacheRefusesAnotherJobsToken(t *testing.T) {
	image := needE2E(t)
	svc := sandboxtest.NewCache(t)
	d := newCacheDriver(t, image, svc)
	tok := sandboxtest.CacheToken(svc, "someone-else")
	spec := jobSpec(fmt.Sprintf(`on: push
jobs:
  j:
    runs-on: x
    steps:
      - shell: python3 {0}
        run: |
%s
`, indent(fmt.Sprintf(`import urllib.request, urllib.error
req = urllib.request.Request("http://127.0.0.1:%d/_apis/artifactcache/cache?keys=k&version=v", headers={"Authorization": "Bearer %s"})
try:
    urllib.request.urlopen(req, timeout=20); print("status 200")
except urllib.error.HTTPError as e:
    print("status", e.code)
`, cachesvc.DefaultPort, tok), 10)))
	spec.CachePort, spec.CacheToken = cachesvc.DefaultPort, tok
	s := runJob(t, d, spec, nil)
	all := strings.Join(s.Logs, "\n")
	if s.Result == nil || s.Result.Conclusion != execpb.StepConclusion_STEP_CONCLUSION_SUCCESS || !strings.Contains(all, "status 403") {
		t.Fatalf("result %v\n%s", s.Result, all)
	}
}

// indent prefixes every non-empty line of s with n spaces.
func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
