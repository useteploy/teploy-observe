package main

import (
	"context"
	"time"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/integrations"
)

// wireIssueNotifications connects issue lifecycle events (a new issue's first
// occurrence, a resolved issue regressing) to the site's configured
// integrations. internal/errors only knows the IssueNotifier callback type;
// the translation to the integrations package happens here so neither package
// imports the other. The callback only enqueues (bounded, drop-oldest), so
// error ingest never waits on a notification.
func wireIssueNotifications(issueSvc *obserrors.IssueService, integrationSvc *integrations.IntegrationService, publicURL string) *integrations.IssueDispatcher {
	d := integrationSvc.NewIssueDispatcher(publicURL)
	issueSvc.SetNotifier(func(_ context.Context, ev obserrors.IssueEvent) {
		d.Notify(integrations.IssueEvent{
			Kind:       ev.Kind,
			SiteID:     ev.SiteID,
			IssueID:    ev.IssueID,
			Title:      ev.Title,
			Culprit:    ev.Culprit,
			Level:      ev.Level,
			Release:    ev.Release,
			EventCount: ev.EventCount,
		})
	})
	return d
}

// shutdownIssueNotifications drains queued notifications on shutdown, bounded
// so a hung receiver cannot hold the process open.
func shutdownIssueNotifications(d *integrations.IssueDispatcher) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return d.Shutdown(ctx)
}
