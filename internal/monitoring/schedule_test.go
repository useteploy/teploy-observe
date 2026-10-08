package monitoring

import (
	"testing"
	"time"
)

func TestNextScheduledRunCommonForms(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct{ schedule, next string }{
		{"@hourly", "2026-01-01T01:00:00Z"},
		{"@daily", "2026-01-02T00:00:00Z"},
		{"@midnight", "2026-01-02T00:00:00Z"},
		{"@weekly", "2026-01-04T00:00:00Z"},
		{"@monthly", "2026-02-01T00:00:00Z"},
		{"@yearly", "2027-01-01T00:00:00Z"},
		{"@every 90s", "2026-01-01T00:01:30Z"},
		{"@every 15m", "2026-01-01T00:15:00Z"},
		{"* * * * *", "2026-01-01T00:01:00Z"},
		{"*/5 * * * *", "2026-01-01T00:05:00Z"},
		{"0,30 * * * *", "2026-01-01T00:30:00Z"},
		{"15-45 * * * *", "2026-01-01T00:15:00Z"},
		{"0 * * * *", "2026-01-01T01:00:00Z"},
		{"30 */6 * * *", "2026-01-01T00:30:00Z"},
		{"0 3 * * *", "2026-01-01T03:00:00Z"},
		{"0 3 * * 1", "2026-01-05T03:00:00Z"},
		{"0 3 1 * *", "2026-01-01T03:00:00Z"},
		{"0 */5 * * * *", "2026-01-01T00:05:00Z"},
	} {
		next, ok := NextScheduledRun(c.schedule, after)
		want, _ := time.Parse(time.RFC3339, c.next)
		if !ok || !next.Equal(want) {
			t.Errorf("next(%q)=%v %v want %v", c.schedule, next, ok, want)
		}
	}
	for _, schedule := range []string{"", "not a schedule", "@reboot", "@every nonsense"} {
		if _, ok := NextScheduledRun(schedule, after); ok {
			t.Errorf("accepted %q", schedule)
		}
	}
}

func TestCronDeadlineGraceOnlyAndHourly(t *testing.T) {
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		schedule string
		grace    int
		silence  time.Duration
	}{
		{"0 * * * *", 300, time.Hour + 5*time.Minute},
		{"", 300, 5 * time.Minute},
		{"", 0, 5 * time.Minute},
	} {
		got, ok := CronDeadline(c.schedule, last, c.grace)
		if !ok || !got.Equal(last.Add(c.silence)) {
			t.Fatalf("deadline(%q)=%v %v", c.schedule, got, ok)
		}
	}
	if _, ok := CronDeadline("invalid", last, 300); ok {
		t.Fatal("invalid historical schedule would create false grace-only incidents")
	}
}
