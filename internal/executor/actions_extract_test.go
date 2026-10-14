// SPDX-License-Identifier: MIT

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tent struct {
	name, body, link string
	typ              byte
}

func tarball(t *testing.T, ents ...tent) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	return bytes.NewReader(buf.Bytes())
}

func TestExtractTarball(t *testing.T) {
	dir := t.TempDir()
	err := extractTarball(context.Background(), tarball(t,
		tent{name: "o-r-abc/", typ: tar.TypeDir},
		tent{name: "o-r-abc/action.yml", body: "name: x"},
		tent{name: "o-r-abc/sub/main.js", body: "1"},
		tent{name: "o-r-abc/link", typ: tar.TypeSymlink, link: "sub/main.js"},
		tent{name: "o-r-abc/hard", typ: tar.TypeLink, link: "o-r-abc/sub/main.js"},
	), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The top directory is stripped: the content is in dir itself.
	for _, f := range []string{"action.yml", "link", "hard"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "link")); string(b) != "1" {
		t.Errorf("link: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "o-r-abc")); err == nil {
		t.Error("the top directory must be stripped")
	}
}

func TestExtractTarballRejects(t *testing.T) {
	cases := map[string][]tent{
		"absolute":       {{name: "/etc/passwd", body: "x"}},
		"dotdot":         {{name: "t/a/../../x", body: "x"}},
		"symlink out":    {{name: "t/a/l", typ: tar.TypeSymlink, link: "../../x"}},
		"symlink abs":    {{name: "t/l", typ: tar.TypeSymlink, link: "/etc"}},
		"hardlink out":   {{name: "t/l", typ: tar.TypeLink, link: "../x"}},
		"hardlink abs":   {{name: "t/l", typ: tar.TypeLink, link: "/etc/passwd"}},
		"through link":   {{name: "t/d", typ: tar.TypeSymlink, link: "."}, {name: "t/d/../../x", body: "x"}},
		"link then dir":  {{name: "t/l", typ: tar.TypeSymlink, link: "a/.."}, {name: "t/l/../x", body: "x"}},
		"two tops":       {{name: "t/a", body: "1"}, {name: "u/b", body: "2"}},
		"file beside":    {{name: "t/", typ: tar.TypeDir}, {name: "loose", body: "x"}},
		"hardlink other": {{name: "t/a", body: "1"}, {name: "t/l", typ: tar.TypeLink, link: "u/a"}},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "dest")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractTarball(context.Background(), tarball(t, ents...), dir, nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if _, statErr := os.Lstat(filepath.Join(parent, "x")); statErr == nil {
				t.Fatal("wrote outside")
			}
		})
	}
}

// GO-001: "top/link -> ../outside" stays inside the staging directory of the
// extraction but escapes once the top directory is stripped. The target is
// resolved against the final root, whatever the location the files are
// extracted to, and the relocated tree has no link that leaves it.
func TestExtractTarballLinkEscapesStrippedRoot(t *testing.T) {
	// From top/sub/l, i.e. sub/l once the top directory is stripped.
	for link, ok := range map[string]bool{"../outside": true, "..": true, "../x": true, "../../outside": false, "../../top/x": false, "../../../x": false} {
		dir := filepath.Join(t.TempDir(), "stage")
		_ = os.Mkdir(dir, 0o755)
		err := extractTarball(context.Background(), tarball(t,
			tent{name: "top/x", body: "x"},
			tent{name: "top/sub/l", typ: tar.TypeSymlink, link: link},
		), dir, nil)
		if (err == nil) != ok {
			t.Errorf("link %q: accepted=%v, want %v (%v)", link, err == nil, ok, err)
		}
	}
	// The direct case of the finding.
	dir := filepath.Join(t.TempDir(), "stage")
	_ = os.Mkdir(dir, 0o755)
	if err := extractTarball(context.Background(), tarball(t,
		tent{name: "top/link", typ: tar.TypeSymlink, link: "../outside"},
	), dir, nil); err == nil {
		t.Fatal("top/link -> ../outside was accepted")
	}
	// A tree that was accepted keeps all its links inside after relocation.
	stage := filepath.Join(t.TempDir(), "stage")
	_ = os.Mkdir(stage, 0o755)
	if err := extractTarball(context.Background(), tarball(t,
		tent{name: "top/real", body: "r"},
		tent{name: "top/a/b/l", typ: tar.TypeSymlink, link: "../../real"},
	), stage, nil); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(t.TempDir(), "final")
	if err := os.Rename(stage, final); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(final, "a", "b", "l")); err != nil || string(b) != "r" {
		t.Fatalf("relocated link: %q %v", b, err)
	}
}

func TestExtractTarballNotGzip(t *testing.T) {
	err := extractTarball(context.Background(), strings.NewReader("not gzip"), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "not a gzip") {
		t.Fatalf("%v", err)
	}
}

// A symlink to a directory inside the tree must not let a later entry write
// outside it, even through a chain of links.
func TestExtractTarballLinkChain(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "dest")
	_ = os.Mkdir(dir, 0o755)
	err := extractTarball(context.Background(), tarball(t,
		tent{name: "t/a", typ: tar.TypeSymlink, link: "b"},
		tent{name: "t/b", typ: tar.TypeSymlink, link: "."},
		tent{name: "t/a/a/a/f", body: "x"},
	), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "x" {
		t.Fatalf("f: %q", b)
	}
}

// GO-004: PAX and GNU long-name records are not entries and have no Size in the
// header Next returns, so the counters do not see them. The decompressed stream
// is capped.
func TestExtractTarballMetadataFlood(t *testing.T) {
	old := maxStreamBytes
	maxStreamBytes = 256 << 10
	defer func() { maxStreamBytes = old }()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i := 0; i < 400; i++ {
		// A PAX record per file with a 4 KiB value; the entries stay below
		// every counter.
		h := &tar.Header{Name: "t/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"comment": strings.Repeat("x", 4096)}}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	err := extractTarball(context.Background(), bytes.NewReader(buf.Bytes()), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "too large once unpacked") {
		t.Fatalf("%v", err)
	}
}

func TestExtractTarballCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := extractTarball(ctx, tarball(t, tent{name: "t/a", body: "1"}), t.TempDir(), nil)
	if err != context.Canceled {
		t.Fatalf("%v", err)
	}
}
