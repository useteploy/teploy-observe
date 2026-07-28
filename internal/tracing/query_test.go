package tracing

import (
	"math"
	"testing"
)

func TestApdex(t *testing.T) {
	const T int64 = 500 // satisfied <=500ms, tolerated <=2000ms, frustrated >2000ms

	tests := []struct {
		name      string
		durations []int64
		t         int64
		want      float64
	}{
		{
			name:      "empty input returns zero",
			durations: nil,
			t:         T,
			want:      0,
		},
		{
			name:      "all satisfied",
			durations: []int64{10, 100, 200, 499, 500},
			t:         T,
			want:      1.0,
		},
		{
			name:      "all frustrated",
			durations: []int64{2001, 5000, 10000},
			t:         T,
			want:      0.0,
		},
		{
			name:      "all tolerated",
			durations: []int64{501, 1000, 1500, 2000},
			t:         T,
			want:      0.5,
		},
		{
			name:      "mixed: 2 satisfied, 1 tolerated, 1 frustrated",
			durations: []int64{100, 400, 1500, 5000},
			t:         T,
			// (2 + 1/2) / 4 = 0.625
			want: 0.625,
		},
		{
			name:      "boundary at T is satisfied",
			durations: []int64{500},
			t:         T,
			want:      1.0,
		},
		{
			name:      "boundary at 4T is tolerated",
			durations: []int64{2000},
			t:         T,
			want:      0.5,
		},
		{
			name:      "just past 4T is frustrated",
			durations: []int64{2001},
			t:         T,
			want:      0.0,
		},
		{
			name:      "zero threshold returns zero",
			durations: []int64{1, 2, 3},
			t:         0,
			want:      0,
		},
		{
			name:      "negative threshold returns zero",
			durations: []int64{1, 2, 3},
			t:         -10,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apdex(tt.durations, tt.t)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("apdex(%v, %d) = %v, want %v", tt.durations, tt.t, got, tt.want)
			}
		})
	}
}

// TestApdexFromCounts_MatchesApdex pins the SQL-side bucketing to the original
// Go implementation: ListServices now has the database count satisfied and
// tolerated root spans instead of pulling every duration back, so the two must
// agree for the same input or the Traces page would quietly report a different
// score than it used to.
func TestApdexFromCounts_MatchesApdex(t *testing.T) {
	const T int64 = 500
	cases := [][]int64{
		{},
		{1},
		{500, 501, 2000, 2001},
		{100, 200, 300, 1500, 1900, 5000, 9000},
		{2001, 2001, 2001},
	}
	for _, durations := range cases {
		var satisfied, tolerated int64
		for _, d := range durations {
			switch {
			case d <= T:
				satisfied++
			case d <= 4*T:
				tolerated++
			}
		}
		want := apdex(durations, T)
		got := apdexFromCounts(satisfied, tolerated, int64(len(durations)))
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("apdexFromCounts(%d,%d,%d) = %v, apdex(%v) = %v",
				satisfied, tolerated, len(durations), got, durations, want)
		}
	}
}

// TestAggregateRollups_BucketingByMinute verifies spans are grouped into
// 60-second buckets keyed by start_time/60_000.
func TestAggregateRollups_BucketingByMinute(t *testing.T) {
	// Two spans 30s apart land in the same bucket; a third 90s later
	// lands in the next bucket.
	const base int64 = 1_700_000_040_000 // aligned to a 60s bucket boundary // arbitrary epoch ms aligned to a minute boundary
	spans := []flatSpan{
		{ServiceName: "api", OperationName: "GET /x", SpanID: "a", StartMs: base, EndMs: base + 10, DurationMs: 10},
		{ServiceName: "api", OperationName: "GET /x", SpanID: "b", StartMs: base + 30_000, EndMs: base + 30_020, DurationMs: 20},
		{ServiceName: "api", OperationName: "GET /x", SpanID: "c", StartMs: base + 90_000, EndMs: base + 90_005, DurationMs: 5},
	}
	services, _ := aggregateRollups(spans)

	if got := len(services); got != 2 {
		t.Fatalf("expected 2 service buckets, got %d (%v)", got, services)
	}
	bucket0 := ServiceBucket{Service: "api", Operation: "GET /x", Bucket: base}
	bucket1 := ServiceBucket{Service: "api", Operation: "GET /x", Bucket: base + 60_000}
	if a := services[bucket0]; a == nil || a.Count != 2 || a.DurationSum != 30 {
		t.Errorf("bucket0 = %+v, want count=2 durationSum=30", a)
	}
	if a := services[bucket1]; a == nil || a.Count != 1 || a.DurationSum != 5 {
		t.Errorf("bucket1 = %+v, want count=1 durationSum=5", a)
	}
}

