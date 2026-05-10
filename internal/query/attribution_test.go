package query

import (
	"math"
	"testing"
)

// approxEq compares floats with a small epsilon — linear attribution
// produces 1/3 etc which can't be matched bit-for-bit.
func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// findRow returns the row for the given source, or a zero AttributionRow
// (with Source="") if not present. Lets tests assert "must be missing"
// by checking row.Source == "".
func findRow(rows []AttributionRow, source string) AttributionRow {
	for _, r := range rows {
		if r.Source == source {
			return r
		}
	}
	return AttributionRow{}
}

// Synthetic three-session dataset used by every model test below:
//
//	S1: google -> twitter           (two distinct sources)
//	S2: facebook                    (single source)
//	S3: (no utm anywhere)           (counts as direct)
//
// Session 1 spans two utm_source values across separate events so we
// can exercise first/last/linear cleanly.
func threeSessionFixture() []attributionEvent {
	return []attributionEvent{
		// S1 — google first, twitter last
		{SessionID: "s1", UTMSource: "google", Timestamp: 100},
		{SessionID: "s1", UTMSource: "twitter", Timestamp: 200},
		// S2 — facebook only
		{SessionID: "s2", UTMSource: "facebook", Timestamp: 150},
		// S3 — no utm; should bucket as (direct)
		{SessionID: "s3", UTMSource: "", Timestamp: 300},
	}
}

func TestComputeAttribution_FirstTouch(t *testing.T) {
	rows := ComputeAttribution(threeSessionFixture(), AttributionFirstTouch)

	// google should get S1 (first touch), facebook S2, direct S3.
	// twitter should NOT appear — it was never the first touch.
	google := findRow(rows, "google")
	facebook := findRow(rows, "facebook")
	direct := findRow(rows, directBucket)
	twitter := findRow(rows, "twitter")

	if !approxEq(google.Sessions, 1.0) {
		t.Errorf("first-touch: google sessions = %v, want 1.0", google.Sessions)
	}
	if !approxEq(facebook.Sessions, 1.0) {
		t.Errorf("first-touch: facebook sessions = %v, want 1.0", facebook.Sessions)
	}
	if !approxEq(direct.Sessions, 1.0) {
		t.Errorf("first-touch: direct sessions = %v, want 1.0", direct.Sessions)
	}
	if twitter.Source != "" {
		t.Errorf("first-touch: twitter should not appear (was never first), got %+v", twitter)
	}
}

func TestComputeAttribution_LastTouch(t *testing.T) {
	rows := ComputeAttribution(threeSessionFixture(), AttributionLastTouch)

	// twitter should now get S1 (last touch), facebook S2, direct S3.
	// google should NOT appear — it was never the last non-empty source.
	twitter := findRow(rows, "twitter")
	facebook := findRow(rows, "facebook")
	direct := findRow(rows, directBucket)
	google := findRow(rows, "google")

	if !approxEq(twitter.Sessions, 1.0) {
		t.Errorf("last-touch: twitter sessions = %v, want 1.0", twitter.Sessions)
	}
	if !approxEq(facebook.Sessions, 1.0) {
		t.Errorf("last-touch: facebook sessions = %v, want 1.0", facebook.Sessions)
	}
	if !approxEq(direct.Sessions, 1.0) {
		t.Errorf("last-touch: direct sessions = %v, want 1.0", direct.Sessions)
	}
	if google.Source != "" {
		t.Errorf("last-touch: google should not appear (was never last), got %+v", google)
	}
}

// TestComputeAttribution_Linear_Fractional explicitly covers the
// fractional credit case — S1 has two sources so each gets 0.5.
func TestComputeAttribution_Linear_Fractional(t *testing.T) {
	rows := ComputeAttribution(threeSessionFixture(), AttributionLinear)

	google := findRow(rows, "google")
	twitter := findRow(rows, "twitter")
	facebook := findRow(rows, "facebook")
	direct := findRow(rows, directBucket)

	// S1 split 50/50 between google + twitter.
	if !approxEq(google.Sessions, 0.5) {
		t.Errorf("linear: google sessions = %v, want 0.5", google.Sessions)
	}
	if !approxEq(twitter.Sessions, 0.5) {
		t.Errorf("linear: twitter sessions = %v, want 0.5", twitter.Sessions)
	}
	// S2 had only facebook, so it gets full 1.0.
	if !approxEq(facebook.Sessions, 1.0) {
		t.Errorf("linear: facebook sessions = %v, want 1.0", facebook.Sessions)
	}
	// S3 was direct.
	if !approxEq(direct.Sessions, 1.0) {
		t.Errorf("linear: direct sessions = %v, want 1.0", direct.Sessions)
	}

	// Total credit must sum to total session count (3.0). This
	// invariant is what makes "linear attribution" coherent — we
	// don't lose or duplicate credit.
	var total float64
	for _, r := range rows {
		total += r.Sessions
	}
	if !approxEq(total, 3.0) {
		t.Errorf("linear: total credit across all sources = %v, want 3.0 (one per session)", total)
	}
}

