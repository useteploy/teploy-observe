package platform

import (
	"testing"
	"time"
)

func TestAlertRuleRow_ToDomain(t *testing.T) {
	row := alertRuleRow{
		RuleID:        "r1",
		SiteID:        "site",
		Name:          "high errors",
		Metric:        "error_count",
		Operator:      "gt",
		Threshold:     "5.5",
		WindowMinutes: "10",
		CheckInterval: "60",
		Cooldown:      "30",
		Enabled:       "true",
		CreatedBy:     "admin",
		CreatedAt:     "1712000000000",
	}
	d := row.toDomain()
	if d.Threshold != 5.5 {
		t.Errorf("threshold: want 5.5 got %v", d.Threshold)
	}
	if d.WindowMinutes != 10 {
		t.Errorf("window: want 10 got %d", d.WindowMinutes)
	}
	if d.CheckInterval != 60 {
		t.Errorf("check_interval: want 60 got %d", d.CheckInterval)
	}
	if d.Cooldown != 30 {
		t.Errorf("cooldown: want 30 got %d", d.Cooldown)
	}
	if !d.Enabled {
		t.Error("enabled: want true")
	}
	expected := time.UnixMilli(1712000000000).UTC()
	if !d.CreatedAt.Equal(expected) {
		t.Errorf("created_at: want %v got %v", expected, d.CreatedAt)
	}
}

func TestAlertRuleRow_ToDomain_Disabled(t *testing.T) {
	cases := map[string]bool{
		"true": true, "1": true, "True": true, "TRUE": true,
		"false": false, "0": false, "": false, "garbage": false,
	}
	for input, want := range cases {
		got := parseBool(input)
		if got != want {
			t.Errorf("parseBool(%q): want %v got %v", input, want, got)
		}
	}
}

func TestAlertRuleRow_ToDomain_BadNumbers(t *testing.T) {
	row := alertRuleRow{
		Threshold:     "not a number",
		WindowMinutes: "abc",
		Cooldown:      "",
	}
	d := row.toDomain()
	if d.Threshold != 0 {
		t.Errorf("threshold: want 0 on parse error, got %v", d.Threshold)
	}
	if d.WindowMinutes != 0 {
		t.Errorf("window: want 0 on parse error, got %v", d.WindowMinutes)
	}
	if d.Cooldown != 0 {
		t.Errorf("cooldown: want 0 on empty, got %v", d.Cooldown)
	}
}

func TestAlertHistoryRow_ToDomain(t *testing.T) {
	row := alertHistoryRow{
		AlertID:     "a1",
		RuleID:      "r1",
		SiteID:      "site",
		TriggeredAt: "1712000000000",
		MetricValue: "42.5",
		Threshold:   "40.0",
		Status:      "triggered",
	}
	d := row.toDomain()
	if d.MetricValue != 42.5 {
		t.Errorf("metric_value: want 42.5 got %v", d.MetricValue)
	}
	if d.Threshold != 40.0 {
		t.Errorf("threshold: want 40 got %v", d.Threshold)
	}
	if d.TriggeredAt.IsZero() {
		t.Error("triggered_at: zero")
	}
}

func TestParseEpochMillis_RFC3339Fallback(t *testing.T) {
	s := "2024-04-01T12:00:00Z"
	got := parseEpochMillis(s)
	if got.IsZero() {
		t.Errorf("RFC3339 fallback failed for %q", s)
	}
}

func TestParseEpochMillis_Empty(t *testing.T) {
	if !parseEpochMillis("").IsZero() {
		t.Error("empty string should return zero time")
	}
}
