package monitoring

import (
	"testing"
	"time"
)

func TestSchedulePeriod(t *testing.T) {
	cases := []struct {
		schedule string
		want     time.Duration
		ok       bool
	}{
		{"", 0, false},
		{"not a schedule", 0, false},
		{"@reboot", 0, false},

		{"@hourly", time.Hour, true},
		{"@daily", 24 * time.Hour, true},
		{"@midnight", 24 * time.Hour, true},
		{"@weekly", 7 * 24 * time.Hour, true},
		{"@monthly", 30 * 24 * time.Hour, true},
		{"@yearly", 365 * 24 * time.Hour, true},
		{"@every 90s", 90 * time.Second, true},
		{"@every 15m", 15 * time.Minute, true},
		{"@every nonsense", 0, false},

		{"* * * * *", time.Minute, true},
		{"*/5 * * * *", 5 * time.Minute, true},
		{"0,30 * * * *", 30 * time.Minute, true},
		{"15-45 * * * *", time.Minute, true},

		// Minute pinned: the hour field sets the period.
		{"0 * * * *", time.Hour, true},
		{"30 */6 * * *", 6 * time.Hour, true},

		// Minute and hour both pinned: the day fields do.
		{"0 3 * * *", 24 * time.Hour, true},
		{"0 3 * * 1", 7 * 24 * time.Hour, true},
		{"0 3 1 * *", 31 * 24 * time.Hour, true},

		// Six fields is seconds-first; the minute-onward tail is what counts.
		{"0 */5 * * * *", 5 * time.Minute, true},
	}
	for _, c := range cases {
		got, ok := SchedulePeriod(c.schedule)
		if ok != c.ok || got != c.want {
			t.Errorf("SchedulePeriod(%q) = %v, %v; want %v, %v", c.schedule, got, ok, c.want, c.ok)
		}
	}
}

// TestDueAfterMs is the arithmetic behind the incident flood.
//
// The detector used to allow a monitor only its GRACE PERIOD of silence,
// ignoring its schedule entirely. An hourly cron with a five-minute grace was
// therefore "missed" for fifty-five minutes out of every hour: an incident
// opened, the next hourly run closed it, and the cycle repeated forever — one
// incident per cron run. That is how ten monitors produced 12,398 incident rows
// on the live instance.
func TestDueAfterMs(t *testing.T) {
	const grace = 300 // seconds

	hourly := DueAfterMs("0 * * * *", grace)
	if want := int64((time.Hour + 300*time.Second) / time.Millisecond); hourly != want {
		t.Fatalf("hourly cron may be silent for %dms, want %dms", hourly, want)
	}
	if hourly <= int64(grace)*1000 {
		t.Fatal("an hourly cron is judged on its grace period alone — it will be declared missed between every pair of runs")
	}

	// An unreadable schedule keeps the old grace-only behaviour, which is the
	// only safe fallback: it alerts earlier, never later.
	if got, want := DueAfterMs("", grace), int64(grace)*1000; got != want {
		t.Fatalf("empty schedule allows %dms of silence, want the grace period %dms", got, want)
	}

	// A zero or negative grace falls back to the 300s default rather than
	// making every monitor instantly overdue.
	if got, want := DueAfterMs("", 0), int64(300)*1000; got != want {
		t.Fatalf("zero grace allows %dms, want the %dms default", got, want)
	}
}
