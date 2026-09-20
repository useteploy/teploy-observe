package monitoring

import (
	"strings"
	"testing"
)

// R20 (round 4): numeric configuration is validated, not silently rewritten.
func TestValidateMonitorBounds(t *testing.T) {
	ok := Monitor{SiteID: "s", Name: "n", URL: "https://example.com", IntervalSecs: 60, ExpectedStatus: 200}
	if err := validateMonitor(ok); err != nil {
		t.Fatalf("valid monitor rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Monitor)
	}{
		{"negative interval", func(m *Monitor) { m.IntervalSecs = -5 }},
		{"interval below floor", func(m *Monitor) { m.IntervalSecs = 5 }},
		{"interval above ceiling", func(m *Monitor) { m.IntervalSecs = 90000 }},
		{"status below range", func(m *Monitor) { m.ExpectedStatus = 99 }},
		{"status above range", func(m *Monitor) { m.ExpectedStatus = 600 }},
		{"empty site", func(m *Monitor) { m.SiteID = " " }},
		{"empty name", func(m *Monitor) { m.Name = "" }},
	} {
		m := ok
		tc.mut(&m)
		if err := validateMonitor(m); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
}

// R20 (round 4): cron bounds — grace nonnegative and bounded, bounded
// name/slug/schedule, empty schedule (grace-only mode) still legal.
func TestValidateCronBounds(t *testing.T) {
	ok := CronMonitor{SiteID: "s", Name: "n", Slug: "n", Schedule: "", GracePeriod: 300, Enabled: true}
	if err := validateCron(ok); err != nil {
		t.Fatalf("valid cron rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*CronMonitor)
	}{
		{"negative grace", func(c *CronMonitor) { c.GracePeriod = -1 }},
		{"grace above day", func(c *CronMonitor) { c.GracePeriod = 86401 }},
		{"empty name", func(c *CronMonitor) { c.Name = "" }},
		{"name too long", func(c *CronMonitor) { c.Name = strings.Repeat("x", 129) }},
		{"slug too long", func(c *CronMonitor) { c.Slug = strings.Repeat("x", 129) }},
		{"schedule too long", func(c *CronMonitor) { c.Schedule = strings.Repeat("x", 65) }},
	} {
		c := ok
		tc.mut(&c)
		if err := validateCron(c); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
}

// R21 (round 4): caller-supplied limits are defaulted and capped.
func TestPageLimit(t *testing.T) {
	for _, tc := range []struct{ requested, want int }{
		{0, 50}, {-3, 50}, {1, 1}, {50, 50}, {499, 499}, {500, 500}, {501, 500}, {1 << 30, 500},
	} {
		if got := pageLimit(tc.requested, 50, 500); got != tc.want {
			t.Fatalf("pageLimit(%d)=%d want %d", tc.requested, got, tc.want)
		}
	}
}

// R19 (round 4): replacement versions never regress or tie.
func TestNextMutationVersion(t *testing.T) {
	if got := nextMutationVersionInt(1000, 1000); got != 1001 {
		t.Fatalf("same-ms tie: got %d want 1001", got)
	}
	if got := nextMutationVersionInt(2000, 1000); got != 2001 {
		t.Fatalf("clock rollback: got %d want 2001 (strictly greater than prior)", got)
	}
	if got := nextMutationVersionInt(999, 1000); got != 1000 {
		t.Fatalf("normal advance: got %d want 1000", got)
	}
}
