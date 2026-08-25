package integrations

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/mailx"
	"github.com/useteploy/teploy-observe/internal/netsafe"
)

type IntegrationService struct {
	db     *nucleus.Client
	logger *slog.Logger
	client *http.Client
}

func NewIntegrationService(db *nucleus.Client, logger *slog.Logger) *IntegrationService {
	// netsafe client blocks dialing private/loopback/metadata IPs (SSRF) for
	// every outbound integration (Jira/Slack/webhook/etc.) at dial time.
	return &IntegrationService{db: db, logger: logger, client: netsafe.Client(10 * time.Second)}
}

type Integration struct {
	IntegrationID string `json:"integration_id" db:"integration_id"`
	TenantID      string `json:"-" db:"tenant_id"`
	SiteID        string `json:"site_id" db:"site_id"`
	Name          string `json:"name" db:"name"`
	IntType       string `json:"type" db:"int_type"`
	Config        string `json:"config" db:"config"`
	Enabled       string `json:"enabled" db:"enabled"`
	CreatedAt     string `json:"created_at" db:"created_at"`
	Version       string `json:"-" db:"version"`
}

// IntegrationConfig is the parsed config for each type.
type JiraConfig struct {
	BaseURL  string `json:"base_url"`
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
	Project  string `json:"project"`
}

type GitHubConfig struct {
	Token string `json:"token"`
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

type PagerDutyConfig struct {
	RoutingKey string `json:"routing_key"`
}

type EmailConfig struct {
	SMTPHost string `json:"smtp_host"`
	SMTPPort string `json:"smtp_port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	To       string `json:"to"`
}

func (s *IntegrationService) Create(ctx context.Context, siteID, name, intType, config string) (*Integration, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO integrations (integration_id, tenant_id, site_id, name, int_type, config, enabled, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, NULLIF($5, ''), 'true', $6, $7)`,
		id, siteID, name, intType, config, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create integration: %w", err)
	}
	return &Integration{IntegrationID: id, SiteID: siteID, Name: name, IntType: intType, Config: config, Enabled: "true", CreatedAt: now}, nil
}

func (s *IntegrationService) List(ctx context.Context, siteID string) ([]Integration, error) {
	return nucleus.Query[Integration](ctx, s.db.SQL(),
		`SELECT integration_id, tenant_id, site_id, name, int_type, COALESCE(config, '') AS config, enabled, created_at, version
		 FROM `+integrationsLatest("site_id = $1")+`
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
}

func (s *IntegrationService) Delete(ctx context.Context, integrationID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO integrations (integration_id, tenant_id, site_id, name, int_type, config, enabled, created_at, version)
		 SELECT integration_id, tenant_id, site_id, name, int_type, NULLIF(CAST(config AS TEXT), ''), 'false', created_at, $2
		 FROM `+integrationsLatest("integration_id = $1"),
		integrationID, now)
	return err
}

// AlertPayload is sent to integrations when an alert fires.
type AlertPayload struct {
	Title     string `json:"title"`
	Message   string `json:"message"`
	Severity  string `json:"severity"`
	SiteID    string `json:"site_id"`
	RuleName  string `json:"rule_name"`
	Metric    string `json:"metric"`
	Value     string `json:"value"`
	Threshold string `json:"threshold"`
	URL       string `json:"url"`
}

// Fire sends an alert to all enabled integrations for a site. Each attempt
// is recorded in integration_deliveries so users can inspect history and replay.
func (s *IntegrationService) Fire(ctx context.Context, siteID string, payload AlertPayload) {
	intgs, err := s.List(ctx, siteID)
	if err != nil {
		return
	}
	for _, intg := range intgs {
		go func(i Integration) {
			s.fireAndRecord(context.Background(), i, payload, false, false)
		}(intg)
	}
}

