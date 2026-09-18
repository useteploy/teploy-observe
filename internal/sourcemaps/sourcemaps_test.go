package sourcemaps

import "testing"

// TestDecodeMappings_CumulativeLines is the regression for the VLQ decoder bug:
// source line/column deltas accumulate across all preceding mapping lines, so a
// frame on the second line must reflect the running total, not a per-line reset.
//
// "AAEA;AACA": line 1 segment [0,0,2,0] -> original line 3 (srcLine 2 + 1);
// line 2 segment [0,0,1,0] -> original line 4 (srcLine 2+1 + 1). The old
// in-isolation decoder would wrongly report original line 2 for line 2.
func TestDecodeMappings_CumulativeLines(t *testing.T) {
	sources := []string{"app.ts"}
	mappings := "AAEA;AACA"

	m1 := decodeMappings(mappings, sources, nil, 1, 1)
	if m1 == nil || m1.OriginalLine != 3 {
		t.Fatalf("line 1: want original line 3, got %+v", m1)
	}

	m2 := decodeMappings(mappings, sources, nil, 2, 1)
	if m2 == nil || m2.OriginalLine != 4 {
		t.Fatalf("line 2: want original line 4 (cumulative), got %+v", m2)
	}
	if m2.OriginalFile != "app.ts" {
		t.Fatalf("line 2: want source app.ts, got %q", m2.OriginalFile)
	}
}

// TestDecodeMappings_Names verifies the original name is resolved from the names
// table when a segment carries a name index.
func TestDecodeMappings_Names(t *testing.T) {
	sources := []string{"app.ts"}
	names := []string{"handleClick"}
	// Single segment [0,0,0,0,0]: genCol 0, source 0, line 0, col 0, name 0.
	// "AAAAA" decodes to five zeros.
	m := decodeMappings("AAAAA", sources, names, 1, 1)
	if m == nil {
		t.Fatal("expected a mapping")
	}
	if m.OriginalName != "handleClick" {
		t.Fatalf("want original name handleClick, got %q", m.OriginalName)
	}
}

// TestDecodeMappings_OutOfRange returns nil for lines past the mappings.
func TestDecodeMappings_OutOfRange(t *testing.T) {
	if m := decodeMappings("AAAA", nil, nil, 5, 1); m != nil {
		t.Fatalf("expected nil for out-of-range line, got %+v", m)
	}
}

func TestKeepReleasesDefaultsAndOverride(t *testing.T) {
	t.Setenv("OBSERVE_SOURCEMAP_KEEP_RELEASES", "")
	if got := KeepReleases(); got != DefaultKeepReleases {
		t.Fatalf("expected default %d, got %d", DefaultKeepReleases, got)
	}
	t.Setenv("OBSERVE_SOURCEMAP_KEEP_RELEASES", "3")
	if got := KeepReleases(); got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
	// A nonsense value must not silently disable retention.
	for _, bad := range []string{"0", "-5", "abc"} {
		t.Setenv("OBSERVE_SOURCEMAP_KEEP_RELEASES", bad)
		if got := KeepReleases(); got != DefaultKeepReleases {
			t.Fatalf("%q should fall back to the default, got %d", bad, got)
		}
	}
}

// Release names come from the upload request, so one can be a prefix of
// another. Pruning "v1" must not delete the retained "v1:beta" as a side effect
// of matching by key prefix.
func TestPruneSkipsKeysOfLongerRetainedRelease(t *testing.T) {
	site := "site-1"
	retained := []string{"v1:beta", "v2"}

	victim := kvKey(site, "v1", "app.js")
	if belongsToOther(victim, site, "v1", retained) {
		t.Fatalf("%q belongs to v1 and must be deletable", victim)
	}

	protected := kvKey(site, "v1:beta", "app.js")
	if !belongsToOther(protected, site, "v1", retained) {
		t.Fatalf("%q belongs to retained v1:beta and must be skipped", protected)
	}
}

func TestReleaseKeysAreDistinctPerRelease(t *testing.T) {
	a := kvKey("s", "v1", "app.js")
	b := kvKey("s", "v2", "app.js")
	if a == b {
		t.Fatal("different releases must not share a key")
	}
	if releaseAgeKey("s") == releasesSetKey("s") {
		t.Fatal("age index must not collide with the releases set")
	}
}