// TestComputeAttribution_Linear_ThreeWaySplit covers a session with
// three distinct sources to make sure the 1/N math doesn't special-
// case N=2.
func TestComputeAttribution_Linear_ThreeWaySplit(t *testing.T) {
	events := []attributionEvent{
		{SessionID: "s", UTMSource: "google", Timestamp: 1},
		{SessionID: "s", UTMSource: "twitter", Timestamp: 2},
		{SessionID: "s", UTMSource: "facebook", Timestamp: 3},
	}
	rows := ComputeAttribution(events, AttributionLinear)

	for _, src := range []string{"google", "twitter", "facebook"} {
		got := findRow(rows, src).Sessions
		if !approxEq(got, 1.0/3.0) {
			t.Errorf("linear 3-way: %s sessions = %v, want %v", src, got, 1.0/3.0)
		}
	}
}

// TestComputeAttribution_Linear_RepeatedSourceCountsOnce verifies that
// a session that touches the SAME source three times credits it once
// (linear is on unique sources, not event count).
func TestComputeAttribution_Linear_RepeatedSourceCountsOnce(t *testing.T) {
	events := []attributionEvent{
		{SessionID: "s", UTMSource: "google", Timestamp: 1},
		{SessionID: "s", UTMSource: "google", Timestamp: 2},
		{SessionID: "s", UTMSource: "google", Timestamp: 3},
	}
	rows := ComputeAttribution(events, AttributionLinear)

	google := findRow(rows, "google")
	if !approxEq(google.Sessions, 1.0) {
		t.Errorf("linear repeated: google sessions = %v, want 1.0 (single unique source)", google.Sessions)
	}
}

// TestComputeAttribution_OutOfOrderInput verifies that sessions are
// re-sorted internally so callers can pass events in any order.
func TestComputeAttribution_OutOfOrderInput(t *testing.T) {
	// Same as fixture but reversed in time order — first/last must
	// still pick google/twitter respectively.
	events := []attributionEvent{
		{SessionID: "s1", UTMSource: "twitter", Timestamp: 200},
		{SessionID: "s1", UTMSource: "google", Timestamp: 100},
	}
	first := ComputeAttribution(events, AttributionFirstTouch)
	if g := findRow(first, "google").Sessions; !approxEq(g, 1.0) {
		t.Errorf("out-of-order first: google = %v, want 1.0", g)
	}
	if t2 := findRow(first, "twitter").Source; t2 != "" {
		t.Errorf("out-of-order first: twitter should be absent, got source=%q", t2)
	}

	last := ComputeAttribution(events, AttributionLastTouch)
	if tw := findRow(last, "twitter").Sessions; !approxEq(tw, 1.0) {
		t.Errorf("out-of-order last: twitter = %v, want 1.0", tw)
	}
}

// TestComputeAttribution_AllDirect verifies the direct bucket dominates
// when no session has any utm_source.
func TestComputeAttribution_AllDirect(t *testing.T) {
	events := []attributionEvent{
		{SessionID: "a", UTMSource: "", Timestamp: 1},
		{SessionID: "b", UTMSource: "", Timestamp: 2},
	}
	for _, model := range []string{AttributionFirstTouch, AttributionLastTouch, AttributionLinear} {
		rows := ComputeAttribution(events, model)
		if len(rows) != 1 || rows[0].Source != directBucket {
			t.Errorf("model=%s: want single (direct) row, got %+v", model, rows)
			continue
		}
		if !approxEq(rows[0].Sessions, 2.0) {
			t.Errorf("model=%s: direct sessions = %v, want 2.0", model, rows[0].Sessions)
		}
	}
}

func TestComputeAttribution_Empty(t *testing.T) {
	rows := ComputeAttribution(nil, AttributionLinear)
	if len(rows) != 0 {
		t.Errorf("empty input: want 0 rows, got %d", len(rows))
	}
}

// TestComputeAttribution_ConversionPct sanity-checks the percentage math.
// In v1 every session counts as a conversion so the rate is always 100%.
func TestComputeAttribution_ConversionPct(t *testing.T) {
	rows := ComputeAttribution(threeSessionFixture(), AttributionFirstTouch)
	for _, r := range rows {
		if !approxEq(r.ConversionPct, 100.0) {
			t.Errorf("v1 basic mode: %s conversion_pct = %v, want 100.0", r.Source, r.ConversionPct)
		}
	}
}

func TestIsValidModel(t *testing.T) {
	cases := map[string]bool{
		AttributionFirstTouch: true,
		AttributionLastTouch:  true,
		AttributionLinear:     true,
		"":                    false,
		"random":              false,
		"FIRST":               false, // case-sensitive
	}
	for in, want := range cases {
		if got := IsValidModel(in); got != want {
			t.Errorf("IsValidModel(%q) = %v, want %v", in, got, want)
		}
	}
}
