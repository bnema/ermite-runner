// SPDX-License-Identifier: MIT
package registryproxy

import (
	"sort"
	"strings"
)

var manifestTypes = []string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}

// normalizeAccept bounds both negotiation and metadata cardinality to the
// fifteen nonempty subsets of the supported manifest types.
func normalizeAccept(raw string) string {
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		typ, _, _ := strings.Cut(part, ";")
		typ = strings.ToLower(strings.TrimSpace(typ))
		for _, known := range manifestTypes {
			if typ == known {
				seen[typ] = true
			}
		}
	}
	if len(seen) == 0 {
		for _, typ := range manifestTypes {
			seen[typ] = true
		}
	}
	types := make([]string, 0, len(seen))
	for typ := range seen {
		types = append(types, typ)
	}
	sort.Strings(types)
	return strings.Join(types, ",")
}
