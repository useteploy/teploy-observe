package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func kvTestArchive(t *testing.T, manifest Manifest, kvLines []string, results []TableResult) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, raw []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(raw)), ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if manifest.Tables == nil {
		manifest.Tables = Tables
	}
	mraw, _ := json.Marshal(manifest)
	write(manifestName, mraw)
	if kvLines != nil {
		write(kvSrcmapEntryName, []byte(strings.Join(kvLines, "\n")+"\n"))
	}
	if results == nil {
		results = make([]TableResult, 0, len(Tables)+1)
		for _, tbl := range Tables {
			results = append(results, TableResult{Table: tbl, Rows: 0, OK: true})
		}
		results = append(results, TableResult{Table: kvSrcmapResultTable, Rows: int64(len(kvLines)), OK: true})
	}
	rraw, _ := json.Marshal(results)
	write(resultsName, rraw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func mustReject(t *testing.T, buf *bytes.Buffer, marker string) {
	t.Helper()
	err := Restore(context.Background(), nil, buf)
	if err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("expected rejection mentioning %q, got %v", marker, err)
	}
}

func TestRestore_RejectsKVSectionNotDeclared(t *testing.T) {
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: nil},
		[]string{`{"key":"srcmap:v2:a:b:c","type":"string","value":"` + base64.StdEncoding.EncodeToString([]byte("{}")) + `"}`},
		nil)
	mustReject(t, buf, "does not declare")
}

func TestRestore_RejectsKVSectionMissingEntry(t *testing.T) {
	results := make([]TableResult, 0, len(Tables)+1)
	for _, tbl := range Tables {
		results = append(results, TableResult{Table: tbl, Rows: 0, OK: true})
	}
	results = append(results, TableResult{Table: kvSrcmapResultTable, Rows: 2, OK: true})
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{kvSrcmapSection}},
		nil,
		results)
	mustReject(t, buf, "missing from the archive (completion record says 2 keys)")
}

func TestRestore_RejectsKVRowCountMismatch(t *testing.T) {
	defaultResults := func(kvRows int64) []TableResult {
		results := make([]TableResult, 0, len(Tables)+1)
		for _, tbl := range Tables {
			results = append(results, TableResult{Table: tbl, Rows: 0, OK: true})
		}
		return append(results, TableResult{Table: kvSrcmapResultTable, Rows: kvRows, OK: true})
	}
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{kvSrcmapSection}},
		[]string{`{"key":"srcmap:a:r:f","type":"set","members":["r1"]}`},
		defaultResults(7))
	mustReject(t, buf, "completion record says 7 keys")
}

func TestRestore_RejectsKVRowOutsideNamespace(t *testing.T) {
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{kvSrcmapSection}},
		[]string{`{"key":"other:ns","type":"set","members":[]}`},
		nil)
	mustReject(t, buf, "outside the srcmap namespace")
}

func TestRestore_RejectsKVBadBase64(t *testing.T) {
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{kvSrcmapSection}},
		[]string{`{"key":"srcmap:a:r:f","type":"string","value":"!!!not-base64!!!"}`},
		nil)
	mustReject(t, buf, "base64")
}

func TestRestore_RejectsKVUnknownType(t *testing.T) {
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{kvSrcmapSection}},
		[]string{`{"key":"srcmap:a:r:f","type":"list","members":[]}`},
		nil)
	mustReject(t, buf, "unknown type")
}

func TestRestore_RejectsKVDuplicateKeys(t *testing.T) {
	line := `{"key":"srcmap:a:r:f","type":"set","members":["x"]}`
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{kvSrcmapSection}},
		[]string{line, line},
		nil)
	mustReject(t, buf, "duplicate key")
}

func TestRestore_RejectsUnknownKVSection(t *testing.T) {
	buf := kvTestArchive(t,
		Manifest{Version: manifestVersion, CreatedAt: time.Now(), KVSections: []string{"other"}},
		nil,
		nil)
	mustReject(t, buf, "unknown kv section")
}

func TestDecodeKVRow_PreservesExactScores(t *testing.T) {
	line := `{"key":"srcmap:relage:s1","type":"zset","entries":[{"member":"r1","score":1737289123},{"member":"r2","score":1e9}]}`
	row, err := decodeKVRow([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if row.Entries[0].Score != 1737289123 || row.Entries[1].Score != 1e9 {
		t.Fatalf("scores not preserved exactly: %+v", row.Entries)
	}
}

func TestKVSrcmapType(t *testing.T) {
	cases := map[string]string{
		"srcmap:v2:c2l0ZQ:cjE:ZmlsZS5qcw": "string",
		"srcmap:site1:rel1:app.js.map":    "string",
		"srcmap:releases:site1":           "set",
		"srcmap:relage:site1":             "zset",
	}
	for key, want := range cases {
		if got := kvSrcmapType(key); got != want {
			t.Errorf("kvSrcmapType(%q) = %q, want %q", key, got, want)
		}
	}
}

// The dump reconstructs set/zset keys from the site ids derivable in blob
// keys (KV_KEYS cannot list collection keys — upstream). Both key shapes
// must yield their site id, reserved prefixes must never be read as sites,
// and a v2 component that is not valid base64url must be skipped rather
// than mangled.
func TestSrcmapSites(t *testing.T) {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	keys := []string{
		"srcmap:v2:" + b64("site-a") + ":" + b64("r1") + ":" + b64("app.js.map"),
		"srcmap:v2:" + b64("site-b") + ":" + b64("r:1") + ":" + b64("x:y.map"), // colons inside components
		"srcmap:site-c:r1:app.js.map",
		"srcmap:site-c:r2:legacy.map", // duplicate site collapses
		"srcmap:v2:!!!not-b64!!!:cjE:Zg",
	}
	got := srcmapSites(keys)
	want := []string{"site-a", "site-b", "site-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("srcmapSites = %v, want %v", got, want)
	}

	reserved := srcmapSites([]string{
		"srcmap:releases:r1:fake.map", // a set key shape misread as a blob key
		"srcmap:relage:r1:fake.map",
	})
	if len(reserved) != 0 {
		t.Fatalf("reserved prefixes must not be derived as sites, got %v", reserved)
	}
}
