package detectors

import "testing"

func TestConsecutiveDB(t *testing.T) {
	parent := "p1"
	trace := "t1"
	mkSpan := func(id string, start, end int64, stmt string) Span {
		return Span{
			TraceID: trace, SpanID: id, ParentSpanID: parent,
			ServiceName: "api", OperationName: "db.query",
			SpanKind:   "client",
			StartMs:    start, EndMs: end, DurationMs: end - start,
			Attributes: map[string]string{"db.statement": stmt, "db.system": "postgres"},
		}
	}

	t.Run("three serial spans fire", func(t *testing.T) {
		spans := []Span{
			mkSpan("a", 0, 50, "SELECT * FROM users"),
			mkSpan("b", 50, 100, "SELECT * FROM orders"),
			mkSpan("c", 100, 150, "SELECT * FROM payments"),
		}
		got := NewConsecutiveDB().Detect(spans)
		if len(got) != 1 {
			t.Fatalf("expected 1 issue, got %d", len(got))
		}
	})

	t.Run("three parallel spans do not fire", func(t *testing.T) {
		spans := []Span{
			mkSpan("a", 0, 50, "SELECT * FROM users"),
			mkSpan("b", 10, 60, "SELECT * FROM orders"), // overlaps a
			mkSpan("c", 20, 70, "SELECT * FROM payments"), // overlaps a, b
		}
		got := NewConsecutiveDB().Detect(spans)
		if len(got) != 0 {
			t.Fatalf("parallel spans should not fire, got %d", len(got))
		}
	})

	t.Run("under-threshold serial spans do not fire", func(t *testing.T) {
		spans := []Span{
			mkSpan("a", 0, 50, "SELECT * FROM users"),
			mkSpan("b", 50, 100, "SELECT * FROM orders"),
		}
		got := NewConsecutiveDB().Detect(spans)
		if len(got) != 0 {
			t.Fatalf("two-span run should not fire, got %d", len(got))
		}
	})

	t.Run("low total duration suppressed", func(t *testing.T) {
		// 3 spans @ 5ms each = 15ms total, under the 100ms floor.
		spans := []Span{
			mkSpan("a", 0, 5, "SELECT 1"),
			mkSpan("b", 5, 10, "SELECT 2"),
			mkSpan("c", 10, 15, "SELECT 3"),
		}
		got := NewConsecutiveDB().Detect(spans)
		if len(got) != 0 {
			t.Fatalf("low-duration run should be suppressed, got %d", len(got))
		}
	})
}
