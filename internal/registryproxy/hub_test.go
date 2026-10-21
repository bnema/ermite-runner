// SPDX-License-Identifier: MIT

package registryproxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bnema/zerowrap"
)

// An opt-in manual check using curl against a real local HTTP listener, with
// Docker Hub as upstream. No daemon is required.
func TestDockerHubCurl(t *testing.T) {
	if os.Getenv("ERMITE_RUNNER_IT_REGISTRY") != "1" {
		t.Skip("set ERMITE_RUNNER_IT_REGISTRY=1 for live Docker Hub curl check")
	}
	var logs bytes.Buffer
	p, err := New(Config{Dir: t.TempDir(), Max: 64 << 20, Log: zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logs})})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	curl := func(path string) []byte {
		t.Helper()
		out, err := exec.Command("curl", "--max-time", "20", "-fsS", "-H", "Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json", srv.URL+path).CombinedOutput()
		if err != nil {
			t.Fatalf("curl %s: %v: %s", path, err, out)
		}
		t.Logf("curl %s: HTTP 200, %d bytes", path, len(out))
		return out
	}
	var idx struct {
		Manifests []struct {
			Digest   string
			Platform struct{ Architecture, OS string }
		}
	}
	b := curl("/v2/library/alpine/manifests/3")
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, curl("/v2/library/alpine/manifests/3")) {
		t.Fatal("tag changed during check")
	}
	var digest string
	for _, m := range idx.Manifests {
		if m.Platform.Architecture == "amd64" && m.Platform.OS == "linux" {
			digest = m.Digest
			break
		}
	}
	if !validDigest(digest) {
		t.Fatal("no linux/amd64 manifest")
	}
	var manifest struct{ Config struct{ Digest string } }
	if err := json.Unmarshal(curl("/v2/library/alpine/manifests/"+digest), &manifest); err != nil {
		t.Fatal(err)
	}
	if !validDigest(manifest.Config.Digest) {
		t.Fatal("no config blob")
	}
	path := "/v2/library/alpine/blobs/" + manifest.Config.Digest
	first := curl(path)
	second := curl(path)
	if !bytes.Equal(first, second) {
		t.Fatal("blob differs")
	}
	all := logs.String()
	t.Log(all)
	if !strings.Contains(all, `"hit":true`) || strings.Count(all, `"message":"registry blob"`) != 1 {
		t.Fatal("missing cache hits or repeated blob miss")
	}
}
