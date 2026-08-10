//go:build ignore

// Generates geo_ranges6.bin — sorted (start_u64, end_u64, cc[2]) records, 18
// bytes each, from the same RIR delegation files the IPv4 table uses.
//
// Only the top 64 bits of each address are stored. RIR delegations are never
// longer than /64 (they run /19 to /48 in practice), so a 64-bit prefix
// compares exactly as well as the full address for country lookup, at half the
// size and with no big-integer comparisons in the hot path.
//
// Run: go run gen_binary6.go
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

type entry6 struct {
	start   uint64
	end     uint64
	country [2]byte
}

var sources = []string{
	"https://ftp.apnic.net/stats/apnic/delegated-apnic-latest",
	"https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest",
	"https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest",
	"https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-latest",
	"https://ftp.afrinic.net/pub/stats/afrinic/delegated-afrinic-latest",
}

func main() {
	var entries []entry6

	for _, url := range sources {
		fmt.Fprintf(os.Stderr, "fetching %s\n", url)
		resp, err := http.Get(url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "#") || line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) < 5 {
				continue
			}
			cc, typ, startStr, prefixStr := parts[1], parts[2], parts[3], parts[4]

			// For ipv6 rows the fifth field is a prefix length, not a count.
			if typ != "ipv6" || cc == "*" || cc == "" || len(cc) != 2 {
				continue
			}
			ip := net.ParseIP(startStr)
			if ip == nil || ip.To16() == nil || ip.To4() != nil {
				continue
			}
			prefix, err := strconv.Atoi(prefixStr)
			if err != nil || prefix < 1 || prefix > 128 {
				continue
			}

			start := binary.BigEndian.Uint64(ip.To16()[:8])
			var end uint64
			if prefix >= 64 {
				// Everything at or below a /64 collapses to a single 64-bit key.
				end = start
			} else {
				end = start | (^uint64(0) >> uint(prefix))
			}

			var ccBytes [2]byte
			copy(ccBytes[:], strings.ToUpper(cc))
			entries = append(entries, entry6{start: start, end: end, country: ccBytes})
		}
		resp.Body.Close()
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].start < entries[j].start })

	f, err := os.Create("geo_ranges6.bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	for _, e := range entries {
		binary.Write(f, binary.BigEndian, e.start)
		binary.Write(f, binary.BigEndian, e.end)
		f.Write(e.country[:])
	}

	fmt.Fprintf(os.Stderr, "wrote %d entries (%d bytes)\n", len(entries), len(entries)*18)
}
