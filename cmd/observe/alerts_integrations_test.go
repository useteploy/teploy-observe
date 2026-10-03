package main

import (
	"context"
	"testing"

	"github.com/useteploy/teploy-observe/internal/integrations"
	"github.com/useteploy/teploy-observe/internal/platform"
)

func TestAlertMetricWhitelistRejectsUnknown(t *testing.T) {
	for _, ok := range []string{"error_count", "trace_error_rate", "trace_p95_ms", "log_error_count", "uptime_failures"} {
		if _, in := validAlertMetrics[ok]; !in {
			t.Errorf("%s should be accepted", ok)
		}
	}
	for _, bad := range []string{"metric_value", "cpu", "error_count'; --", ""} {
		if _, in := validAlertMetrics[bad]; in {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestAlertFireToIntegrationsMapsPayload(t *testing.T) {
	var gotSite string
	var got integrations.AlertPayload
	fn := alertFireToIntegrations(func(_ context.Context, site string, p integrations.AlertPayload) {
		gotSite, got = site, p
	})
	fn(context.Background(), platform.AlertFireEvent{
		SiteID: "s1", Title: "t", Message: "m", Severity: "critical",
		RuleName: "r", Metric: "uptime_failures", Value: "3.00", Threshold: "1",
	})
	if gotSite != "s1" || got.Metric != "uptime_failures" || got.Severity != "critical" || got.Value != "3.00" || got.Threshold != "1" || got.SiteID != "s1" {
		t.Fatalf("payload = %+v site=%s", got, gotSite)
	}
}
