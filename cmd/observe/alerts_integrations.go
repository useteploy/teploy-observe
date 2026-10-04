package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/useteploy/teploy-observe/internal/integrations"
	"github.com/useteploy/teploy-observe/internal/platform"
)

// wireAlertsToIntegrations routes alert FIRE events (the opening edge only)
// to the site's integrations through IntegrationService.Fire, which records
// every attempt in integration_deliveries. Default off:
// OBSERVE_ALERTS_TO_INTEGRATIONS=true opts in globally. RECOVER and repeat
// are deliberately NOT routed - the integration senders are create-only
// (PagerDuty always "trigger", Jira/GitHub always open a new issue), so a
// recovery would page or file a second ticket instead of resolving.
func wireAlertsToIntegrations(alerts *platform.AlertService, svc *integrations.IntegrationService, logger *slog.Logger) {
	if os.Getenv("OBSERVE_ALERTS_TO_INTEGRATIONS") != "true" {
		return
	}
	alerts.SetIntegrationsHook(alertFireToIntegrations(svc.Fire))
	logger.Info("alert fire events routed to site integrations (fire-only)")
}

// alertFireToIntegrations adapts the engine event to the integrations payload.
func alertFireToIntegrations(fire func(ctx context.Context, siteID string, p integrations.AlertPayload)) func(ctx context.Context, ev platform.AlertFireEvent) {
	return func(ctx context.Context, ev platform.AlertFireEvent) {
		fire(ctx, ev.SiteID, integrations.AlertPayload{
			Title: ev.Title, Message: ev.Message, Severity: ev.Severity,
			SiteID: ev.SiteID, RuleName: ev.RuleName, Metric: ev.Metric,
			Value: ev.Value, Threshold: ev.Threshold,
		})
	}
}