// TestAggregateRollups_REDMetrics covers count + error + min/max bookkeeping.
func TestAggregateRollups_REDMetrics(t *testing.T) {
	const base int64 = 1_700_000_040_000 // aligned to a 60s bucket boundary
	spans := []flatSpan{
		{ServiceName: "api", OperationName: "POST /pay", SpanID: "1", StartMs: base, DurationMs: 50, StatusCode: "ok"},
		{ServiceName: "api", OperationName: "POST /pay", SpanID: "2", StartMs: base + 1_000, DurationMs: 200, StatusCode: "error"},
		{ServiceName: "api", OperationName: "POST /pay", SpanID: "3", StartMs: base + 2_000, DurationMs: 100, StatusCode: "ok"},
	}
	services, _ := aggregateRollups(spans)
	key := ServiceBucket{Service: "api", Operation: "POST /pay", Bucket: base}
	a := services[key]
	if a == nil {
		t.Fatalf("missing aggregate for %+v", key)
	}
	if a.Count != 3 || a.Errors != 1 {
		t.Errorf("count=%d errors=%d, want 3 / 1", a.Count, a.Errors)
	}
	if a.DurationSum != 350 {
		t.Errorf("durationSum=%d, want 350", a.DurationSum)
	}
	if a.DurationMin != 50 || a.DurationMax != 200 {
		t.Errorf("min=%d max=%d, want 50/200", a.DurationMin, a.DurationMax)
	}
}

// TestAggregateRollups_DependencyEdges verifies the parent-service ->
// child-service edge map is built from the in-batch span_id lookup.
func TestAggregateRollups_DependencyEdges(t *testing.T) {
	const base int64 = 1_700_000_040_000 // aligned to a 60s bucket boundary
	spans := []flatSpan{
		// Root in service "web"
		{ServiceName: "web", SpanID: "root", ParentSpanID: "", StartMs: base, DurationMs: 100},
		// Two child calls into "api"
		{ServiceName: "api", SpanID: "c1", ParentSpanID: "root", StartMs: base + 5, DurationMs: 50, StatusCode: "ok"},
		{ServiceName: "api", SpanID: "c2", ParentSpanID: "root", StartMs: base + 60, DurationMs: 30, StatusCode: "error"},
		// Same-service child should NOT create an edge
		{ServiceName: "web", SpanID: "c3", ParentSpanID: "root", StartMs: base + 90, DurationMs: 5, StatusCode: "ok"},
		// Grandchild in "worker" parented by api/c1
		{ServiceName: "worker", SpanID: "g1", ParentSpanID: "c1", StartMs: base + 10, DurationMs: 20, StatusCode: "ok"},
	}
	_, deps := aggregateRollups(spans)

	if got := len(deps); got != 2 {
		t.Fatalf("expected 2 edges (web->api, api->worker), got %d (%v)", got, deps)
	}

	webApi := ServiceEdge{Src: "web", Dst: "api", Bucket: base}
	if e := deps[webApi]; e == nil || e.CallCount != 2 || e.ErrorCount != 1 || e.DurationSum != 80 {
		t.Errorf("web->api edge = %+v, want callCount=2 errorCount=1 durationSum=80", e)
	}

	apiWorker := ServiceEdge{Src: "api", Dst: "worker", Bucket: base}
	if e := deps[apiWorker]; e == nil || e.CallCount != 1 || e.ErrorCount != 0 {
		t.Errorf("api->worker edge = %+v, want callCount=1 errorCount=0", e)
	}
}

// TestAggregateRollups_OrphanParentIgnored verifies that a child whose
// parent_span_id isn't in the batch is not silently bucketed under an
// empty src service.
func TestAggregateRollups_OrphanParentIgnored(t *testing.T) {
	const base int64 = 1_700_000_040_000 // aligned to a 60s bucket boundary
	spans := []flatSpan{
		{ServiceName: "api", SpanID: "c1", ParentSpanID: "missing-parent", StartMs: base, DurationMs: 10},
	}
	services, deps := aggregateRollups(spans)
	if len(services) != 1 {
		t.Errorf("services=%d, want 1", len(services))
	}
	if len(deps) != 0 {
		t.Errorf("deps=%d, want 0 (orphan parent should not produce an edge)", len(deps))
	}
}

// TestAggregateRollups_PercentilesSorted verifies the writeRollups path
// can compute percentiles from the Aggregate.Durations slice. The
// aggregator collects the raw durations; percentile(sorted, p) is
// applied at flush time.
func TestAggregateRollups_PercentilesSorted(t *testing.T) {
	const base int64 = 1_700_000_040_000 // aligned to a 60s bucket boundary
	spans := []flatSpan{
		{ServiceName: "api", OperationName: "GET /x", SpanID: "1", StartMs: base, DurationMs: 100},
		{ServiceName: "api", OperationName: "GET /x", SpanID: "2", StartMs: base, DurationMs: 200},
		{ServiceName: "api", OperationName: "GET /x", SpanID: "3", StartMs: base, DurationMs: 300},
		{ServiceName: "api", OperationName: "GET /x", SpanID: "4", StartMs: base, DurationMs: 400},
	}
	services, _ := aggregateRollups(spans)
	key := ServiceBucket{Service: "api", Operation: "GET /x", Bucket: base}
	a := services[key]
	if a == nil {
		t.Fatalf("missing aggregate")
	}
	// Sort in place the way writeRollups does.
	durs := append([]int64(nil), a.Durations...)
	for i := 0; i < len(durs); i++ {
		for j := i + 1; j < len(durs); j++ {
			if durs[j] < durs[i] {
				durs[i], durs[j] = durs[j], durs[i]
			}
		}
	}
	if got := percentile(durs, 0.50); got != 200 {
		t.Errorf("p50=%d want 200", got)
	}
	// percentile uses int(n-1 * p): for n=4 p=0.95 -> idx=2 -> 300
	if got := percentile(durs, 0.95); got != 300 {
		t.Errorf("p95=%d want 300", got)
	}
	if got := percentile(durs, 0.99); got != 300 {
		t.Errorf("p99=%d want 300", got)
	}
}
