// SPDX-License-Identifier: MIT

package e2e

import "net/netip"

func netPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }
