package main

import (
	"context"
	"fmt"
	"time"

	"github.com/useteploy/teploy-observe/internal/platform"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/integrations"
)

// wireIssueNotifications connects issue lifecycle events (a new issue's first
// occurrence, a resolved issue regressing, a burst spike) to the site's
// configured integrations AND to the durable signed notification outbox
// (EV1). internal/errors only knows the IssueNotifier callback type; the
// translation happens here so none of the three packages imports another.
// Both callbacks only enqueue (bounded, drop-when-full), so error ingest
// never waits on a notification.
func wireIssueNotifications(issueSvc *obserrors.IssueService, integrationSvc *integrations.IntegrationService, publicURL string, emitter *platform.IssueAlertEmitter) *integrations.IssueDispatcher {
	d := integrationSvc.NewIssueDispatcher(publicURL)
	issueSvc.SetNotifier(func(_ context.Context, ev obserrors.IssueEvent) {
		// The integrations dispatcher keeps exactly the two kinds it has
		// always understood; a spike would render as a misleading "New
		// issue" in its payload (issue_dispatch.go labels every non-
		// regression kind that way).
		switch ev.Kind {
		case obserrors.IssueEventNew, obserrors.IssueEventRegression:
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
		}
		// The outbox emitter takes all three issue kinds (opt-in per
		// webhook through the `kinds` filter).
		if kind := issueOutboxKind(ev.Kind); kind != "" {
			emitter.Notify(platform.IssueAlertEvent{
				Kind: kind, WindowCount: ev.WindowCount,
				IssueID: ev.IssueID, SiteID: ev.SiteID, Fingerprint: ev.GroupHash,
				Title: ev.Title, Culprit: ev.Culprit, Level: ev.Level, Release: ev.Release,
				Environment: ev.Environment, SampleStack: ev.SampleStack, At: ev.At,
			})
		}
	})
	return d
}

// issueOutboxKind maps an internal lifecycle kind onto its outbox kind.
func issueOutboxKind(kind string) string {
	switch kind {
	case obserrors.IssueEventNew:
		return platform.NotifyIssueNew
	case obserrors.IssueEventRegression:
		return platform.NotifyIssueRegression
	case obserrors.IssueEventSpike:
		return platform.NotifyIssueSpike
	}
	return ""
}

// issueSnapshotReader adapts IssueService.GetIssue to the emitter's re-read
// seam: the emitter re-reads the issue after its settle delay so the
// payload's event_count / last_seen / user_count are the real, post-insert
// numbers rather than the racey ingest-time cache.
func issueSnapshotReader(svc *obserrors.IssueService) func(context.Context, string, string) (*platform.IssueSnapshot, error) {
	return func(ctx context.Context, siteID, issueID string) (*platform.IssueSnapshot, error) {
		iss, err := svc.GetIssue(ctx, issueID, siteID)
		if err != nil {
			return nil, err
		}
		if iss == nil {
			return nil, fmt.Errorf("issue %s not found", issueID)
		}
		return &platform.IssueSnapshot{
			IssueID:            iss.IssueID,
			SiteID:             iss.SiteID,
			GroupHash:          iss.GroupHash,
			Title:              iss.Title,
			Culprit:            iss.Culprit,
			Level:              iss.Level,
			Status:             iss.Status,
			FirstSeen:          iss.FirstSeen,
			LastSeen:           iss.LastSeen,
			EventCount:         iss.EventCount,
			UserCount:          iss.UserCount,
			RegressionCount:    iss.RegressionCount,
			ReleaseTag:         iss.ReleaseTag,
			FingerprintVersion: iss.FingerprintVersion,
		}, nil
	}
}

// shutdownIssueNotifications drains queued notifications on shutdown, bounded
// so a hung receiver cannot hold the process open.
func shutdownIssueNotifications(d *integrations.IssueDispatcher) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return d.Shutdown(ctx)
}
