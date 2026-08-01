package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/integrations"
)

// TestRedactIntegration_NeverSerializesConfig is the regression for OBS-013:
// List (viewer-accessible) used to return the raw Config, which can carry a
// Jira/GitHub API token, PagerDuty routing key, SMTP credentials, or a Slack
// webhook URL. integrationSummary has no Config field at all, so this is
// enforced by the type system, not a runtime check that could regress —
// this test just confirms the marshaled JSON matches that expectation.
func TestRedactIntegration_NeverSerializesConfig(t *testing.T) {
	in := integrations.Integration{
		IntegrationID: "int_1",
		SiteID:        "site_1",
		Name:          "prod pagerduty",
		IntType:       "pagerduty",
		Config:        `{"routing_key":"super-secret-value-should-never-leak"}`,
		Enabled:       "true",
		CreatedAt:     "0",
	}

	summary := redactIntegration(in)
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(data)

	if strings.Contains(body, "super-secret-value-should-never-leak") {
		t.Fatalf("secret leaked into API response: %s", body)
	}
	if strings.Contains(body, "routing_key") {
		t.Fatalf("raw config key leaked into API response: %s", body)
	}
	if !strings.Contains(body, `"configured":true`) {
		t.Errorf("expected configured:true for a non-empty config, got: %s", body)
	}
}

func TestRedactIntegration_EmptyConfigReportsNotConfigured(t *testing.T) {
	for _, cfg := range []string{"", "{}"} {
		in := integrations.Integration{IntegrationID: "int_2", Config: cfg}
		if got := redactIntegration(in); got.Configured {
			t.Errorf("config %q: expected Configured=false, got true", cfg)
		}
	}
}
