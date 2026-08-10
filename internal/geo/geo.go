package geo

import (
	_ "embed"
	"encoding/binary"
	"net"
	"sort"
)

//go:embed geo_ranges.bin
var rangeData []byte

//go:embed geo_ranges6.bin
var rangeData6 []byte

const recordSize = 10  // 4 (start) + 4 (end) + 2 (country)
const recordSize6 = 18 // 8 (start) + 8 (end) + 2 (country)

// Lookup resolves an IP address to a 2-letter ISO country code, for IPv4 and
// IPv6 alike. Returns empty string when the address is unparseable or falls in
// no delegated range.
//
// IPv6 previously returned empty unconditionally, which silently blanked the
// country of every visitor on a dual-stack connection — the majority of real
// traffic, since browsers prefer IPv6 when it is available.
func Lookup(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	if ip4 := ip.To4(); ip4 != nil {
		return lookupV4(binary.BigEndian.Uint32(ip4))
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return ""
	}
	return lookupV6(binary.BigEndian.Uint64(ip16[:8]))
}

// lookupV6 compares only the top 64 bits, which is what the table stores; RIR
// delegations are never longer than /64 so this loses no resolution.
func lookupV6(prefix uint64) string {
	n := len(rangeData6) / recordSize6
	if n == 0 {
		return ""
	}
	idx := sort.Search(n, func(i int) bool {
		off := i * recordSize6
		end := binary.BigEndian.Uint64(rangeData6[off+8 : off+16])
		return end >= prefix
	})
	if idx < n {
		off := idx * recordSize6
		start := binary.BigEndian.Uint64(rangeData6[off : off+8])
		if prefix >= start {
			return string(rangeData6[off+16 : off+18])
		}
	}
	return ""
}

func lookupV4(ip uint32) string {
	n := len(rangeData) / recordSize
	if n == 0 {
		return ""
	}
	idx := sort.Search(n, func(i int) bool {
		off := i * recordSize
		end := binary.BigEndian.Uint32(rangeData[off+4 : off+8])
		return end >= ip
	})
	if idx < n {
		off := idx * recordSize
		start := binary.BigEndian.Uint32(rangeData[off : off+4])
		if ip >= start {
			return string(rangeData[off+8 : off+10])
		}
	}
	return ""
}
