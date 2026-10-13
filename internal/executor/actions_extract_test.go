// SPDX-License-Identifier: MIT

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	err := extractTarball(tarball(t,
		tent{name: "o-r-abc/", typ: tar.TypeDir},
		tent{name: "o-r-abc/action.yml", body: "name: x"},
		tent{name: "o-r-abc/sub/main.js", body: "1"},
		tent{name: "o-r-abc/link", typ: tar.TypeSymlink, link: "sub/main.js"},
	), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "o-r-abc", "action.yml")); string(b) != "name: x" {
		t.Errorf("action.yml: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "o-r-abc", "link")); string(b) != "1" {
		t.Errorf("link: %q", b)
	}
}

func TestExtractTarballRejects(t *testing.T) {
	cases := map[string][]tent{
		"absolute":      {{name: "/etc/passwd", body: "x"}},
		"dotdot":        {{name: "a/../../x", body: "x"}},
		"symlink out":   {{name: "a/l", typ: tar.TypeSymlink, link: "../../x"}},
		"symlink abs":   {{name: "l", typ: tar.TypeSymlink, link: "/etc"}},
		"hardlink out":  {{name: "l", typ: tar.TypeLink, link: "../x"}},
		"hardlink abs":  {{name: "l", typ: tar.TypeLink, link: "/etc/passwd"}},
		"through link":  {{name: "d", typ: tar.TypeSymlink, link: "."}, {name: "d/../../x", body: "x"}},
		"link then dir": {{name: "l", typ: tar.TypeSymlink, link: "a/.."}, {name: "l/../x", body: "x"}},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "dest")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractTarball(tarball(t, ents...), dir, nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if _, statErr := os.Lstat(filepath.Join(parent, "x")); statErr == nil {
				t.Fatal("wrote outside")
			}
		})
	}
}

func TestExtractTarballNotGzip(t *testing.T) {
	err := extractTarball(strings.NewReader("not gzip"), t.TempDir(), nil)
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
	err := extractTarball(tarball(t,
		tent{name: "a", typ: tar.TypeSymlink, link: "b"},
		tent{name: "b", typ: tar.TypeSymlink, link: "."},
		tent{name: "a/a/a/f", body: "x"},
	), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "x" {
		t.Fatalf("f: %q", b)
	}
}
