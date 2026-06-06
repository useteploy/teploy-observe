package jobs

import (
	"testing"
	"time"
)

func TestIsValidCronSpec(t *testing.T) {
	valid := []string{"@hourly", "@daily", "@weekly", "*/5 * * * *", "*/1 * * * *"}
	for _, s := range valid {
		if !isValidCronSpec(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	invalid := []string{"", "0 9 * * *", "@minutely", "*/0 * * * *", "*/abc * * * *", "daily"}
	for _, s := range invalid {
		if isValidCronSpec(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestIsDue(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	hourAgo := now.Add(-2 * time.Hour).UnixMilli()
	justNow := now.Add(-1 * time.Minute).UnixMilli()

	if !isDue("@hourly", hourAgo, now) {
		t.Error("@hourly should be due after 2h")
	}
	if isDue("@hourly", justNow, now) {
		t.Error("@hourly should not be due after 1m")
	}
	if !isDue("*/5 * * * *", now.Add(-6*time.Minute).UnixMilli(), now) {
		t.Error("*/5 should be due after 6m")
	}
	// Plain 5-field cron is not supported by isDue today; document that.
	if isDue("0 9 * * *", 0, now) {
		t.Error("plain 5-field cron is not interpreted (rejected at create); isDue returns false")
	}
}
