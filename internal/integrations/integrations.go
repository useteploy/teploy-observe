package integrations

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type IntegrationService struct {
	db     *nucleus.Client
	logger *slog.Logger
	client *http.Client
}

func NewIntegrationService(db *nucleus.Client, logger *slog.Logger) *IntegrationService {
	return &IntegrationService{db: db, logger: logger, client: &http.Client{Timeout: 10 * time.Second}}
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
		 VALUES ($1, 'default', $2, $3, $4, $5, 'true', $6, $7)`,
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
		 FROM integrations WHERE site_id = $1 AND enabled = 'true' ORDER BY created_at DESC`, siteID)
}

func (s *IntegrationService) Delete(ctx context.Context, integrationID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO integrations (integration_id, tenant_id, site_id, name, int_type, config, enabled, created_at, version)
		 SELECT integration_id, tenant_id, site_id, name, int_type, config, 'false', created_at, $2
		 FROM integrations WHERE integration_id = $1`,
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

// Fire sends an alert to all enabled integrations for a site.
func (s *IntegrationService) Fire(ctx context.Context, siteID string, payload AlertPayload) {
	intgs, err := s.List(ctx, siteID)
	if err != nil {
		return
	}
	for _, intg := range intgs {
		go func(i Integration) {
			var err error
			switch i.IntType {
			case "jira":
				err = s.fireJira(i.Config, payload)
			case "github":
				err = s.fireGitHub(i.Config, payload)
			case "pagerduty":
				err = s.firePagerDuty(i.Config, payload)
			case "email":
				err = s.fireEmail(i.Config, payload)
			case "slack":
				err = s.fireSlack(i.Config, payload)
			}
			if err != nil {
				s.logger.Error("integration fire failed", "type", i.IntType, "name", i.Name, "err", err)
			}
		}(intg)
	}
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
	resp.Body.Close()
	return nil
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
	resp.Body.Close()
	return nil
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
	resp.Body.Close()
	return nil
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
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: [Observe Alert] %s\r\n\r\n%s\r\n\r\nMetric: %s = %s (threshold: %s)",
		cfg.From, cfg.To, p.Title, p.Message, p.Metric, p.Value, p.Threshold)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	return smtp.SendMail(cfg.SMTPHost+":"+port, auth, cfg.From, strings.Split(cfg.To, ","), []byte(msg))
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
	resp.Body.Close()
	return nil
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
