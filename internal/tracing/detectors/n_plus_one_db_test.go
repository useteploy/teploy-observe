package detectors

import "testing"

// makeDBSpans builds n sibling DB spans under a shared parent with the given
// statement template. Each span starts after the previous ends so they're
// temporally consecutive.
func makeDBSpans(parent, traceID string, statements []string) []Span {
	out := make([]Span, 0, len(statements))
	start := int64(1000)
	for i, stmt := range statements {
		out = append(out, Span{
			TraceID:       traceID,
			SpanID:        spanID(i),
			ParentSpanID:  parent,
			ServiceName:   "api",
			OperationName: "db.query",
			SpanKind:      "client",
			StartMs:       start,
			EndMs:         start + 5,
			DurationMs:    5,
			Attributes:    map[string]string{"db.statement": stmt, "db.system": "postgres"},
		})
		start += 6
	}
	return out
}

func spanID(i int) string { return "sp" + string(rune('0'+i)) }

func TestNPlusOneDB(t *testing.T) {
	parent := "p1"
	trace := "t1"
	template := "SELECT * FROM users WHERE id = ?"

	tests := []struct {
		name      string
		spans     []Span
		wantFires bool
	}{
		{
			name: "five identical templates fires",
			spans: append(
				[]Span{{SpanID: parent, TraceID: trace, OperationName: "GET /users"}},
				makeDBSpans(parent, trace, []string{
					"SELECT * FROM users WHERE id = 1",
					"SELECT * FROM users WHERE id = 2",
					"SELECT * FROM users WHERE id = 3",
					"SELECT * FROM users WHERE id = 4",
					"SELECT * FROM users WHERE id = 5",
				})...,
			),
			wantFires: true,
		},
		{
			name: "five different statements does not fire",
			spans: append(
				[]Span{{SpanID: parent, TraceID: trace, OperationName: "GET /users"}},
				makeDBSpans(parent, trace, []string{
					"SELECT * FROM users WHERE id = 1",
					"SELECT * FROM orders WHERE id = 1",
					"SELECT * FROM payments WHERE id = 1",
					"SELECT * FROM addresses WHERE id = 1",
					"SELECT * FROM carts WHERE id = 1",
				})...,
			),
			wantFires: false,
		},
		{
			name: "three identical templates under threshold",
			spans: append(
				[]Span{{SpanID: parent, TraceID: trace, OperationName: "GET /users"}},
				makeDBSpans(parent, trace, []string{
					"SELECT * FROM users WHERE id = 1",
					"SELECT * FROM users WHERE id = 2",
					"SELECT * FROM users WHERE id = 3",
				})...,
			),
			wantFires: false,
		},
		{
			name: "exactly four identical templates fires (boundary)",
			spans: append(
				[]Span{{SpanID: parent, TraceID: trace, OperationName: "GET /users"}},
				makeDBSpans(parent, trace, []string{
					"SELECT * FROM users WHERE id = 1",
					"SELECT * FROM users WHERE id = 2",
					"SELECT * FROM users WHERE id = 3",
					"SELECT * FROM users WHERE id = 4",
				})...,
			),
			wantFires: true,
		},
		{
			name: "non-sql statements skipped",
			spans: append(
				[]Span{{SpanID: parent, TraceID: trace, OperationName: "GET /users"}},
				makeDBSpans(parent, trace, []string{"random text", "random text", "random text", "random text"})...,
			),
			wantFires: false,
		},
		{
			name: "different parents do not aggregate",
			spans: []Span{
				{SpanID: parent, TraceID: trace, OperationName: "GET /users"},
				// 2 under p1, 2 under p2 — neither hits threshold individually.
				{TraceID: trace, SpanID: "a1", ParentSpanID: "p1", OperationName: "db.q",
					Attributes: map[string]string{"db.statement": template, "db.system": "postgres"}, StartMs: 1, EndMs: 2, DurationMs: 1},
				{TraceID: trace, SpanID: "a2", ParentSpanID: "p1", OperationName: "db.q",
					Attributes: map[string]string{"db.statement": template, "db.system": "postgres"}, StartMs: 3, EndMs: 4, DurationMs: 1},
				{TraceID: trace, SpanID: "b1", ParentSpanID: "p2", OperationName: "db.q",
					Attributes: map[string]string{"db.statement": template, "db.system": "postgres"}, StartMs: 5, EndMs: 6, DurationMs: 1},
				{TraceID: trace, SpanID: "b2", ParentSpanID: "p2", OperationName: "db.q",
					Attributes: map[string]string{"db.statement": template, "db.system": "postgres"}, StartMs: 7, EndMs: 8, DurationMs: 1},
			},
			wantFires: false,
		},
	}

	d := NewNPlusOneDB()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := d.Detect(tc.spans)
			if tc.wantFires && len(got) == 0 {
				t.Fatalf("expected at least one issue, got none")
			}
			if !tc.wantFires && len(got) > 0 {
				t.Fatalf("expected no issues, got %d: %+v", len(got), got)
			}
			if tc.wantFires {
				iss := got[0]
				if iss.DetectorName != "n_plus_one_db" {
					t.Errorf("detector name = %q, want n_plus_one_db", iss.DetectorName)
				}
				if iss.Fingerprint == "" {
					t.Errorf("fingerprint must not be empty")
				}
				if iss.TraceID != trace {
					t.Errorf("trace_id = %q, want %q", iss.TraceID, trace)
				}
			}
		})
	}
}

func TestFingerprintSQL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"SELECT * FROM users WHERE id = 1", "SELECT * FROM users WHERE id = ?"},
		{"SELECT * FROM users WHERE id = 12345", "SELECT * FROM users WHERE id = ?"},
		{"SELECT * FROM users WHERE name = 'tyler'", "SELECT * FROM users WHERE name = ?"},
		{"SELECT  *   FROM\tusers", "SELECT * FROM users"},
		{"random text", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := fingerprintSQL(c.in)
		if got != c.want {
			t.Errorf("fingerprintSQL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
