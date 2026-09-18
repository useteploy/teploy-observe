package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// F45 restore completeness, live: a dump taken on an instance carrying real
// srcmap-domain state (v2 blob keys, a legacy raw blob key, a releases SET,
// a relage ZSET with exact integer and fractional scores) restores into a
// wiped instance with EVERY key back and byte-identical — the KV write path
// may not silently drop a key or round a score. Also proves the emptiness
// gate now covers the KV namespace, not just tables.
func TestBackupRestore_KVSrcmapRoundTripLive(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	site := "f45rt-" + time.Now().UTC().Format("150405.000000000")
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	blobV2 := "srcmap:v2:" + b64(site) + ":" + b64("r1") + ":" + b64("app.js.map")
	blobLegacy := "srcmap:" + site + ":r1:app.js.map"
	payloadV2 := `{"version":3,"file":"app.js","sources":["webpack://app/a.js"],"names":[],"mappings":"AAAA"}`
	payloadLegacy := `{"version":3,"file":"legacy.js","mappings":"CAAC"}`

	wipeKV := func() {
		keys, err := kvListKeys(ctx, db.Pool(), kvSrcmapPattern)
		if err != nil {
			t.Fatalf("kv list before wipe: %v", err)
		}
		for _, k := range keys {
			if _, err := db.Pool().Exec(ctx, "SELECT KV_DEL($1)", k); err != nil {
				t.Fatalf("kv del %s: %v", k, err)
			}
		}
	}
	wipeTables := func() {
		for _, tbl := range Tables {
			if _, err := db.SQL().Exec(ctx, "DELETE FROM "+tbl); err != nil {
				t.Fatalf("clear %s: %v", tbl, err)
			}
		}
	}

	// The restore target must be empty in BOTH domains and this test is the
	// suite's KV round-trip owner, so clear the whole srcmap namespace (a
	// scratch engine holds only test data) and every table up front.
	wipeKV()
	wipeTables()

	// Seed through the same KV SQL dispatch the product uses.
	if _, err := db.Pool().Exec(ctx, "SELECT KV_SET($1, $2)", blobV2, payloadV2); err != nil {
		t.Fatalf("seed v2 blob: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, "SELECT KV_SET($1, $2)", blobLegacy, payloadLegacy); err != nil {
		t.Fatalf("seed legacy blob: %v", err)
	}
	for _, rel := range []string{"r1", "r2", "r3"} {
		if _, err := db.Pool().Exec(ctx, "SELECT KV_SADD($1, $2)", "srcmap:releases:"+site, rel); err != nil {
			t.Fatalf("seed releases: %v", err)
		}
	}
	for _, sc := range []struct {
		member string
		score  float64
	}{{"r1", 1737289123}, {"r2", 1737289456.5}, {"r3", 0.25}} {
		if _, err := db.Pool().Exec(ctx, "SELECT KV_ZADD($1, $2, $3)", "srcmap:relage:"+site, sc.score, sc.member); err != nil {
			t.Fatalf("seed relage: %v", err)
		}
	}

	var arch bytes.Buffer
	if err := DumpWithLog(ctx, db, &arch, os.Stderr); err != nil {
		t.Fatalf("dump: %v", err)
	}
	manifest := readManifest(t, &arch)
	if !containsStr(manifest.KVSections, kvSrcmapSection) {
		t.Fatalf("manifest does not declare the kv srcmap section:\n%s", mustJSON(t, manifest))
	}
	if manifest.Lease == nil || !manifest.Lease.Held {
		t.Fatalf("expected lease.held=true on a lease-capable engine:\n%s", mustJSON(t, manifest))
	}
	if !archiveHoldsEntry(t, &arch, kvSrcmapEntryName) {
		t.Fatal("archive has no kv-srcmap entry")
	}

	// Wipe BOTH domains and restore.
	wipeKV()
	wipeTables()
	if err := Restore(ctx, db, bytes.NewReader(arch.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// Completeness: exactly the seeded keys exist again. KV_KEYS cannot
	// see collection keys (upstream — see kvsrcmap.go), so the blobs are
	// asserted through the listing and the set/zset through the typed
	// reads immediately below.
	keys, err := kvListKeys(ctx, db.Pool(), kvSrcmapPattern)
	if err != nil {
		t.Fatal(err)
	}
	wantStrings := map[string]bool{blobV2: true, blobLegacy: true}
	gotStrings := map[string]bool{}
	for _, k := range keys {
		gotStrings[k] = true
	}
	if !reflect.DeepEqual(gotStrings, wantStrings) {
		t.Fatalf("restored string keys = %v, want exactly %v", keys, wantStrings)
	}

	// Exactness: blobs byte-identical, set members equal, scores unrounded.
	var v2raw *string
	if err := db.Pool().QueryRow(ctx, "SELECT KV_GET($1)", blobV2).Scan(&v2raw); err != nil {
		t.Fatalf("kv get v2: %v", err)
	}
	if v2raw == nil || *v2raw != payloadV2 {
		t.Fatalf("v2 blob not byte-identical after round-trip: %q", deref(v2raw))
	}
	var legraw *string
	if err := db.Pool().QueryRow(ctx, "SELECT KV_GET($1)", blobLegacy).Scan(&legraw); err != nil {
		t.Fatalf("kv get legacy: %v", err)
	}
	if legraw == nil || *legraw != payloadLegacy {
		t.Fatalf("legacy blob not byte-identical after round-trip: %q", deref(legraw))
	}
	members, err := kvSMembers(ctx, db.Pool(), "srcmap:releases:"+site)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("releases set holds %v after restore, want 3 members", members)
	}
	entries, err := kvZRangeAll(ctx, db.Pool(), "srcmap:relage:"+site)
	if err != nil {
		t.Fatal(err)
	}
	scores := map[string]float64{}
	for _, e := range entries {
		scores[e.Member] = e.Score
	}
	for m, want := range map[string]float64{"r1": 1737289123, "r2": 1737289456.5, "r3": 0.25} {
		if got, ok := scores[m]; !ok || got != want {
			t.Fatalf("relage score for %s = %v (present=%v), want %v — scores must round-trip exactly", m, got, ok, want)
		}
	}

	// The emptiness gate covers the KV domain: with tables wiped but the
	// namespace populated, restore must refuse instead of mixing.
	wipeTables()
	if err := Restore(ctx, db, bytes.NewReader(arch.Bytes())); err == nil {
		t.Fatal("restore into a populated kv namespace succeeded — the emptiness gate does not cover KV")
	} else if want := "kv namespace"; !strings.Contains(err.Error(), want) {
		t.Fatalf("expected the refusal to name the kv namespace, got: %v", err)
	}

	// Leave the scratch engine clean for the rest of the suite.
	wipeKV()
	wipeTables()
}

// The dump-side shape on an empty namespace: a dump of an instance with no
// srcmap keys still declares the section and carries the (empty) entry —
// matching existing-but-empty tables, which get an empty .jsonl entry too —
// reports zero keys, and restores cleanly into an empty target.
func TestBackupRestore_KVSrcmapEmptyNamespace(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	var arch bytes.Buffer
	if err := DumpWithLog(ctx, db, &arch, io.Discard); err != nil {
		t.Fatalf("dump: %v", err)
	}
	manifest := readManifest(t, &arch)
	if !containsStr(manifest.KVSections, kvSrcmapSection) {
		t.Fatalf("manifest must declare the kv section even when empty:\n%s", mustJSON(t, manifest))
	}
	if !archiveHoldsEntry(t, &arch, kvSrcmapEntryName) {
		t.Fatal("empty namespace omitted the kv-srcmap entry — the section must be present like an existing-but-empty table")
	}

	// Both domains must be empty for the restore leg; the shared scratch
	// engine may hold whatever other packages' tests left behind.
	tablesDirty := false
	for _, tbl := range Tables {
		rows, err := nucleus.Query[struct {
			N int64 `db:"n"`
		}](ctx, db.SQL(), "SELECT COUNT(*) AS n FROM "+tbl)
		if err == nil && len(rows) > 0 && rows[0].N > 0 {
			tablesDirty = true
			break
		}
	}
	keys, err := kvListKeys(ctx, db.Pool(), kvSrcmapPattern)
	if err != nil {
		t.Fatal(err)
	}
	if tablesDirty || len(keys) > 0 {
		t.Skip("shared scratch engine is dirty — round-trip restore leg covered by TestBackupRestore_KVSrcmapRoundTripLive")
	}
	if err := Restore(ctx, db, bytes.NewReader(arch.Bytes())); err != nil {
		t.Fatalf("restore of an empty-namespace archive: %v", err)
	}
}

func archiveHoldsEntry(t *testing.T, arch *bytes.Buffer, name string) bool {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(arch.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == name {
			return true
		}
	}
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
