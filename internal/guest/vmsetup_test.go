// SPDX-License-Identifier: MIT

package guest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateDevSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := createDevSymlinks(root); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]string{
			"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
			"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
			"core": "/proc/kcore",
		} {
			got, err := os.Readlink(filepath.Join(root, "dev", name))
			if err != nil || got != want {
				t.Errorf("%s: target = %q, error = %v; want %q", name, got, err, want)
			}
		}
	}
}

func TestCreateDevSymlinksPreservesExisting(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "dev")
	if err := os.Mkdir(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "fd"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/missing", filepath.Join(dev, "stdin")); err != nil {
		t.Fatal(err)
	}
	if err := createDevSymlinks(root); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dev, "fd")); err != nil || string(got) != "existing" {
		t.Fatalf("existing file changed: %q, %v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(dev, "stdin")); err != nil || got != "/missing" {
		t.Fatalf("existing dangling symlink changed: %q, %v", got, err)
	}
	if _, err := os.Readlink(filepath.Join(dev, "stderr")); err != nil {
		t.Fatal(err)
	}
}

func TestCreateDevSymlinksFailure(t *testing.T) {
	// A missing /dev deterministically fails even when tests run as root.
	err := createDevSymlinks(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want missing directory error", err)
	}
}
