package heatmaps

import (
	"sort"
	"testing"
)

// TestAggregate_Buckets covers the pure aggregation: given a slice of click
// events, the aggregator must group them into the (x_bucket, y_bucket,
// vw_bucket) keys defined in 015_click_heatmaps.up.sql.
func TestAggregate_Buckets(t *testing.T) {
	tests := []struct {
		name   string
		events []ClickEvent
		// expect maps a bucketKey shorthand to its expected count.
		// We compare via the same `aggregate` function the production code
		// uses so a refactor that changes bucket sizes will surface here.
		expect map[bucketKey]int64
	}{
		{
			name:   "empty input returns empty map",
			events: nil,
			expect: map[bucketKey]int64{},
		},
		{
			name: "two clicks in the same 10x10 bucket collapse",
			events: []ClickEvent{
				{X: 12, Y: 7, ViewportWidth: 1280},
				{X: 19, Y: 5, ViewportWidth: 1280},
			},
			// bucket = (1, 0), vw = 12
			expect: map[bucketKey]int64{
				{URL: "/p", X: 1, Y: 0, VW: 12}: 2,
			},
		},
		{
			name: "clicks across bucket boundary land in distinct buckets",
			events: []ClickEvent{
				{X: 19, Y: 9, ViewportWidth: 1280}, // bucket (1, 0)
				{X: 20, Y: 10, ViewportWidth: 1280}, // bucket (2, 1)
			},
			expect: map[bucketKey]int64{
				{URL: "/p", X: 1, Y: 0, VW: 12}: 1,
				{URL: "/p", X: 2, Y: 1, VW: 12}: 1,
			},
		},
		{
			name: "different viewport widths split into different vw buckets",
			events: []ClickEvent{
				{X: 100, Y: 100, ViewportWidth: 1280}, // vw bucket 12
				{X: 100, Y: 100, ViewportWidth: 1920}, // vw bucket 19
			},
			expect: map[bucketKey]int64{
				{URL: "/p", X: 10, Y: 10, VW: 12}: 1,
				{URL: "/p", X: 10, Y: 10, VW: 19}: 1,
			},
		},
		{
			name: "negative coords clamp to 0",
			events: []ClickEvent{
				{X: -5, Y: -8, ViewportWidth: 0},
			},
			expect: map[bucketKey]int64{
				{URL: "/p", X: 0, Y: 0, VW: 0}: 1,
			},
		},
		{
			name: "zero viewport falls into vw bucket 0",
			events: []ClickEvent{
				{X: 5, Y: 5, ViewportWidth: 0},
				{X: 5, Y: 5, ViewportWidth: 99}, // still bucket 0 (99/100=0)
			},
			expect: map[bucketKey]int64{
				{URL: "/p", X: 0, Y: 0, VW: 0}: 2,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregate("/p", tt.events)
			if len(got) != len(tt.expect) {
				t.Fatalf("bucket count = %d, want %d (got=%v)", len(got), len(tt.expect), got)
			}
			for k, v := range tt.expect {
				if got[k] != v {
					t.Errorf("bucket %+v: got %d, want %d", k, got[k], v)
				}
			}
		})
	}
}

// TestExtractClicks confirms the defensive parser pulls x/y out of the
// shapes the replay SDK can send (typed map[string]any, JSON string,
// json.Number) and silently drops events that don't carry both fields.
func TestExtractClicks(t *testing.T) {
	events := []RawEvent{
		{Type: "click", Data: map[string]any{"x": float64(100), "y": float64(50)}, ViewportWidth: 1280},
		{Type: "click", Data: map[string]any{"x": "200", "y": "75"}, ViewportWidth: 1280},
		{Type: "click", Data: map[string]any{"x": float64(300)}, ViewportWidth: 1280},   // missing y
		{Type: "mousemove", Data: map[string]any{"x": float64(0), "y": float64(0)}},     // wrong type
		{Type: "click", Data: "not even a map"},                                          // garbage
	}

	clicks := ExtractClicks(events)
	if len(clicks) != 2 {
		t.Fatalf("clicks = %d, want 2 (got %v)", len(clicks), clicks)
	}
	// Stable order makes the assertion easier to read; ExtractClicks
	// preserves input order but sort defensively.
	sort.Slice(clicks, func(i, j int) bool { return clicks[i].X < clicks[j].X })

	if clicks[0].X != 100 || clicks[0].Y != 50 || clicks[0].ViewportWidth != 1280 {
		t.Errorf("clicks[0] = %+v", clicks[0])
	}
	if clicks[1].X != 200 || clicks[1].Y != 75 || clicks[1].ViewportWidth != 1280 {
		t.Errorf("clicks[1] = %+v", clicks[1])
	}
}

// TestAggregateScales feeds 1000 clicks into the aggregator and confirms
// the bucket map collapses them to a sane number of cells (sanity check
// that the bucket math doesn't accidentally explode rows).
func TestAggregateScales(t *testing.T) {
	events := make([]ClickEvent, 0, 1000)
	for i := 0; i < 1000; i++ {
		events = append(events, ClickEvent{
			X:             i % 200,         // cycle 200 px wide
			Y:             (i * 7) % 200,    // cycle 200 px tall
			ViewportWidth: 1280,
		})
	}
	got := aggregate("/p", events)
	// 200/10 = 20 wide × 200/10 = 20 tall → upper bound 400 cells.
	if len(got) > 400 {
		t.Errorf("bucket count = %d, want <= 400", len(got))
	}
	// Total count across all buckets must equal input.
	var total int64
	for _, c := range got {
		total += c
	}
	if total != 1000 {
		t.Errorf("total count = %d, want 1000", total)
	}
}
