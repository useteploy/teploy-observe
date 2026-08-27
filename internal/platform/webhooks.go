package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/netsafe"
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
		// Webhook delivery is the ONE fetch path with an operator who has
		// declared where their own services live. Empty by default, in which
		// case this is exactly netsafe.Client. See netsafe.Allow for why the
		// allowance is CIDRs and why link-local is never allowlistable.
		client: netsafe.ClientWithAllow(10*time.Second, webhookAllow()),
	}
}

type Webhook struct {
	WebhookID   string `json:"webhook_id" db:"webhook_id"`
	TenantID    string `json:"-" db:"tenant_id"`
	SiteID      string `json:"site_id" db:"site_id"`
	Name        string `json:"name"`
	WebhookType string `json:"webhook_type" db:"webhook_type"`
	URL         string `json:"url"`
	Secret      string `json:"-" db:"secret"`
	// SecretReveal carries the signing secret back to the caller exactly once,
	// at creation. It has no db tag, so List/Get never populate it.
	SecretReveal string    `json:"secret,omitempty"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	Version      string    `json:"-" db:"version"`
}

func (s *WebhookService) Create(ctx context.Context, siteID, name, webhookType, url string) (*Webhook, error) {
	if webhookType != "slack" && webhookType != "http" {
		webhookType = "http"
	}
	if err := netsafe.ValidateURL(url); err != nil {
		return nil, fmt.Errorf("webhook url: %w", err)
	}
	id := genID()
	secret := genID() + genID() // 32 random bytes, hex-encoded
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'true', $7, $8)`,
		id, siteID, name, webhookType, url, secret, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}

	nowMs, _ := strconv.ParseInt(now, 10, 64)
	return &Webhook{
		WebhookID: id, SiteID: siteID, Name: name,
		WebhookType: webhookType, URL: url, Enabled: true,
		Secret: secret, SecretReveal: secret,
		CreatedAt: time.UnixMilli(nowMs).UTC(),
	}, nil
}

func (s *WebhookService) List(ctx context.Context, siteID string) ([]Webhook, error) {
	return nucleus.Query[Webhook](ctx, s.db.SQL(),
		`SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version
		 FROM `+webhooksLatest("site_id = $1")+`
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
}

func (s *WebhookService) Delete(ctx context.Context, webhookID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version)
		 SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, 'false', created_at, $2
		 FROM `+webhooksLatest("webhook_id = $1"),
		webhookID, now,
	)
	return err
}

// AlertPayload is sent to webhooks when an alert triggers.
type AlertPayload struct {
	AlertID string `json:"alert_id"`
	// RuleID identifies the RULE, where AlertID identifies one firing of it.
	// A rule that keeps breaching fires once per cooldown and each firing gets
	// a fresh alert_id, so a receiver that opens work items keyed on alert_id
	// opens a new one every cooldown for a single ongoing incident. rule_id is
	// the stable key that lets it collapse them.
	RuleID    string  `json:"rule_id"`
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
				err = s.fireHTTP(h.URL, h.Secret, payload)
			}
			if err != nil {
				s.logger.Error("webhook fire failed", "webhook", h.Name, "type", h.WebhookType, "err", err)
			}
		}(hook)
	}
}

func (s *WebhookService) fireHTTP(url, secret string, payload AlertPayload) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// A delivery id, unique per attempt. Receivers dedupe on it so a resend
	// cannot re-run whatever the first delivery started; without one they have
	// to fall back to hashing the body, which does not distinguish a resend
	// from a genuinely repeated alert. Set unconditionally — an unsigned
	// webhook needs replay protection at least as much as a signed one.
	req.Header.Set("X-Observe-Delivery", genID())
	// Sign so the receiver can verify authenticity:
	//   X-Observe-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))
	if secret != "" {
		ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
		mac.Write(body)
		req.Header.Set("X-Observe-Timestamp", ts)
		req.Header.Set("X-Observe-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.client.Do(req)
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
	// Slack returns 4xx (e.g. invalid_payload, channel_not_found) with a 200-less
	// status; treat any >=400 as a delivery failure so it's logged, not swallowed.
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}
	return nil
}

// webhookAllow reads OBSERVE_WEBHOOK_ALLOW_CIDRS.
//
// Self-hosted Teploy runs entirely on a tailnet (100.64.0.0/10), which the SSRF
// guard blocks by design — so without this an alert could never reach a
// self-hosted receiver, and the first person to hit it would be tempted to
// weaken the guard itself. A malformed value is logged and IGNORED rather than
// silently treated as empty: an operator who mistyped a CIDR should not
// discover it as a delivery that never arrives.
func webhookAllow() netsafe.Allow {
	raw := strings.TrimSpace(os.Getenv("OBSERVE_WEBHOOK_ALLOW_CIDRS"))
	if raw == "" {
		return nil
	}
	allow, err := netsafe.ParseAllow(raw)
	if err != nil {
		slog.Error("OBSERVE_WEBHOOK_ALLOW_CIDRS is malformed and was ignored; webhook delivery to private addresses stays blocked",
			"error", err)
		return nil
	}
	slog.Info("webhook delivery may reach operator-declared private networks", "cidrs", raw)
	return allow
}
