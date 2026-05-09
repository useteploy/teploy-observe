package detectors

import "testing"

func TestSlowDBQuery(t *testing.T) {
	tests := []struct {
		name      string
		duration  int64
		wantFires bool
	}{
		{"100ms below threshold", 100, false},
		{"999ms boundary below", 999, false},
		{"1000ms boundary fires", 1000, true},
		{"5s well above", 5000, true},
	}

	d := NewSlowDBQuery()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			span := Span{
				TraceID:       "t1",
				SpanID:        "s1",
				ParentSpanID:  "p1",
				ServiceName:   "api",
				OperationName: "db.query",
				DurationMs:    tc.duration,
				Attributes:    map[string]string{"db.statement": "SELECT * FROM users WHERE id = 1", "db.system": "postgres"},
				StartMs:       1000,
				EndMs:         1000 + tc.duration,
			}
			got := d.Detect([]Span{span})
			if tc.wantFires && len(got) != 1 {
				t.Fatalf("want 1 issue, got %d", len(got))
			}
			if !tc.wantFires && len(got) != 0 {
				t.Fatalf("want 0 issues, got %d", len(got))
			}
		})
	}
}

func TestSlowDBQueryNonDBIgnored(t *testing.T) {
	d := NewSlowDBQuery()
	got := d.Detect([]Span{{
		TraceID: "t1", SpanID: "s1", ParentSpanID: "p1",
		ServiceName: "api", OperationName: "stripe.charge",
		DurationMs: 5000, StartMs: 1, EndMs: 5001,
	}})
	if len(got) != 0 {
		t.Fatalf("non-db span should not fire: got %d issues", len(got))
	}
}

func TestSlowDBQuerySeverity(t *testing.T) {
	d := NewSlowDBQuery()
	// 6x threshold should escalate to "error".
	got := d.Detect([]Span{{
		TraceID: "t1", SpanID: "s1", ParentSpanID: "p1",
		ServiceName: "api", OperationName: "db.query",
		DurationMs: 6000, StartMs: 1, EndMs: 6001,
		Attributes: map[string]string{"db.statement": "SELECT 1", "db.system": "postgres"},
	}})
	if len(got) != 1 {
		t.Fatalf("expected 1 issue")
	}
	if got[0].Severity != "error" {
		t.Errorf("severity = %q, want error", got[0].Severity)
	}
}
