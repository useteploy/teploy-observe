package monitoring

import (
	"testing"
	"time"
)

func TestCronDeadlineCalendarBoundaries(t *testing.T) {
	for _, c := range []struct{ schedule, last, next string }{
		{"0 9 * * 1-5", "2026-10-02T09:00:00Z", "2026-10-05T09:00:00Z"},
		{"0,5 * * * *", "2026-10-02T09:05:00Z", "2026-10-02T10:00:00Z"},
		{"15-45 * * * *", "2026-10-02T09:45:00Z", "2026-10-02T10:15:00Z"},
		{"@monthly", "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"0 0 1 1 *", "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"0 0 29 2 *", "2096-02-29T00:00:00Z", "2104-02-29T00:00:00Z"},
		{"*/15 * * * * *", "2026-10-02T09:00:30Z", "2026-10-02T09:00:45Z"},
		{"0 9 * * mon-fri", "2026-10-02T09:00:00Z", "2026-10-05T09:00:00Z"},
		{"@every 90s", "2026-10-02T09:00:00Z", "2026-10-02T09:01:30Z"},
		// Non-UTC inputs are interpreted on the same absolute UTC calendar; DST
		// does not move the expected wall-clock hour of this UTC-only contract.
		{"0 9 * * *", "2026-03-07T09:00:00-08:00", "2026-03-08T09:00:00Z"},
	} {
		t.Run(c.schedule+"/"+c.last, func(t *testing.T) {
			last, _ := time.Parse(time.RFC3339, c.last)
			next, _ := time.Parse(time.RFC3339, c.next)
			deadline, ok := CronDeadline(c.schedule, last, 300)
			if !ok || !deadline.Equal(next.Add(5*time.Minute)) {
				t.Fatalf("deadline %v %v, expected %v", deadline, ok, next.Add(5*time.Minute))
			}
			if next.Before(deadline) && deadline.Before(next.Add(5*time.Minute)) {
				t.Fatal("early missed deadline")
			}
		})
	}
}
func TestCronScheduleAdmissionRejectsUnsupportedAndImpossible(t *testing.T) {
	for _, schedule := range []string{"bad", "@reboot", "0 0 30 2 *", "99 * * * *", "*/0 * * * *", "0 0 * 13 *", "0 0 * * 8", "0 0 * * * extra extra"} {
		if err := validateCron(CronMonitor{SiteID: "s", Name: "cron", Schedule: schedule}); err == nil {
			t.Errorf("accepted %q", schedule)
		}
	}
}
