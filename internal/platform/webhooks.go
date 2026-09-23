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
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/netsafe"
)

type WebhookService struct {
	db     *nucleus.Client
	logger *slog.Logger
	client *http.Client

	// R22 (round 4): delivery runs on lifecycle-owned bounded workers with a
	// bounded queue instead of one unowned goroutine per hook — an alert
	// burst can no longer spawn unbounded goroutines, and Shutdown drains
	// what is queued. Each firing keeps ONE stable delivery id across its
	// attempts (and gets a bounded retry), so a receiver can dedupe a resent
	// alert. Durable across-restart delivery landed for alerts in O10 (the
	// notification_outbox, notify_outbox.go); this in-memory path remains
	// for immediate fire-and-forget sends that opt out of durability.
	deliverMu  sync.Mutex
	deliverQ   []deliveryJob
	deliverWg  sync.WaitGroup
	deliverSem chan struct{}
	stopped    bool
}

type deliveryJob struct {
	id      string // stable logical delivery id, reused across attempts
	hook    Webhook
	payload AlertPayload
}

const (
	maxQueuedDeliveries = 1000
	deliveryWorkers     = 4
	maxDeliveryAttempts = 3
)

func NewWebhookService(db *nucleus.Client, logger *slog.Logger) *WebhookService {
	return &WebhookService{
		db:     db,
		logger: logger,
		// Webhook delivery is the ONE fetch path with an operator who has
		// declared where their own services live. Empty by default, in which
		// case this is exactly netsafe.Client. See netsafe.Allow for why the
		// allowance is CIDRs and why link-local is never allowlistable.
		client:     netsafe.ClientWithAllow(10*time.Second, webhookAllow()),
		deliverSem: make(chan struct{}, deliveryWorkers),
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
	// Strictly-monotonic version (the 70f6eff version-tie defect): a
	// same-millisecond create+delete must not tie, or the tombstone loses
	// the collapse and the deleted webhook keeps receiving payloads.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, enabled, created_at, version)
		 SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, 'false', created_at,
		        GREATEST(CAST($2 AS BIGINT), version + 1)
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

// Fire enqueues an alert payload for delivery to all enabled webhooks for a
// site (R22, round 4). Delivery happens on the owned worker set: bounded
// queue (drop-newest-oldest-first refused — a full queue drops the NEWEST
// job with a loud log, never silently), bounded workers, one stable delivery
// id per firing reused across retries, and Shutdown waits for the drain.
func (s *WebhookService) Fire(ctx context.Context, siteID string, payload AlertPayload) {
	hooks, err := s.List(ctx, siteID)
	if err != nil {
		s.logger.Error("webhook list failed", "err", err)
		return
	}

	for _, hook := range hooks {
		// One logical delivery id per (firing, hook): receivers dedupe on it
		// so a retry cannot re-run what the first delivery started.
		job := deliveryJob{id: genID(), hook: hook, payload: payload}
		s.deliverMu.Lock()
		if s.stopped {
			s.deliverMu.Unlock()
			s.logger.Error("webhook delivery refused — service stopped", "webhook", hook.Name)
			return
		}
		if len(s.deliverQ) >= maxQueuedDeliveries {
			// The queue is full: the OLDEST entries have had their chance;
			// dropping the newest would silently swallow the current alert.
			// Drop from the front and log loudly.
			old := s.deliverQ[0]
			s.deliverQ = s.deliverQ[1:]
			s.logger.Error("webhook delivery queue overflow — dropped oldest queued delivery",
				"webhook", old.hook.Name, "delivery_id", old.id, "alert", old.payload.AlertID)
		}
		s.deliverQ = append(s.deliverQ, job)
		s.deliverMu.Unlock()
		select {
		case s.deliverSem <- struct{}{}:
			s.deliverWg.Add(1)
			go s.deliveryWorker()
		default:
			// Enough workers are already draining; they will pick this up.
		}
	}
}

// deliveryWorker drains the queue until empty, then releases its slot.
func (s *WebhookService) deliveryWorker() {
	defer s.deliverWg.Done()
	defer func() { <-s.deliverSem }()
	for {
		s.deliverMu.Lock()
		if len(s.deliverQ) == 0 {
			s.deliverMu.Unlock()
			return
		}
		job := s.deliverQ[0]
		s.deliverQ = s.deliverQ[1:]
		s.deliverMu.Unlock()
		s.attemptDelivery(job)
	}
}

// attemptDelivery sends one job with a bounded retry; the delivery id is
// stable across attempts.
func (s *WebhookService) attemptDelivery(job deliveryJob) {
	var lastErr error
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		var err error
		switch job.hook.WebhookType {
		case "slack":
			err = s.fireSlack(job.hook.URL, job.payload)
		default:
			err = s.fireHTTP(job.id, job.hook.URL, job.hook.Secret, job.payload)
		}
		if err == nil {
			return
		}
		lastErr = err
		// Bounded backoff between attempts; a full sleep is acceptable on a
		// worker goroutine, and shutdown does not wait on the backoff tail
		// beyond the current attempt.
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	s.logger.Error("webhook fire failed",
		"webhook", job.hook.Name, "type", job.hook.WebhookType,
		"delivery_id", job.id, "attempts", maxDeliveryAttempts, "err", lastErr)
}

// Shutdown stops admission and waits for in-flight deliveries (R22).
func (s *WebhookService) Shutdown() {
	s.deliverMu.Lock()
	s.stopped = true
	s.deliverMu.Unlock()
	s.deliverWg.Wait()
}

func (s *WebhookService) fireHTTP(deliveryID, url, secret string, payload AlertPayload) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The delivery id is the LOGICAL delivery identity (R22): stable across
	// retries of the same firing, fresh per firing. Receivers dedupe on it so
	// a resend cannot re-run whatever the first delivery started; without one
	// they have to fall back to hashing the body, which does not distinguish
	// a resend from a genuinely repeated alert. Set unconditionally — an
	// unsigned webhook needs replay protection at least as much as a signed
	// one.
	req.Header.Set("X-Observe-Delivery", deliveryID)
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
