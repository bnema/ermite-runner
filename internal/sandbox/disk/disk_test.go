// SPDX-License-Identifier: MIT

package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

func TestCreateScratchIsSparseExclusiveAndNoFollow(t *testing.T) {
	dir := t.TempDir()
	run := &cmdrun.Fake{}
	p := filepath.Join(dir, "scratch.ext4")
	if err := CreateScratch(context.Background(), run, p, 64, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Size() != 64<<20 || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v", st)
	}
	if got := run.Lines(); len(got) != 1 || !strings.Contains(got[0], "mkfs.ext4") || !strings.Contains(got[0], "^has_journal") {
		t.Fatalf("mkfs call: %v", got)
	}
	if err := CreateScratch(context.Background(), run, p, 64, -1); err == nil {
		t.Fatal("an existing file must be refused")
	}
	victim := filepath.Join(dir, "victim")
	_ = os.WriteFile(victim, []byte("x"), 0o600)
	link := filepath.Join(dir, "link")
	_ = os.Symlink(victim, link)
	if err := CreateScratch(context.Background(), run, link, 1, -1); err == nil {
		t.Fatal("a symlink must be refused")
	}
	// mkfs failure is reported.
	bad := &cmdrun.Fake{Handle: func(cmdrun.Call) ([]byte, error) { return nil, errors.New("boom") }}
	if err := CreateScratch(context.Background(), bad, filepath.Join(dir, "x.ext4"), 1, -1); err == nil || !strings.Contains(err.Error(), "format scratch disk") {
		t.Fatalf("%v", err)
	}
}

// C14: the mkfs.ext4 version is checked.
func TestCheckMkfs(t *testing.T) {
	ver := func(out string, err error) cmdrun.Runner {
		return &cmdrun.Fake{Handle: func(cmdrun.Call) ([]byte, error) { return []byte(out), err }}
	}
	for name, tc := range map[string]struct {
		r  cmdrun.Runner
		ok bool
	}{
		"current":      {ver("mke2fs 1.47.4 (6-Mar-2025)\n", nil), true},
		"exit 1 on -V": {ver("mke2fs 1.46.5 (30-Dec-2021)\nUsing EXT2FS Library version 1.46.5\n", errors.New("exit status 1")), true},
		"minimum":      {ver("mke2fs 1.43 (17-May-2016)\n", nil), true},
		"too old":      {ver("mke2fs 1.42.13 (17-May-2015)\n", nil), false},
		"major 0":      {ver("mke2fs 0.9 (1-Jan-2000)\n", nil), false},
		"garbage":      {ver("hello\n", nil), false},
		"missing":      {ver("", errors.New("executable file not found")), false},
	} {
		err := CheckMkfs(context.Background(), tc.r)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v ok=%v", name, err, tc.ok)
		}
	}
	if err := CheckMkfs(context.Background(), ver("mke2fs 1.42.9 (4-Feb-2014)", nil)); err == nil || !strings.Contains(err.Error(), "1.43") {
		t.Errorf("the error must name the minimum: %v", err)
	}
}

const mountinfo = `22 28 0:21 / /sys rw,nosuid shared:7 - sysfs sysfs rw
36 28 7:0 / /var/lib/ermite-runner/containers/er00112233/scratch rw,nosuid,nodev shared:9 - ext4 /dev/loop0 rw
37 28 7:1 / /srv/with\040space rw - ext4 /dev/loop1 rw
`

func TestMounterIsMountedAndUnmount(t *testing.T) {
	mi := filepath.Join(t.TempDir(), "mountinfo")
	_ = os.WriteFile(mi, []byte(mountinfo), 0o644)
	f := &cmdrun.Fake{}
	m := Mounter{Run: f, Mountinfo: mi}
	for dir, want := range map[string]bool{
		"/var/lib/ermite-runner/containers/er00112233/scratch": true,
		"/srv/with space": true,
		"/var/lib/ermite-runner/containers/er00112233": false,
		"/nowhere": false,
	} {
		if got, err := m.IsMounted(dir); err != nil || got != want {
			t.Errorf("%s: %v %v", dir, got, err)
		}
	}
	if err := m.Unmount(context.Background(), "/nowhere"); err != nil || len(f.Calls) != 0 {
		t.Fatalf("a directory that is not mounted needs no umount: %v %v", err, f.Lines())
	}
	if err := m.Unmount(context.Background(), "/var/lib/ermite-runner/containers/er00112233/scratch"); err != nil {
		t.Fatal(err)
	}
	if got := f.Lines(); len(got) != 1 || got[0] != "umount /var/lib/ermite-runner/containers/er00112233/scratch" {
		t.Fatalf("%v", got)
	}
	if err := m.Mount(context.Background(), "/s/scratch.ext4", "/s/mnt"); err != nil {
		t.Fatal(err)
	}
	if last := f.Lines()[1]; !strings.Contains(last, "loop,noatime") || !strings.HasSuffix(last, "/s/scratch.ext4 /s/mnt") {
		t.Fatalf("mount call: %s", last)
	}
	bad := Mounter{Run: &cmdrun.Fake{Handle: func(cmdrun.Call) ([]byte, error) { return nil, errors.New("busy") }}, Mountinfo: mi}
	if err := bad.Unmount(context.Background(), "/srv/with space"); err == nil {
		t.Fatal("umount failure must be reported")
	}
	if _, err := (Mounter{Mountinfo: "/nonexistent/mountinfo"}).IsMounted("/x"); err == nil {
		t.Fatal("an unreadable mountinfo must be an error, not 'not mounted'")
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	for in, want := range map[string]string{
		`/plain`: "/plain", `/a\040b`: "/a b", `/tab\011x`: "/tab\tx", `/bs\134x`: `/bs\x`, `/bad\9zz`: `/bad\9zz`, `/end\04`: `/end\04`,
	} {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
