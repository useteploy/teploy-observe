package metrics

import "testing"

func TestBuildMetricPlaceholders_Shape(t *testing.T) {
	got := buildMetricPlaceholders(2)
	want := "($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11),($12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMetricArgs_MatchesColumnCount(t *testing.T) {
	r := metricPointRow{name: "n", kind: "gauge"}
	args := metricArgs(nil, "site1", &r)
	if len(args) != metricPointCols {
		t.Fatalf("metricArgs produced %d args, want %d (metricPointCols)", len(args), metricPointCols)
	}
}

func TestInsertMetricRows_EmptyIsNoop(t *testing.T) {
	n, err := insertMetricRows(nil, nil, "site1", nil)
	if err != nil || n != 0 {
		t.Fatalf("insertMetricRows(empty) = (%d, %v), want (0, nil)", n, err)
	}
}
