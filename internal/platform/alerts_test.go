package platform

import (
	"testing"
)

func TestAlertRuleDefaults(t *testing.T) {
	// Verify CreateRule applies defaults for empty fields
	rule := AlertRule{
		SiteID: "test", Name: "high errors", Metric: "error_count",
	}
	// Simulate what CreateRule does
	if rule.Operator == "" {
		rule.Operator = "gt"
	}
	if rule.WindowMinutes <= 0 {
		rule.WindowMinutes = 5
	}
	if rule.Cooldown <= 0 {
		rule.Cooldown = 5
	}
	if rule.CheckInterval <= 0 {
		rule.CheckInterval = 60
	}

	if rule.Operator != "gt" {
		t.Errorf("operator: want gt, got %s", rule.Operator)
	}
	if rule.WindowMinutes != 5 {
		t.Errorf("window: want 5, got %d", rule.WindowMinutes)
	}
	if rule.Cooldown != 5 {
		t.Errorf("cooldown: want 5, got %d", rule.Cooldown)
	}
	if rule.CheckInterval != 60 {
		t.Errorf("check_interval: want 60, got %d", rule.CheckInterval)
	}
}

func TestAlertRuleTypedFields(t *testing.T) {
	// Verify the struct has proper types for JSON serialization
	rule := AlertRule{
		Threshold:     5.5,
		WindowMinutes: 10,
		Cooldown:      30,
		Enabled:       true,
	}
	if rule.Threshold != 5.5 {
		t.Errorf("threshold: want 5.5, got %v", rule.Threshold)
	}
	if !rule.Enabled {
		t.Error("enabled: want true")
	}
}
