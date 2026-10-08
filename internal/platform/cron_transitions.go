package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/incidents"
)

// EnsureCronMissed commits the incident, timeline and all target intents in one
// transaction. Repeated checks reconcile older partial admissions per target
// without replacing an existing delivered, pending or suppressed intent.
func (s *AlertService) EnsureCronMissed(ctx context.Context, cron CronMonitorRef, graceSecs int) (incidents.Incident, bool, error) {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	hooks, err := s.listHooks(ctx, cron.SiteID, "warning")
	if err != nil {
		return incidents.Incident{}, false, err
	}
	var inc incidents.Incident
	created := false
	err = s.incidents.Transaction(ctx, func(sql *nucleus.SQLModel) error {
		active, err := s.incidents.ActiveByRule(ctx, "cron:"+cron.CronID)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			inc = active[0]
		} else {
			inc, err = s.incidents.CreateTx(ctx, sql, incidents.CreateInput{
				SiteID: cron.SiteID, Title: "Cron missed: " + cron.Name,
				Description: fmt.Sprintf("cron %q (slug %q) missed its next scheduled run plus %ds grace", cron.Name, cron.Slug, graceSecs),
				Severity:    "warning", Source: incidents.SourceCron, RuleID: "cron:" + cron.CronID,
			}, "cron")
			if err != nil {
				return err
			}
			created = true
			if err := s.incidents.RecordEventTx(ctx, sql, "opened-"+inc.IncidentID, inc.IncidentID, inc.StartedAt, incidents.EventOpened, "cron", "cron heartbeat missed"); err != nil {
				return err
			}
		}
		return s.enqueueCronTx(ctx, sql, NotifyCronMissed, cron, inc, graceSecs, hooks)
	})
	if err != nil {
		return incidents.Incident{}, false, err
	}
	return inc, created, nil
}

// RecoverCron commits every active incident's close, timeline and correlated
// recovery intents together. An admission failure leaves incidents open for
// the next check-in; repeat check-ins after success create no new intents.
func (s *AlertService) RecoverCron(ctx context.Context, cron CronMonitorRef, graceSecs int) (int, error) {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	hooks, err := s.listHooks(ctx, cron.SiteID, "warning")
	if err != nil {
		return 0, err
	}
	count := 0
	err = s.incidents.Transaction(ctx, func(sql *nucleus.SQLModel) error {
		active, err := s.incidents.ActiveByRule(ctx, "cron:"+cron.CronID)
		if err != nil {
			return err
		}
		for _, inc := range active {
			if err := s.incidents.CloseTx(ctx, sql, inc.IncidentID); err != nil {
				return err
			}
			if err := s.incidents.RecordEventTx(ctx, sql, "recovered-"+inc.IncidentID, inc.IncidentID, s.now().UnixMilli(), incidents.EventRecovered, "cron", "cron heartbeat recovered"); err != nil {
				return err
			}
			if err := s.enqueueCronTx(ctx, sql, NotifyCronRecovered, cron, inc, graceSecs, hooks); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *AlertService) enqueueCronTx(ctx context.Context, sql *nucleus.SQLModel, kind string, cron CronMonitorRef, inc incidents.Incident, graceSecs int, hooks []Webhook) error {
	payload := BuildCronPayload(kind, cron, inc.IncidentID, graceSecs, s.now())
	for _, hook := range hooks {
		// Include legacy randomly-minted intents in the existence check so upgrades
		// also reconcile partial target sets without resending admitted destinations.
		existing, err := nucleus.Query[struct {
			ID string `db:"id"`
		}](ctx, sql,
			`SELECT id FROM (`+notifyCollapseSelect("incident_id = $1 AND kind = $2 AND webhook_id = $3")+`) LIMIT 1`, inc.IncidentID, kind, hook.WebhookID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			continue
		}
		sum := sha256.Sum256([]byte(kind + "\x00" + inc.IncidentID + "\x00" + hook.WebhookID))
		if _, err := s.notifier.enqueueTx(ctx, sql, NotificationIntent{
			ID: "nfy-" + hex.EncodeToString(sum[:]), Kind: kind, RuleID: "cron:" + cron.CronID,
			IncidentID: inc.IncidentID, SiteID: cron.SiteID, WebhookID: hook.WebhookID,
			TargetType: hook.WebhookType, TargetURL: hook.URL, Secret: hook.Secret, Payload: payload,
		}); err != nil {
			return err
		}
	}
	return nil
}
