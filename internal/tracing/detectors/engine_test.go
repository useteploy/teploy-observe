package detectors

import (
	"testing"
)

// TestEngineRunAllSyntheticBatch crafts a single batch with one example of
// each detector pattern and asserts the engine emits the expected detector
// names. This is the contract test — any future detector added to the
// default suite needs an entry here.
func TestEngineRunAllSyntheticBatch(t *testing.T) {
	parentNPlus := "parent_nplus"
	parentSerial := "parent_serial"
	parentSlow := "parent_slow"

	spans := []Span{
		// Roots so the perf-issue traces have real parents and trace IDs.
		{SpanID: parentNPlus, TraceID: "trace-nplus", OperationName: "GET /n+1"},
		{SpanID: parentSerial, TraceID: "trace-serial", OperationName: "GET /serial"},
		{SpanID: parentSlow, TraceID: "trace-slow", OperationName: "GET /slow"},

		// N+1: 5 same-template DB spans under parentNPlus.
		dbSpan("n1", parentNPlus, "trace-nplus", "SELECT * FROM users WHERE id = 1", 0, 5),
		dbSpan("n2", parentNPlus, "trace-nplus", "SELECT * FROM users WHERE id = 2", 5, 10),
		dbSpan("n3", parentNPlus, "trace-nplus", "SELECT * FROM users WHERE id = 3", 10, 15),
		dbSpan("n4", parentNPlus, "trace-nplus", "SELECT * FROM users WHERE id = 4", 15, 20),
		dbSpan("n5", parentNPlus, "trace-nplus", "SELECT * FROM users WHERE id = 5", 20, 25),

		// Consecutive: 3 different DB spans serially under parentSerial,
		// total > 100ms.
		dbSpan("c1", parentSerial, "trace-serial", "SELECT * FROM users", 0, 50),
		dbSpan("c2", parentSerial, "trace-serial", "SELECT * FROM orders", 50, 100),
		dbSpan("c3", parentSerial, "trace-serial", "SELECT * FROM payments", 100, 200),

		// Slow DB query: single 2s DB call.
		{
			TraceID: "trace-slow", SpanID: "slow_db", ParentSpanID: parentSlow,
			ServiceName: "api", OperationName: "db.query", SpanKind: "client",
			StartMs: 0, EndMs: 2000, DurationMs: 2000,
			Attributes: map[string]string{"db.statement": "SELECT * FROM huge_table", "db.system": "postgres"},
		},

		// Slow HTTP call: single 5s outbound HTTP.
		{
			TraceID: "trace-slow", SpanID: "slow_http", ParentSpanID: parentSlow,
			ServiceName: "api", OperationName: "HTTP POST", SpanKind: "client",
			StartMs: 0, EndMs: 5000, DurationMs: 5000,
			Attributes: map[string]string{"http.url": "https://stripe.com/charge", "http.method": "POST"},
		},
	}

	e := NewWithDetectors(nil, DefaultDetectors())
	got := e.RunAll(spans)

	wantDetectors := map[string]bool{
		"n_plus_one_db":  false,
		"slow_db_query":  false,
		"consecutive_db": false,
		"slow_http_call": false,
	}
	for _, iss := range got {
		if _, ok := wantDetectors[iss.DetectorName]; !ok {
			t.Errorf("unexpected detector emitted: %q", iss.DetectorName)
			continue
		}
		wantDetectors[iss.DetectorName] = true
	}
	for name, fired := range wantDetectors {
		if !fired {
			t.Errorf("detector %q did not fire on synthetic batch", name)
		}
	}

	// Every issue must have a non-empty fingerprint and a trace_id —
	// downstream queries depend on both.
	for _, iss := range got {
		if iss.Fingerprint == "" {
			t.Errorf("issue %+v missing fingerprint", iss)
		}
		if iss.TraceID == "" {
			t.Errorf("issue %+v missing trace_id", iss)
		}
	}
}

// dbSpan is a small helper to keep the table above readable.
func dbSpan(id, parent, trace, stmt string, start, end int64) Span {
	return Span{
		TraceID: trace, SpanID: id, ParentSpanID: parent,
		ServiceName: "api", OperationName: "db.query", SpanKind: "client",
		StartMs:    start, EndMs: end, DurationMs: end - start,
		Attributes: map[string]string{"db.statement": stmt, "db.system": "postgres"},
	}
}

func TestEngineEmptyBatch(t *testing.T) {
	e := NewWithDetectors(nil, DefaultDetectors())
	if got := e.RunAll(nil); len(got) != 0 {
		t.Fatalf("nil batch should yield 0 issues, got %d", len(got))
	}
	if got := e.RunAll([]Span{}); len(got) != 0 {
		t.Fatalf("empty batch should yield 0 issues, got %d", len(got))
	}
}
