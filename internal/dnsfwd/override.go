// SPDX-License-Identifier: MIT

package dnsfwd

import (
	"encoding/binary"
	"net/netip"
	"strings"
)

const (
	typeA   = 1
	classIN = 1
	// overrideTTL is short: the sandbox lives for one job.
	overrideTTL = 30
)

// overrideName returns Override normalised for comparison: lower case, without
// a trailing dot.
func (f *Forwarder) overrideName() string {
	return strings.TrimSuffix(strings.ToLower(f.Override), ".")
}

// questionName reads the name of the question that starts at q[hdrLen]. It
// returns the lower-cased name without trailing dot and the offset just after
// the name. Compression pointers and over-long labels are refused (a question
// never uses them): ok is false and the query is forwarded as usual.
func questionName(q []byte) (name string, end int, ok bool) {
	var sb strings.Builder
	i := hdrLen
	for {
		if i >= len(q) {
			return "", 0, false
		}
		n := int(q[i])
		i++
		if n == 0 {
			return sb.String(), i, true
		}
		if n&0xC0 != 0 || i+n > len(q) {
			return "", 0, false
		}
		if sb.Len() > 0 {
			sb.WriteByte('.')
		}
		sb.WriteString(strings.ToLower(string(q[i : i+n])))
		i += n
		if sb.Len() > 253 {
			return "", 0, false
		}
	}
}

// overrideAnswer builds the local answer to q when q asks for the overridden
// name in class IN, else nil. A gets addr; every other type gets NOERROR with
// no data (so AAAA is an empty answer: sandboxes have no IPv6, and an upstream
// NXDOMAIN or CNAME for the name must not leak in).
func (f *Forwarder) overrideAnswer(addr netip.Addr, q []byte) []byte {
	want := f.overrideName()
	if want == "" || len(q) < hdrLen {
		return nil
	}
	// A plain query: QR=0, opcode 0, one question, nothing else but additional
	// records (EDNS).
	if q[2]&0xF8 != 0 || binary.BigEndian.Uint16(q[4:]) != 1 ||
		binary.BigEndian.Uint16(q[6:]) != 0 || binary.BigEndian.Uint16(q[8:]) != 0 {
		return nil
	}
	name, end, ok := questionName(q)
	if !ok || name != want || end+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end:])
	if binary.BigEndian.Uint16(q[end+2:]) != classIN {
		return nil
	}
	end += 4
	withA := qtype == typeA && addr.Is4()

	r := make([]byte, 0, end+16+11)
	r = append(r, q[0], q[1])
	r = append(r, 0x80|(q[2]&0x01), 0x80) // QR, RD copied; RA; NOERROR
	r = append(r, 0, 1)                   // QDCOUNT
	if withA {
		r = append(r, 0, 1)
	} else {
		r = append(r, 0, 0)
	}
	r = append(r, 0, 0) // NSCOUNT
	// An EDNS query gets an OPT record back (RFC 6891), without options.
	edns := binary.BigEndian.Uint16(q[10:]) >= 1 && len(q) >= end+11 &&
		q[end] == 0 && binary.BigEndian.Uint16(q[end+1:]) == 41
	if edns {
		r = append(r, 0, 1)
	} else {
		r = append(r, 0, 0)
	}
	r = append(r, q[hdrLen:end]...)
	if withA {
		r = append(r, 0xC0, hdrLen, 0, typeA, 0, classIN)
		r = binary.BigEndian.AppendUint32(r, overrideTTL)
		a := addr.As4()
		r = append(r, 0, 4, a[0], a[1], a[2], a[3])
	}
	if edns {
		r = append(r, 0, 0, 41, 0x10, 0, 0, 0, 0, 0, 0, 0) // root, OPT, 4096 bytes, no options
	}
	return r
}
