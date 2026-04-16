package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type WebhookService struct {
	db     *nucleus.Client
	logger *slog.Logger
	client *http.Client
}

func NewWebhookService(db *nucleus.Client, logger *slog.Logger) *WebhookService {
	return &WebhookService{
		db:     db,
		logger: logger,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

type Webhook struct {
	WebhookID   string    `json:"webhook_id" db:"webhook_id"`
	TenantID    string    `json:"-" db:"tenant_id"`
	SiteID      string    `json:"site_id" db:"site_id"`
	Name        string    `json:"name"`
	WebhookType string    `json:"webhook_type" db:"webhook_type"`
	URL         string    `json:"url"`
	Secret      string    `json:"-" db:"secret"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	Version     string    `json:"-" db:"version"`
}

func (s *WebhookService) Create(ctx context.Context, siteID, name, webhookType, url string) (*Webhook, error) {
	if webhookType != "slack" && webhookType != "http" {
		webhookType = "http"
	}
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, '', 'true', $6, $7)`,
		id, siteID, name, webhookType, url, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}

	nowMs, _ := strconv.ParseInt(now, 10, 64)
	return &Webhook{
		WebhookID: id, SiteID: siteID, Name: name,
		WebhookType: webhookType, URL: url, Enabled: true,
		CreatedAt: time.UnixMilli(nowMs).UTC(),
	}, nil
}

func (s *WebhookService) List(ctx context.Context, siteID string) ([]Webhook, error) {
	return nucleus.Query[Webhook](ctx, s.db.SQL(),
		`SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version
		 FROM webhooks WHERE site_id = $1 AND enabled = 'true' ORDER BY created_at DESC`, siteID)
}

func (s *WebhookService) Delete(ctx context.Context, webhookID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version)
		 SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, 'false', created_at, $2
		 FROM webhooks WHERE webhook_id = $1`,
		webhookID, now,
	)
	return err
}

// AlertPayload is sent to webhooks when an alert triggers.
type AlertPayload struct {
	AlertID   string  `json:"alert_id"`
	RuleName  string  `json:"rule_name"`
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold string  `json:"threshold"`
	SiteID    string  `json:"site_id"`
	Timestamp string  `json:"timestamp"`
}

// Fire sends an alert payload to all enabled webhooks for a site.
func (s *WebhookService) Fire(ctx context.Context, siteID string, payload AlertPayload) {
	hooks, err := s.List(ctx, siteID)
	if err != nil {
		s.logger.Error("webhook list failed", "err", err)
		return
	}

	for _, hook := range hooks {
		go func(h Webhook) {
			var err error
			switch h.WebhookType {
			case "slack":
				err = s.fireSlack(h.URL, payload)
			default:
				err = s.fireHTTP(h.URL, payload)
			}
			if err != nil {
				s.logger.Error("webhook fire failed", "webhook", h.Name, "type", h.WebhookType, "err", err)
			}
		}(hook)
	}
}

func (s *WebhookService) fireHTTP(url string, payload AlertPayload) error {
	body, _ := json.Marshal(payload)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func (s *WebhookService) fireSlack(url string, payload AlertPayload) error {
	text := fmt.Sprintf("*Alert: %s*\nMetric: %s = %.2f (threshold: %s)\nSite: %s",
		payload.RuleName, payload.Metric, payload.Value, payload.Threshold, payload.SiteID)
	slackPayload := map[string]string{"text": text}
	body, _ := json.Marshal(slackPayload)
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
