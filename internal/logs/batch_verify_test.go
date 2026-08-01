package logs

import "testing"

func TestBuildLogPlaceholders_Shape(t *testing.T) {
	got := buildLogPlaceholders(2)
	want := "($1,$2,$3,$4,$5,$6,$7,$8,$9,$10),($11,$12,$13,$14,$15,$16,$17,$18,$19,$20)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestLogArgs_MatchesColumnCount(t *testing.T) {
	p := &preparedLog{id: "id1", input: LogInput{SiteID: "s"}, attrsJSON: "null", tsMs: "1"}
	args := logArgs(nil, p)
	if len(args) != logsCols {
		t.Fatalf("logArgs produced %d args, want %d (logsCols)", len(args), logsCols)
	}
}
