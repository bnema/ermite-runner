// SPDX-License-Identifier: MIT

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The job's /tmp is world-writable and sticky, as on GitHub's runners: a
// program that refuses a replaceable temp directory (a writable ancestor
// without the sticky bit) must accept it.
func TestTmpModeIsStickyAndWorldWritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, TmpMode); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSticky == 0 || fi.Mode().Perm() != 0o777 {
		t.Fatalf("mode %v, want sticky and 0777", fi.Mode())
	}
}
