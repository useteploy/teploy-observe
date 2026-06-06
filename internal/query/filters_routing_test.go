package query

import (
	"testing"
	"time"
)

func TestFilterBuilder_SQLAndParams(t *testing.T) {
	fb := NewFilterBuilder(4)
	fb.Add("browser", "Chrome")
	fb.AddIn("distinct_id", []string{"u1", "u2"})
	fb.Add("language", "en")

	wantSQL := " AND browser = $4 AND distinct_id IN ($5, $6) AND language = $7"
	if got := fb.SQL(); got != wantSQL {
		t.Fatalf("SQL = %q, want %q", got, wantSQL)
	}
	params := fb.Params()
	if len(params) != 4 {
		t.Fatalf("want 4 params, got %d: %v", len(params), params)
	}
	if fb.NextIdx() != 8 {
		t.Fatalf("NextIdx = %d, want 8", fb.NextIdx())
	}
}

func TestFilterBuilder_EmptyCohortSentinel(t *testing.T) {
	fb := NewFilterBuilder(4)
	fb.AddIn("distinct_id", nil)
	if got := fb.SQL(); got != " AND 1 = 0" {
		t.Fatalf("empty cohort SQL = %q, want match-nothing sentinel", got)
	}
	if len(fb.Params()) != 0 {
		t.Fatalf("empty cohort should have no params")
	}
}

func TestFilterBuilder_Subset(t *testing.T) {
	fb := NewFilterBuilder(4)
	fb.Add("browser", "Chrome")
	fb.AddIn("distinct_id", []string{"u1"})
	fb.Add("pathname", "/x")
	fb.Add("language", "en")

	sub := fb.Subset(sessionsFilterColumns)
	// distinct_id and pathname are not on the sessions table; they must drop,
	// and the remaining params must renumber from the same start index.
	wantSQL := " AND browser = $4 AND language = $5"
	if got := sub.SQL(); got != wantSQL {
		t.Fatalf("subset SQL = %q, want %q", got, wantSQL)
	}
	if got := sub.Params(); len(got) != 2 {
		t.Fatalf("subset params = %v, want 2", got)
	}
}

func TestTableForFilters_ForcesEventsForMissingColumns(t *testing.T) {
	now := time.Now()
	wide := now.Add(-72 * time.Hour) // would normally use stats_hourly

	// language is absent on stats_hourly -> must force events.
	fbLang := NewFilterBuilder(4)
	fbLang.Add("language", "en")
	if got := tableForFilters(wide, now, fbLang); got != "events" {
		t.Fatalf("language filter over 72h should force events, got %q", got)
	}

	// cohort distinct_id absent on every rollup -> force events even at 30d.
	fbCohort := NewFilterBuilder(4)
	fbCohort.AddIn("distinct_id", []string{"u1"})
	veryWide := now.Add(-30 * 24 * time.Hour)
	if got := tableForFilters(veryWide, now, fbCohort); got != "events" {
		t.Fatalf("cohort filter over 30d should force events, got %q", got)
	}

	// pathname IS on stats_hourly -> normal routing (not forced).
	fbPath := NewFilterBuilder(4)
	fbPath.Add("pathname", "/x")
	if got := tableForFilters(wide, now, fbPath); got != "stats_hourly" {
		t.Fatalf("pathname filter over 72h should stay on stats_hourly, got %q", got)
	}
}
