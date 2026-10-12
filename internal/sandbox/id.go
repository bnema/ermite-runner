// SPDX-License-Identifier: MIT

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// NewID returns "er" + 8 random hex digits: short (socket paths), unique enough
// for concurrent sandboxes, and safe in file, interface and Docker object names.
func NewID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return "er" + hex.EncodeToString(b[:]), nil
}

// ValidID reports whether id has the form produced by NewID. It guards paths and
// process matches built from ids read back from disk or from /proc.
func ValidID(id string) bool {
	if len(id) != 10 || !strings.HasPrefix(id, "er") {
		return false
	}
	_, err := hex.DecodeString(id[2:])
	return err == nil
}
