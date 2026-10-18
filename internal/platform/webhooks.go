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
	// Severities (O10): comma-delimited subset of the severity domain this
	// webhook wants (info, warning, critical, error). Empty = all - the
	// pre-056 behavior every existing webhook keeps.
	Severities string `json:"severities" db:"severities"`
	// Kinds (066, EV1): comma-delimited subset of the notification kind
	// domain this webhook wants. Empty = every kind that existed before
	// 066 (incident_*/cron_*); the issue_* kinds are OPT-IN, so an
	// existing webhook receives none of them until an operator lists
	// them. See MatchesKind.
	Kinds string `json:"kinds" db:"kinds"`
	// SecretReveal carries the signing secret back to the caller exactly once,
	// at creation. It has no db tag, so List/Get never populate it.
	//
	// SCAN NOTE: the SDK's scanner falls back to a field's json tag when
	// the db tag is absent, so this field maps to column "secret" and -
	// being the LATER field - silently overwrites Secret's mapping. Webhook
	// reads therefore scan into webhookScan (which has no reveal field) and
	// convert; never scan into Webhook directly. Before that fix, every
	// webhook POST left unsigned: the intent's Secret was empty, so the
	// HMAC headers were skipped (X-Observe-Signature, X-Observe-Timestamp)
	// while X-Observe-Delivery still identified the delivery.
	SecretReveal string    `json:"secret,omitempty"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	Version      string    `json:"-" db:"version"`
}

// webhookScan is the typed scan row for webhooks: the domain columns ONLY,
// no DTO-only field whose json tag could hijack a column mapping (see the
// SecretReveal note on Webhook).
type webhookScan struct {
	WebhookID   string `db:"webhook_id"`
	TenantID    string `db:"tenant_id"`
	SiteID      string `db:"site_id"`
	Name        string `db:"name"`
	WebhookType string `db:"webhook_type"`
	URL         string `db:"url"`
	Secret      string `db:"secret"`
	Severities  string `db:"severities"`
	Kinds       string `db:"kinds"`
	Enabled     string `db:"enabled"`
	CreatedAt   string `db:"created_at"`
	Version     string `db:"version"`
}

func (r webhookScan) toWebhook() Webhook {
	enabled := r.Enabled == "true"
	var createdAt time.Time
	if ms, err := strconv.ParseInt(r.CreatedAt, 10, 64); err == nil {
		createdAt = time.UnixMilli(ms).UTC()
	}
	return Webhook{
		WebhookID: r.WebhookID, TenantID: r.TenantID, SiteID: r.SiteID,
		Name: r.Name, WebhookType: r.WebhookType, URL: r.URL,
		Secret: r.Secret, Severities: r.Severities, Kinds: r.Kinds,
		Enabled: enabled, CreatedAt: createdAt, Version: r.Version,
	}
}

// SeverityDomain is the closed set a webhook's severities filter may name.
var SeverityDomain = map[string]bool{
	"info":     true,
	"warning":  true,
	"critical": true,
	"error":    true,
}

// KindDomain is the closed set a webhook's kinds filter may name: the
// notification kinds the outbox has ever delivered. Adding a kind here is a
// wire-contract change receivers can observe, so it is deliberate and rare.
var KindDomain = map[string]bool{
	NotifyIncidentOpened:    true,
	NotifyIncidentRecovered: true,
	NotifyIncidentRepeat:    true,
	NotifyCronMissed:        true,
	NotifyCronRecovered:     true,
	NotifyIssueNew:          true,
	NotifyIssueRegression:   true,
	NotifyIssueSpike:        true,
}

// IsIssueKind reports whether kind is one of the issue_* notification kinds
// (066). Those are opt-in per webhook: an unconfigured (empty) kinds filter
// never receives them.
func IsIssueKind(kind string) bool {
	return kind == NotifyIssueNew || kind == NotifyIssueRegression || kind == NotifyIssueSpike
}

// NormalizeSeverities validates and canonicalizes a comma-delimited
// severity filter: entries must be in the domain, output is lowercase,
// deduped, comma-joined. Empty input is valid (receives everything).
func NormalizeSeverities(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		s := strings.ToLower(strings.TrimSpace(part))
		if s == "" {
			continue
		}
		if !SeverityDomain[s] {
			return "", fmt.Errorf("unknown severity %q (domain: info, warning, critical, error)", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, ","), nil
}

// MatchesSeverity reports whether a webhook receives the given severity:
// an empty filter receives everything.
func MatchesSeverity(severities, severity string) bool {
	if severities == "" {
		return true
	}
	for _, part := range strings.Split(severities, ",") {
		if strings.TrimSpace(part) == severity {
			return true
		}
	}
	return false
}

// NormalizeKinds validates and canonicalizes a comma-delimited kind filter:
// entries must be in KindDomain, output is lowercase, deduped, comma-joined.
// Empty input is valid and means "the pre-066 kinds" (see MatchesKind).
func NormalizeKinds(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		k := strings.ToLower(strings.TrimSpace(part))
		if k == "" {
			continue
		}
		if !KindDomain[k] {
			return "", fmt.Errorf("unknown kind %q (domain: incident_opened, incident_recovered, incident_repeat, cron_missed, cron_recovered, issue_new, issue_regression, issue_spike)", k)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return strings.Join(out, ","), nil
}

// MatchesKind reports whether a webhook receives the given notification kind.
//
// An EMPTY filter receives only the kinds that existed before 066 - the
// pre-066 behavior every existing webhook keeps. The issue_* kinds are
// opt-in: a webhook that does not list them receives none of them, so issue
// traffic never arrives unannounced (and a Slack channel never suddenly
// receives a stack). A non-empty filter is exact: it governs every kind,
// including the pre-066 ones.
func MatchesKind(kinds, kind string) bool {
	if kinds == "" {
		return !IsIssueKind(kind)
	}
	for _, part := range strings.Split(kinds, ",") {
		if strings.TrimSpace(part) == kind {
			return true
		}
	}
	return false
}

func (s *WebhookService) Create(ctx context.Context, siteID, name, webhookType, url, severities, kinds string) (*Webhook, error) {
	if webhookType != "slack" && webhookType != "http" {
		webhookType = "http"
	}
	if err := netsafe.ValidateURL(url); err != nil {
		return nil, fmt.Errorf("webhook url: %w", err)
	}
	severities, err := NormalizeSeverities(severities)
	if err != nil {
		return nil, fmt.Errorf("webhook severities: %w", err)
	}
	normalizedKinds, err := NormalizeKinds(kinds)
	if err != nil {
		return nil, fmt.Errorf("webhook kinds: %w", err)
	}
	id := genID()
	secret := genID() + genID() // 32 random bytes, hex-encoded
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, severities, kinds, enabled, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, 'true', $9, $10)`,
		id, siteID, name, webhookType, url, secret, severities, normalizedKinds, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}

	nowMs, _ := strconv.ParseInt(now, 10, 64)
	return &Webhook{
		WebhookID: id, SiteID: siteID, Name: name,
		WebhookType: webhookType, URL: url, Enabled: true, Severities: severities, Kinds: normalizedKinds,
		Secret: secret, SecretReveal: secret,
		CreatedAt: time.UnixMilli(nowMs).UTC(),
	}, nil
}

func (s *WebhookService) List(ctx context.Context, siteID string) ([]Webhook, error) {
	// Scans through webhookScan (not Webhook): Webhook.SecretReveal's json
	// tag resolves to column "secret" in the SDK's scanner and would
	// hijack Secret's mapping, leaving every intent unsigned.
	rows, err := nucleus.Query[webhookScan](ctx, s.db.SQL(),
		`SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, severities, kinds, enabled, created_at, version
		 FROM `+webhooksLatest("site_id = $1")+`
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, err
	}
	out := make([]Webhook, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toWebhook())
	}
	return out, nil
}

func (s *WebhookService) Delete(ctx context.Context, webhookID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// Strictly-monotonic version (the 70f6eff version-tie defect): a
	// same-millisecond create+delete must not tie, or the tombstone loses
	// the collapse and the deleted webhook keeps receiving payloads.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO webhooks (webhook_id, tenant_id, site_id, name, webhook_type, url, secret, severities, kinds, enabled, created_at, version)
		 SELECT webhook_id, tenant_id, site_id, name, webhook_type, url, secret, severities, kinds, 'false', created_at,
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
