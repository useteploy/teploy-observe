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
