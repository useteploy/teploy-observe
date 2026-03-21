// +build ignore

// Generates geo_ranges.bin — a binary file of sorted (start_u32, end_u32, cc[2]) records.
// Each record is 10 bytes: 4 + 4 + 2.
//
// Run: go run gen_binary.go

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

type entry struct {
	start   uint32
	end     uint32
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
	var entries []entry

	for _, url := range sources {
		fmt.Fprintf(os.Stderr, "fetching %s\n", url)
		resp, err := http.Get(url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "#") || line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) < 5 {
				continue
			}
			cc := parts[1]
			typ := parts[2]
			startStr := parts[3]
			countStr := parts[4]

			if typ != "ipv4" || cc == "*" || cc == "" || len(cc) != 2 {
				continue
			}

			ip := net.ParseIP(startStr)
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			count, err := strconv.ParseUint(countStr, 10, 32)
			if err != nil || count == 0 {
				continue
			}

			start := binary.BigEndian.Uint32(ip4)
			end := start + uint32(count) - 1
			var ccBytes [2]byte
			copy(ccBytes[:], strings.ToUpper(cc))

			entries = append(entries, entry{start: start, end: end, country: ccBytes})
		}
		resp.Body.Close()
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].start < entries[j].start
	})

	f, err := os.Create("geo_ranges.bin")
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

	fmt.Fprintf(os.Stderr, "wrote %d entries (%d bytes)\n", len(entries), len(entries)*10)
}
