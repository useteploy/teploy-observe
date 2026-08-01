package tracing

import "testing"

func TestBuildSpanPlaceholders_Shape(t *testing.T) {
	got := buildSpanPlaceholders(2)
	want := "($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16),($17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSpanArgs_MatchesColumnCount(t *testing.T) {
	sp := flatSpan{TraceID: "t", SpanID: "s"}
	args := spanArgs(nil, "site1", &sp)
	if len(args) != spansCols {
		t.Fatalf("spanArgs produced %d args, want %d (spansCols)", len(args), spansCols)
	}
}

func TestInsertSpans_EmptyIsNoop(t *testing.T) {
	n, err := insertSpans(nil, nil, "site1", nil)
	if err != nil || n != 0 {
		t.Fatalf("insertSpans(empty) = (%d, %v), want (0, nil)", n, err)
	}
}