// fireAndRecord runs fireOne and writes a delivery row regardless of outcome.
func (s *IntegrationService) fireAndRecord(ctx context.Context, i Integration, payload AlertPayload, isTest, isReplay bool) error {
	start := time.Now()
	err := s.fireOne(i, payload)
	dur := time.Since(start).Milliseconds()
	status := "ok"
	errMsg := ""
	if err != nil {
		status = "failed"
		errMsg = err.Error()
		s.logger.Error("integration fire failed", "type", i.IntType, "name", i.Name, "err", err)
	}
	body, _ := json.Marshal(payload)
	_, recErr := s.db.SQL().Exec(ctx,
		`INSERT INTO integration_deliveries
			(delivery_id, tenant_id, integration_id, site_id, payload, status, error_message, duration_ms, created_at, is_test, is_replay)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		genID(), i.IntegrationID, i.SiteID, string(body), status, errMsg, dur,
		strconv.FormatInt(time.Now().UTC().UnixMilli(), 10),
		boolStr(isTest), boolStr(isReplay),
	)
	if recErr != nil {
		s.logger.Warn("integration delivery record failed", "err", recErr)
	}
	return err
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// fireOne dispatches to the correct per-type fire fn.
func (s *IntegrationService) fireOne(i Integration, payload AlertPayload) error {
	switch i.IntType {
	case "jira":
		return s.fireJira(i.Config, payload)
	case "github":
		return s.fireGitHub(i.Config, payload)
	case "pagerduty":
		return s.firePagerDuty(i.Config, payload)
	case "email":
		return s.fireEmail(i.Config, payload)
	case "slack":
		return s.fireSlack(i.Config, payload)
	}
	return fmt.Errorf("unknown integration type: %s", i.IntType)
}

// Test delivers a canned sample payload through the integration so users can
// verify wiring. Returns an error string for the UI.
func (s *IntegrationService) Test(ctx context.Context, integrationID string) error {
	rows, err := nucleus.Query[Integration](ctx, s.db.SQL(),
		`SELECT integration_id, tenant_id, site_id, name, int_type,
		        COALESCE(config, '') AS config, enabled, created_at, version
		 FROM `+integrationsLatest("integration_id = $1")+`
		 WHERE enabled = 'true'`, integrationID)
	if err != nil {
		return fmt.Errorf("lookup integration: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("integration not found")
	}
	sample := AlertPayload{
		Title:     "Observe test alert",
		Message:   "This is a test delivery from Observe. No action required.",
		Severity:  "info",
		SiteID:    rows[0].SiteID,
		RuleName:  "integration_test",
		Metric:    "pageviews",
		Value:     "42",
		Threshold: "0",
		URL:       "",
	}
	return s.fireAndRecord(ctx, rows[0], sample, true, false)
}

// Delivery is one historical attempt to deliver a payload through an integration.
type Delivery struct {
	DeliveryID    string `json:"delivery_id" db:"delivery_id"`
	IntegrationID string `json:"integration_id" db:"integration_id"`
	SiteID        string `json:"site_id" db:"site_id"`
	Payload       string `json:"payload" db:"payload"`
	Status        string `json:"status" db:"status"`
	ErrorMessage  string `json:"error_message" db:"error_message"`
	DurationMs    int64  `json:"duration_ms" db:"duration_ms"`
	CreatedAt     int64  `json:"created_at" db:"created_at"`
	IsTest        string `json:"is_test" db:"is_test"`
	IsReplay      string `json:"is_replay" db:"is_replay"`
}

// ListDeliveries returns the most recent delivery attempts for an integration.
func (s *IntegrationService) ListDeliveries(ctx context.Context, integrationID string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	q := fmt.Sprintf(
		`SELECT delivery_id, integration_id, site_id, payload, status,
		        COALESCE(error_message, '') AS error_message,
		        CAST(duration_ms AS BIGINT) AS duration_ms,
		        CAST(created_at AS BIGINT) AS created_at,
		        is_test, is_replay
		 FROM integration_deliveries
		 WHERE integration_id = $1
		 ORDER BY created_at DESC
		 LIMIT %d`, limit)
	return nucleus.Query[Delivery](ctx, s.db.SQL(), q, integrationID)
}

// Replay re-fires the payload from a prior delivery through the same integration.
func (s *IntegrationService) Replay(ctx context.Context, deliveryID string) error {
	rows, err := nucleus.Query[Delivery](ctx, s.db.SQL(),
		`SELECT delivery_id, integration_id, site_id, payload, status,
		        COALESCE(error_message, '') AS error_message,
		        CAST(duration_ms AS BIGINT) AS duration_ms,
		        CAST(created_at AS BIGINT) AS created_at,
		        is_test, is_replay
		 FROM integration_deliveries WHERE delivery_id = $1 LIMIT 1`, deliveryID)
	if err != nil {
		return fmt.Errorf("lookup delivery: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("delivery not found")
	}
	intgs, err := nucleus.Query[Integration](ctx, s.db.SQL(),
		`SELECT integration_id, tenant_id, site_id, name, int_type,
		        COALESCE(config, '') AS config, enabled, created_at, version
		 FROM `+integrationsLatest("integration_id = $1")+`
		 WHERE enabled = 'true'`, rows[0].IntegrationID)
	if err != nil {
		return fmt.Errorf("lookup integration: %w", err)
	}
	if len(intgs) == 0 {
		return fmt.Errorf("integration not found")
	}
	var payload AlertPayload
	if err := json.Unmarshal([]byte(rows[0].Payload), &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	return s.fireAndRecord(ctx, intgs[0], payload, false, true)
}

// checkResp closes resp and returns an error when the remote signalled failure
// (>=300), so fireAndRecord records status='failed' and a useful message
// instead of silently 'ok'. Reads a bounded body snippet for the message.
func checkResp(provider string, resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: status %d: %s", provider, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func (s *IntegrationService) fireJira(configJSON string, p AlertPayload) error {
	var cfg JiraConfig
	json.Unmarshal([]byte(configJSON), &cfg)
	if cfg.BaseURL == "" || cfg.Project == "" {
		return fmt.Errorf("jira: missing base_url or project")
	}
	body, _ := json.Marshal(map[string]any{
		"fields": map[string]any{
			"project":     map[string]string{"key": cfg.Project},
			"summary":     p.Title,
			"description": p.Message,
			"issuetype":   map[string]string{"name": "Bug"},
		},
	})
	req, _ := http.NewRequest("POST", cfg.BaseURL+"/rest/api/2/issue", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(cfg.Email, cfg.APIToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	return checkResp("jira", resp)
}

func (s *IntegrationService) fireGitHub(configJSON string, p AlertPayload) error {
	var cfg GitHubConfig
	json.Unmarshal([]byte(configJSON), &cfg)
	if cfg.Owner == "" || cfg.Repo == "" || cfg.Token == "" {
		return fmt.Errorf("github: missing owner, repo, or token")
	}
	body, _ := json.Marshal(map[string]any{
		"title": p.Title,
		"body":  p.Message,
		"labels": []string{"observe-alert"},
	})
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", cfg.Owner, cfg.Repo)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	return checkResp("github", resp)
}

func (s *IntegrationService) firePagerDuty(configJSON string, p AlertPayload) error {
	var cfg PagerDutyConfig
	json.Unmarshal([]byte(configJSON), &cfg)
	if cfg.RoutingKey == "" {
		return fmt.Errorf("pagerduty: missing routing_key")
	}
	body, _ := json.Marshal(map[string]any{
		"routing_key":  cfg.RoutingKey,
		"event_action": "trigger",
		"payload": map[string]any{
			"summary":  p.Title,
			"severity": p.Severity,
			"source":   "teploy-observe",
			"custom_details": map[string]string{
				"metric": p.Metric, "value": p.Value, "threshold": p.Threshold,
			},
		},
	})
	req, _ := http.NewRequest("POST", "https://events.pagerduty.com/v2/enqueue", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	return checkResp("pagerduty", resp)
}

func (s *IntegrationService) fireEmail(configJSON string, p AlertPayload) error {
	var cfg EmailConfig
	json.Unmarshal([]byte(configJSON), &cfg)
	if cfg.SMTPHost == "" || cfg.To == "" {
		return fmt.Errorf("email: missing smtp_host or to")
	}
	port := cfg.SMTPPort
	if port == "" {
		port = "587"
	}
	// Guard against SMTP header injection via From/To/Subject.
	if strings.ContainsAny(cfg.From, "\r\n") || strings.ContainsAny(p.Title, "\r\n") {
		return fmt.Errorf("email: invalid character in header")
	}
	recipients, err := parseRecipients(cfg.To)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: [Observe Alert] %s\r\n\r\n%s\r\n\r\nMetric: %s = %s (threshold: %s)",
		cfg.From, strings.Join(recipients, ", "), p.Title, p.Message, p.Metric, p.Value, p.Threshold)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	// Timeout-bounded send so a hung relay can't block the alert dispatch path.
	return mailx.SendMail(cfg.SMTPHost+":"+port, cfg.SMTPHost, auth, cfg.From, recipients, []byte(msg), 0)
}

// parseRecipients splits and validates a comma-separated recipient list,
// rejecting CR/LF (header injection) and malformed addresses.
func parseRecipients(to string) ([]string, error) {
	var out []string
	for _, r := range strings.Split(to, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if strings.ContainsAny(r, "\r\n") {
			return nil, fmt.Errorf("email: invalid character in recipient")
		}
		addr, err := mail.ParseAddress(r)
		if err != nil {
			return nil, fmt.Errorf("email: invalid recipient %q: %w", r, err)
		}
		out = append(out, addr.Address)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("email: no valid recipients")
	}
	return out, nil
}

func (s *IntegrationService) fireSlack(configJSON string, p AlertPayload) error {
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	json.Unmarshal([]byte(configJSON), &cfg)
	if cfg.WebhookURL == "" {
		return fmt.Errorf("slack: missing webhook_url")
	}
	text := fmt.Sprintf("*%s*\n%s\nMetric: %s = %s (threshold: %s)", p.Title, p.Message, p.Metric, p.Value, p.Threshold)
	body, _ := json.Marshal(map[string]string{"text": text})
	resp, err := s.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return checkResp("slack", resp)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
