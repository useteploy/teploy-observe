package geo

import (
	_ "embed"
	"encoding/binary"
	"net"
	"sort"
)

//go:embed geo_ranges.bin
var rangeData []byte

const recordSize = 10 // 4 (start) + 4 (end) + 2 (country)

// Lookup resolves an IP address to a 2-letter ISO country code.
// Returns empty string if the IP can't be resolved or is IPv6.
func Lookup(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	num := binary.BigEndian.Uint32(ip4)
	return lookupV4(num)
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
