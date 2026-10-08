package generator

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const header = "2|test|20261007|1|20261007|20261007|+0000\n"

func TestRefreshPreservesPriorTableOnFailure(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		valid := "test|US|ipv4|1.0.0.0|256|20261007|allocated\n"
		if ipv6 {
			valid = "test|US|ipv6|2001:db8::|32|20261007|allocated\n"
		}
		for _, c := range []struct {
			name, body string
			status     int
		}{
			{"http", header + valid, 503}, {"empty", "", 200}, {"html", "<html>error</html>", 200},
			{"malformed", header + valid + "broken\n", 200},
			{"truncated", strings.Replace(header, "|1|", "|2|", 1) + valid, 200},
			{"scanner", header + valid + strings.Repeat("x", (1<<20)+1), 200},
			{"overlap", header + valid + valid, 200},
		} {
			t.Run(c.name+map[bool]string{false: "4", true: "6"}[ipv6], func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/ok" {
						w.Write([]byte(header + valid))
						return
					}
					w.WriteHeader(c.status)
					w.Write([]byte(c.body))
				}))
				defer server.Close()
				output := filepath.Join(t.TempDir(), "table.bin")
				prior := []byte("previous verified database")
				if err := os.WriteFile(output, prior, 0644); err != nil {
					t.Fatal(err)
				}
				if _, err := Generate(context.Background(), server.Client(), []string{server.URL + "/ok", server.URL + "/bad"}, output, ipv6); err == nil {
					t.Fatal("refresh accepted failure")
				}
				got, err := os.ReadFile(output)
				if err != nil || string(got) != string(prior) {
					t.Fatalf("prior changed: %q %v", got, err)
				}
			})
		}
	}
}

func TestSuccessfulRefreshSortsAndRecordsProvenance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := "2.0.0.0"
		if r.URL.Path == "/second" {
			ip = "1.0.0.0"
		}
		w.Write([]byte(header + "test|US|ipv4|" + ip + "|256|20261007|allocated\n"))
	}))
	defer server.Close()
	output := filepath.Join(t.TempDir(), "table.bin")
	report, err := Generate(context.Background(), server.Client(), []string{server.URL + "/first", server.URL + "/second"}, output, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 20 || binary.BigEndian.Uint32(data) != 0x01000000 || binary.BigEndian.Uint32(data[10:]) != 0x02000000 {
		t.Fatalf("unexpected records: %x", data)
	}
	if report.Entries != 2 || len(report.Sources) != 2 || len(report.SHA256) != 64 || len(report.Sources[0].SHA256) != 64 || report.Sources[0].Header != strings.TrimSpace(header) {
		t.Fatalf("missing provenance: %+v", report)
	}
}

func TestInvalidCountryRanges(t *testing.T) {
	for _, c := range []struct {
		row  string
		ipv6 bool
	}{
		{"test|US|ipv4|255.255.255.255|2|20261007|allocated", false},
		{"test|US|ipv4|1.0.0.0|0|20261007|allocated", false},
		{"test|US|ipv4|bad|1|20261007|allocated", false},
		{"test|US|ipv6|2001:db8::|65|20261007|allocated", true},
		{"test|US|ipv6|2001:db8::1|32|20261007|allocated", true},
		{"test|US|ipv6|1.0.0.0|32|20261007|allocated", true},
	} {
		if _, _, err := parse(header+c.row+"\n", c.ipv6); err == nil {
			t.Fatalf("accepted %s", c.row)
		}
	}
}

func TestOutputFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("new")); err == nil {
		t.Fatal("rename over directory accepted")
	}
	files, _ := os.ReadDir(filepath.Dir(path))
	if len(files) != 1 {
		t.Fatalf("temporary output leaked: %v", files)
	}
}

func TestRegistryVersionAndUnallocatedRows(t *testing.T) {
	data := "2.3|test|20261007|2|20261007|20261007|+0000\n" +
		"test|*|ipv4|*|2|summary\n" +
		"test|*|ipv4|0.0.0.0|256|20261007|reserved\n" +
		"test|US|ipv4|1.0.0.0|256|20261007|allocated\n"
	rows, _, err := parse(data, false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("registry: %v %+v", err, rows)
	}
}

func TestFamilySummaryCompleteness(t *testing.T) {
	data := "2|test|20261007|99|20261007|20261007|+0000\n" +
		"test|*|asn|*|98|summary\n" +
		"test|*|ipv4|*|1|summary\n" +
		"test|US|ipv4|1.0.0.0|256|20261007|allocated\n"
	if _, _, err := parse(data, false); err != nil {
		t.Fatalf("complete IPv4 with stale unrelated ASN summary: %v", err)
	}
	data = strings.Replace(data, "|ipv4|*|1|summary", "|ipv4|*|2|summary", 1)
	if _, _, err := parse(data, false); err == nil {
		t.Fatal("truncated IPv4 family accepted")
	}
}
