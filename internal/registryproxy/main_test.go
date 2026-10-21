// SPDX-License-Identifier: MIT
package registryproxy

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// TestMain fails the run if any store hit an accounting bug (unpinLocked of
// an unpinned entry), whichever test caused it.
func TestMain(m *testing.M) {
	var bugs atomic.Int32
	defaultBugHook = func(msg string) {
		bugs.Add(1)
		fmt.Fprintln(os.Stderr, "registry cache bug:", msg)
	}
	code := m.Run()
	if n := bugs.Load(); n > 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d registry cache accounting bugs\n", n)
		code = 1
	}
	os.Exit(code)
}
