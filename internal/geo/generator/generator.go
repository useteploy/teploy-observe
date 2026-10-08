// Package generator builds the embedded country tables without replacing a
// working table until every registry has been downloaded and validated.
package generator

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var Sources = []string{
	"https://ftp.apnic.net/stats/apnic/delegated-apnic-latest",
	"https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest",
	"https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest",
	"https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-latest",
	"https://ftp.afrinic.net/pub/stats/afrinic/delegated-afrinic-latest",
}

type SourceReport struct {
	URL, Header, SHA256 string
	Entries             int
}
type Report struct {
	Sources []SourceReport
	Entries int
	SHA256  string
}
type entry struct {
	start, end uint64
	country    [2]byte
}

// Generate returns provenance for the successful output. Callers can save its
// JSON report with the generated asset to pin the exact registry inputs.
func Generate(ctx context.Context, client *http.Client, sources []string, output string, ipv6 bool) (*Report, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("no registry sources")
	}
	report := &Report{}
	var entries []entry
	for _, source := range sources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", source, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: HTTP %d", source, resp.StatusCode)
		}
		// Bound memory, and detect rather than silently accept truncation.
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20+1))
		closeErr := resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", source, err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > 64<<20 {
			return nil, fmt.Errorf("registry %s exceeds 64 MiB", source)
		}
		parsed, header, err := parse(string(data), ipv6)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", source, err)
		}
		digest := sha256.Sum256(data)
		report.Sources = append(report.Sources, SourceReport{URL: source, Header: header, SHA256: hex.EncodeToString(digest[:]), Entries: len(parsed)})
		entries = append(entries, parsed...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].start < entries[j].start })
	var data []byte
	for i, e := range entries {
		if i > 0 && e.start <= entries[i-1].end {
			return nil, fmt.Errorf("overlapping registry ranges at %d", i)
		}
		if ipv6 {
			data = binary.BigEndian.AppendUint64(data, e.start)
			data = binary.BigEndian.AppendUint64(data, e.end)
		} else {
			data = binary.BigEndian.AppendUint32(data, uint32(e.start))
			data = binary.BigEndian.AppendUint32(data, uint32(e.end))
		}
		data = append(data, e.country[:]...)
	}
	if err := atomicWrite(output, data); err != nil {
		return nil, err
	}
	report.Entries = len(entries)
	digest := sha256.Sum256(data)
	report.SHA256 = hex.EncodeToString(digest[:])
	return report, nil
}

func parse(data string, ipv6 bool) ([]entry, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var entries []entry
	header := ""
	declared, records := 0, 0
	targetRecords, targetDeclared := 0, -1
	targetType := "ipv4"
	if ipv6 {
		targetType = "ipv6"
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if header == "" {
			if len(parts) < 7 || (parts[0] != "2" && !strings.HasPrefix(parts[0], "2.")) || parts[1] == "" || parts[2] == "" {
				return nil, "", fmt.Errorf("missing version-2 registry header")
			}
			header = line
			var err error
			declared, err = strconv.Atoi(parts[3])
			if err != nil || declared <= 0 {
				return nil, "", fmt.Errorf("invalid declared record count")
			}

			continue
		}
		if len(parts) < 6 {
			return nil, "", fmt.Errorf("malformed registry record")
		}
		cc, typ := parts[1], parts[2]
		if cc == "*" && parts[5] == "summary" {
			if typ == targetType {
				if targetDeclared >= 0 {
					return nil, "", fmt.Errorf("duplicate target summary")
				}
				count, err := strconv.Atoi(parts[4])
				if err != nil || count < 0 {
					return nil, "", fmt.Errorf("invalid target summary count")
				}
				targetDeclared = count
			}
			continue
		} // registry summary
		if len(parts) < 7 {
			return nil, "", fmt.Errorf("incomplete registry record")
		}
		records++
		if typ != "asn" && typ != "ipv4" && typ != "ipv6" {
			return nil, "", fmt.Errorf("unknown record type %q", typ)
		}
		if typ != targetType {
			continue
		}
		targetRecords++
		if cc == "" || cc == "*" {
			continue
		} // unallocated/reserved ranges carry no country
		if len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
			return nil, "", fmt.Errorf("invalid country %q", cc)
		}
		ip := net.ParseIP(parts[3])
		if ip == nil {
			return nil, "", fmt.Errorf("invalid address %q", parts[3])
		}
		e := entry{country: [2]byte{cc[0], cc[1]}}
		if ipv6 {
			if ip.To4() != nil {
				return nil, "", fmt.Errorf("IPv4 in IPv6 record")
			}
			prefix, err := strconv.Atoi(parts[4])
			if err != nil || prefix < 1 || prefix > 64 {
				return nil, "", fmt.Errorf("unsupported IPv6 prefix %q", parts[4])
			}
			e.start = binary.BigEndian.Uint64(ip.To16()[:8])
			mask := ^uint64(0) >> uint(prefix)
			if e.start&mask != 0 || binary.BigEndian.Uint64(ip.To16()[8:]) != 0 {
				return nil, "", fmt.Errorf("unaligned IPv6 range")
			}
			e.end = e.start | mask
		} else {
			ip4 := ip.To4()
			if ip4 == nil {
				return nil, "", fmt.Errorf("IPv6 in IPv4 record")
			}
			count, err := strconv.ParseUint(parts[4], 10, 64)
			if err != nil || count == 0 {
				return nil, "", fmt.Errorf("invalid IPv4 count %q", parts[4])
			}
			e.start = uint64(binary.BigEndian.Uint32(ip4))
			if count > (1<<32)-e.start {
				return nil, "", fmt.Errorf("IPv4 range overflow")
			}
			e.end = e.start + count - 1
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	// Validate the family we consume against its summary. LACNIC's current
	// ASN summary is stale while both IP summaries match the served rows; an
	// unrelated ASN count must not prevent a complete IP refresh.
	if targetDeclared >= 0 {
		if targetRecords != targetDeclared {
			return nil, "", fmt.Errorf("incomplete %s registry: got %d records, expected %d", targetType, targetRecords, targetDeclared)
		}
	} else if records != declared {
		return nil, "", fmt.Errorf("incomplete registry: got %d records, expected %d", records, declared)
	}
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("registry has no country ranges")
	}
	return entries, header, nil
}

func atomicWrite(output string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(output), ".geo-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), output)
}

// Run is the common command entrypoint; a failed refresh exits without changing
// the prior binary. Network deadlines apply to the complete refresh.
func Run(ipv6 bool) error {
	output := "geo_ranges.bin"
	if ipv6 {
		output = "geo_ranges6.bin"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	report, err := Generate(ctx, &http.Client{Timeout: time.Minute}, Sources, output, ipv6)
	if err != nil {
		return err
	}
	// Report serialization cannot fail for this string/int-only shape.
	fmt.Fprintf(os.Stderr, "wrote %d entries to %s (sha256 %s)\n", report.Entries, output, report.SHA256)
	return json.NewEncoder(os.Stdout).Encode(report)
}
