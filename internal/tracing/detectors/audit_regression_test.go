package detectors

import (
	"reflect"
	"testing"
)

func TestOBS105AggregatePreservesOccurrencesLatestAndSeverity(t *testing.T) {
	a := Issue{Fingerprint: "fp", TraceID: "a", FirstSeen: 1000, LastSeen: 2500, Severity: "warning"}
	b := Issue{Fingerprint: "fp", TraceID: "b", FirstSeen: 3000, LastSeen: 9000, Severity: "error"}
	for _, input := range [][]Issue{{a, b}, {b, a}} {
		out := aggregateIssues(input)
		if len(out) != 1 || out[0].Occurrences != 2 || out[0].FirstSeen != 1000 || out[0].LastSeen != 9000 || out[0].TraceID != "b" || out[0].Severity != "error" {
			t.Fatalf("lost detection: %+v", out)
		}
	}
	if !reflect.DeepEqual(aggregateIssues([]Issue{a, b}), aggregateIssues([]Issue{b, a})) {
		t.Fatal("aggregation depends on arrival order")
	}
}
func TestOBS106LateAndEqualSnapshotsRemainVisible(t *testing.T) {
	var prev []issueSnapshot
	times := []int64{5000, 3000, 3000, 7000}
	for i, ts := range times {
		iss := Issue{Occurrences: 1, FirstSeen: ts, LastSeen: ts + 100, Severity: "warning"}
		count, first, last, version, severity := nextSnapshot(iss, prev, 10)
		if count != int64(i+1) || len(prev) > 0 && version <= prev[0].Version {
			t.Fatalf("non-current snapshot %d: count=%d version=%d", i, count, version)
		}
		if i == 1 && (first != 3000 || last != 5100) {
			t.Fatalf("late sample lost: first=%d last=%d", first, last)
		}
		prev = []issueSnapshot{{Count: count, FirstSeen: first, LastSeen: last, Version: version, Severity: severity}}
	}
}
func TestOBS107IndependentTraceSiblingsDoNotPool(t *testing.T) {
	var spans []Span
	for _, trace := range []string{"a", "b"} {
		for i := 0; i < 2; i++ {
			spans = append(spans, Span{TraceID: trace, SpanID: spanID(i), ParentSpanID: "same-parent", StartMs: int64(i * 60), EndMs: int64(i*60 + 60), DurationMs: 60, Attributes: map[string]string{"db.system": "pg", "db.statement": "SELECT * FROM users"}})
		}
	}
	for _, d := range []Detector{NewNPlusOneDB(), NewConsecutiveDB()} {
		if out := d.Detect(spans); len(out) != 0 {
			t.Fatalf("%s pooled independent traces: %+v", d.Name(), out)
		}
	}
}
func TestOBS109FastLongestRunDoesNotHideQualifyingRun(t *testing.T) {
	bounds := [][2]int64{{0, 10}, {10, 20}, {20, 30}, {30, 40}, {35, 85}, {85, 135}, {135, 185}}
	var spans []Span
	for i, b := range bounds {
		spans = append(spans, Span{TraceID: "t", SpanID: spanID(i), ParentSpanID: "p", StartMs: b[0], EndMs: b[1], DurationMs: b[1] - b[0], Attributes: map[string]string{"db.statement": "SELECT * FROM users"}})
	}
	for _, reverse := range []bool{false, true} {
		if reverse {
			for i, j := 0, len(spans)-1; i < j; i, j = i+1, j-1 {
				spans[i], spans[j] = spans[j], spans[i]
			}
		}
		out := NewConsecutiveDB().Detect(spans)
		if len(out) != 1 || out[0].FirstSeen != 35 || out[0].LastSeen != 185 {
			t.Fatalf("qualifying run hidden: %+v", out)
		}
	}
}
