// SPDX-License-Identifier: MIT

package container

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moby/moby/client"
)

// infoDaemon plays the two endpoints Info reads. versionBody "" makes /version
// fail with a 500.
func infoDaemon(t *testing.T, versionBody string) *dockerEngine {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"OSType":"linux","SecurityOptions":["name=seccomp,profile=builtin"],"Runtimes":{"runc":{"path":"runc"}}}`))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		if versionBody == "" {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(versionBody))
	})
	srv := httptest.NewServer(http.StripPrefix("/v1.43", mux))
	t.Cleanup(srv.Close)
	cli, err := client.New(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &dockerEngine{cli: cli}
}

// GO-004: the Podman flag comes from the version components' names; a daemon
// whose /version fails (or names no Podman) is treated as Docker.
func TestInfoDetectsPodman(t *testing.T) {
	for name, tc := range map[string]struct {
		version string
		podman  bool
	}{
		"podman":         {`{"Version":"6.1.3","Components":[{"Name":"Podman Engine","Version":"6.1.3"},{"Name":"Conmon","Version":"2.2.1"}]}`, true},
		"podman later":   {`{"Components":[{"Name":"Conmon","Version":"2"},{"Name":"Podman Engine","Version":"6"}]}`, true},
		"docker":         {`{"Version":"29.9.0","Components":[{"Name":"Engine","Version":"29.9.0"},{"Name":"containerd","Version":"2"}]}`, false},
		"no components":  {`{"Version":"29.9.0"}`, false},
		"lower case":     {`{"Components":[{"Name":"podman engine","Version":"1"}]}`, false}, // the name is "Podman Engine": exact prefix
		"version fails":  {"", false},
		"malformed json": {`{not json`, false},
	} {
		t.Run(name, func(t *testing.T) {
			di, err := infoDaemon(t, tc.version).Info(t.Context())
			if err != nil {
				t.Fatalf("a /version failure must not fail Info: %v", err)
			}
			if di.Podman != tc.podman {
				t.Fatalf("Podman=%v want %v", di.Podman, tc.podman)
			}
			if di.OSType != "linux" || len(di.Runtimes) != 1 || di.Rootless {
				t.Fatalf("the rest of Info is unchanged: %+v", di)
			}
		})
	}
}

func TestIsPodman(t *testing.T) {
	if isPodman(nil) {
		t.Fatal("no components is not podman")
	}
}