// AUD-047 (round 2): the selected mapping must COVER the requested column
// (greatest generated column <= target), never a future one to the right.
// "AAAA,EAAA" on line 1: segments at generated columns 0 and 4 (VLQ 'E'=-1?
// no: A=0, C=1 -> "AACA" would be col +1). Build explicit vectors instead.
func TestDecodeMappings_SelectsCoveringSegment(t *testing.T) {
	sources := []string{"app.ts"}
	// Segments on line 1: genCol 0 -> src line 1; genCol 10 -> src line 2.
	// VLQ: col delta 10 encodes as 'U'; line delta 1 as 'C'.
	mappings := "AAAA,UACA"
	// Column 8 (1-based) = zero-based 7: must select the column-0 segment.
	m := decodeMappings(mappings, sources, nil, 1, 8)
	if m == nil || m.OriginalLine != 1 {
		t.Fatalf("col 8 must map through the covering segment at gen col 0: %+v", m)
	}
	// Column 11 (1-based) = zero-based 10: selects the column-10 segment.
	m = decodeMappings(mappings, sources, nil, 1, 11)
	if m == nil || m.OriginalLine != 2 {
		t.Fatalf("col 11 must select the gen col 10 segment: %+v", m)
	}
	// Column 1 (zero-based 0): the first segment.
	m = decodeMappings(mappings, sources, nil, 1, 1)
	if m == nil || m.GeneratedColumn != 1 {
		t.Fatalf("col 1 must select the first segment: %+v", m)
	}
}

// AUD-047: a generated-column-only segment explicitly marks its region
// unmapped — requests inside it return no mapping.
func TestDecodeMappings_GeneratedOnlySegmentUnmaps(t *testing.T) {
	sources := []string{"app.ts"}
	// Line 1: mapped at genCol 0, unmapped marker at genCol 5 ('K' = 5),
	// mapped again at genCol 10 ('KACA' = [5, 0, 1, 0] — deltas accumulate).
	mappings := "AAAA,K,KACA"
	for col := 6; col <= 10; col++ {
		if m := decodeMappings(mappings, sources, nil, 1, col); m != nil {
			t.Fatalf("col %d lies in the explicitly unmapped region, got %+v", col, m)
		}
	}
	if m := decodeMappings(mappings, sources, nil, 1, 11); m == nil {
		t.Fatal("col 11 is past the remapped segment at gen col 10 and must map")
	}
	if m := decodeMappings(mappings, sources, nil, 1, 4); m == nil {
		t.Fatal("col 4 is still covered by the first mapped segment")
	}
}

// AUD-047: an invalid source index resolves to unmapped, not a nominal
// mapping with an empty filename.
func TestDecodeMappings_InvalidSourceIndexIsUnmapped(t *testing.T) {
	// Segment [0,1,0,0]: source index 1 with a one-entry sources table.
	m := decodeMappings("ACAA", []string{"only.ts"}, nil, 1, 1)
	if m != nil {
		t.Fatalf("invalid source index must not produce a mapping, got %+v", m)
	}
}

// AUD-046 (round 2): the v2 key encoding is injective across components —
// (a:b, c) and (a, b:c) must not collide, unlike the legacy raw key.
func TestKVKeyV2_NoComponentCollisions(t *testing.T) {
	if kvKeyV2("s", "a:b", "c") == kvKeyV2("s", "a", "b:c") {
		t.Fatal("(a:b,c) and (a,b:c) must occupy distinct v2 keys")
	}
	if kvKeyV2("s", "v1*", "app.js") == kvKeyV2("s", "v1?", "app.js") {
		t.Fatal("glob metacharacters must not collide in v2 keys")
	}
	if kvKeyV2("s", "", "x") == kvKeyV2("s", "x", "") {
		t.Fatal("empty components must not collide in v2 keys")
	}
	// Legacy proves the ambiguity the v2 scheme removes.
	if kvKey("s", "a:b", "c") != kvKey("s", "a", "b:c") {
		t.Fatal("legacy key precondition: these two DO collide raw")
	}
}

// AUD-046: membership tests recognize both encodings and reject strangers.
func TestKeyBelongsToRelease_BothEncodings(t *testing.T) {
	if !keyBelongsToRelease(kvKeyV2("s", "v1", "app.js"), "s", "v1") {
		t.Fatal("v2 key of the release must be matched")
	}
	if !keyBelongsToRelease(kvKey("s", "v1", "app.js"), "s", "v1") {
		t.Fatal("legacy key of the release must be matched")
	}
	if keyBelongsToRelease(kvKeyV2("s", "v2", "app.js"), "s", "v1") {
		t.Fatal("another release's key must not match")
	}
	if keyBelongsToRelease(kvKey("s", "av1", "app.js"), "s", "v1") {
		t.Fatal("release name embedded in another release must not match")
	}
	if keyBelongsToRelease("srcmap:releases:s", "s", "v1") {
		t.Fatal("index keys are not blob keys")
	}
}
