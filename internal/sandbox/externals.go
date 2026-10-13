// SPDX-License-Identifier: MIT

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// External is one tool the host provides outside the image (Node.js), attached
// read-only at GuestExternalsDir/<Name>.
type External struct {
	// Name is the directory below GuestExternalsDir: node20, node24,
	// node20_alpine, node24_alpine.
	Name string
	// ID identifies the content: it changes whenever the files (or the reason of
	// an unavailable tool) change, so that drivers can key caches on it.
	ID string
	// Dir is the host directory that is attached. For an unavailable tool it
	// holds the file ExternalUnavailableFile with the reason, and a bin/node
	// script that prints it and fails.
	Dir string
	// Unavailable is true when the tool is missing on this runner.
	Unavailable bool
}

// ExternalsSource hands the drivers the current set of externals. It is called
// at every sandbox start and may change between calls (a download finishing).
type ExternalsSource interface {
	Externals() []External
}

// ExternalsKey is the cache key of a set: the same tools give the same key, in
// any order, and any change of content gives another.
func ExternalsKey(set []External) string {
	ids := make([]string, 0, len(set))
	for _, e := range set {
		ids = append(ids, e.Name+"="+e.ID)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ExternalsOf returns the current set of cfg (nil when there is none).
func (c Config) ExternalsOf() []External {
	if c.Externals == nil {
		return nil
	}
	return c.Externals.Externals()
}
