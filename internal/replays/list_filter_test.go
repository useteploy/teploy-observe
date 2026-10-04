package replays

import (
	"strings"
	"testing"
	"time"
)

var (
	tFrom = time.UnixMilli(1_000_000)
	tTo   = time.UnixMilli(2_000_000)
)

func TestBuildListQueryUnfiltered(t *testing.T) {
	q, args := buildListQuery("s1", tFrom, tTo, 0, -5, ReplayFilter{}, "")
	if len(args) != 3 || strings.Contains(q, "WHERE distinct_id") || !strings.Contains(q, "LIMIT 20 OFFSET 0") {
		t.Fatalf("unexpected: %s %v", q, args)
	}
}

func TestBuildListQueryFilters(t *testing.T) {
	f := ReplayFilter{HasErrors: true, MinDurationMS: 5000, URLContains: "/pricing", DistinctID: "u1"}
	q, args := buildListQuery("s1", tFrom, tTo, 999, 10, f, "HASH")
	for _, want := range []string{
		"distinct_id = $4", "CAST(duration_ms AS BIGINT) >= CAST($5 AS BIGINT)",
		"url ILIKE '%' || $6 || '%'", "FROM error_events WHERE site_id = $1", "LIMIT 200 OFFSET 10",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q in:\n%s", want, q)
		}
	}
	if len(args) != 7 || args[3] != "HASH" || args[5] != "/pricing" {
		t.Fatalf("args: %v", args)
	}
	if strings.Contains(q, "HASH") || strings.Contains(q, "/pricing") || strings.Contains(q, "u1") {
		t.Fatalf("user value interpolated: %s", q)
	}
}

func TestBuildListQueryInjectionCorpus(t *testing.T) {
	corpus := []string{
		"'; DROP TABLE replay_sessions; --", "' OR '1'='1", `\' OR 1=1 --`,
		"%' UNION SELECT * FROM api_keys --", "$1; DELETE FROM x", "100%_\\",
	}
	for _, c := range corpus {
		q, args := buildListQuery("s1", tFrom, tTo, 20, 0, ReplayFilter{URLContains: c, DistinctID: c}, c)
		if strings.Contains(q, "DROP") || strings.Contains(q, "UNION") || strings.Contains(q, "DELETE") || strings.Contains(q, "1=1") {
			t.Errorf("payload reached SQL text: %q -> %s", c, q)
		}
		found := false
		for _, a := range args {
			if a == escapeLike(c) {
				found = true
			}
		}
		if !found {
			t.Errorf("escaped payload not bound: %q", c)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`50%_a\b`); got != `50\%\_a\\b` {
		t.Fatal(got)
	}
}

func TestValidateFilter(t *testing.T) {
	ok := []ReplayFilter{{}, {MinDurationMS: 1}, {URLContains: strings.Repeat("a", maxFilterURLContains)}}
	for _, f := range ok {
		if err := validateFilter(f); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
	bad := []ReplayFilter{
		{URLContains: strings.Repeat("a", maxFilterURLContains+1)},
		{DistinctID: strings.Repeat("a", maxFilterDistinctID+1)},
		{URLContains: "a\x00"}, {DistinctID: "\x00"},
		{MinDurationMS: -1}, {MinDurationMS: maxFilterMinDurationMS + 1},
	}
	for _, f := range bad {
		if validateFilter(f) == nil {
			t.Errorf("accepted %+v", f)
		}
	}
}
