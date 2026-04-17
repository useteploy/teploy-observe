// Package aiquery implements the natural-language -> SQL assistant for
// Observe's explorer. The admin configures provider / endpoint / model /
// API key once at /settings/ai; every explorer user can then hit
// POST /api/v1/ai/query with a question and receive a SQL draft.
//
// Safety is layered:
//   1. Admin-supplied API key: Observe never sees user credentials.
//   2. LLM output is post-processed by explorer.classifyReadOnlySQL so
//      write statements are rejected even if the model drafts one.
//   3. Rate limit applies per user (enforced at the HTTP layer).
package aiquery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/observe/internal/dbutil"
)

const (
	settingsKey = "ai_query_config"
	defaultURL  = "https://api.openai.com/v1/chat/completions"
)

// Config holds the admin-supplied LLM connection settings. Provider is
// informational ("openai", "anthropic", "ollama", "compatible"); all
// providers that speak the OpenAI chat-completions wire format work.
type Config struct {
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	// HasKey is an output-only helper for the UI so we can tell the admin
	// a key is stored without returning the key itself.
	HasKey bool `json:"has_key"`
}

// Service is the AI query assistant service.
type Service struct {
	db     *nucleus.Client
	logger *slog.Logger
	client *http.Client
}

// NewService creates an AI query service.
func NewService(db *nucleus.Client, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// GetConfig returns the current AI config with the API key redacted
// (HasKey indicates whether one is stored).
func (s *Service) GetConfig(ctx context.Context) (Config, error) {
	raw, err := s.readSetting(ctx, settingsKey)
	if err != nil {
		return Config{}, err
	}
	if raw == "" {
		return Config{}, nil
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, fmt.Errorf("aiquery: parse config: %w", err)
	}
	return Config{
		Provider: c.Provider,
		Endpoint: c.Endpoint,
		Model:    c.Model,
		HasKey:   c.APIKey != "",
	}, nil
}

// SetConfig persists the AI config.  Admin-only — the caller must enforce
// RBAC.  A blank APIKey preserves the existing key (so the admin can
// re-save the form without retyping it).
func (s *Service) SetConfig(ctx context.Context, cfg Config) error {
	if cfg.APIKey == "" {
		// Preserve existing key on partial update.
		existing, err := s.loadFullConfig(ctx)
		if err == nil && existing.APIKey != "" {
			cfg.APIKey = existing.APIKey
		}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultURL
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.writeSetting(ctx, settingsKey, string(raw))
}

// loadFullConfig returns the config WITH the API key (for Generate);
// callers outside this package should use GetConfig which redacts it.
func (s *Service) loadFullConfig(ctx context.Context) (Config, error) {
	raw, err := s.readSetting(ctx, settingsKey)
	if err != nil || raw == "" {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, fmt.Errorf("aiquery: parse config: %w", err)
	}
	return c, nil
}

// GenerateResult is the shape returned to the UI.
type GenerateResult struct {
	SQL        string `json:"sql"`
	Model      string `json:"model"`
	LatencyMs  int64  `json:"latency_ms"`
	TokensIn   int    `json:"tokens_in,omitempty"`
	TokensOut  int    `json:"tokens_out,omitempty"`
}

// Generate asks the configured LLM to translate question into SQL,
// using schemaCard as context. The returned SQL has been stripped of
// markdown fences but has NOT been validated — callers must run it
// through the explorer lexer before executing.
func (s *Service) Generate(ctx context.Context, question, schemaCard string) (GenerateResult, error) {
	cfg, err := s.loadFullConfig(ctx)
	if err != nil {
		return GenerateResult{}, err
	}
	if cfg.APIKey == "" {
		return GenerateResult{}, fmt.Errorf("aiquery: not configured (set AI provider + key at /settings/ai)")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultURL
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}

	systemPrompt := BuildSystemPrompt(schemaCard)
	reqBody := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: question},
		},
		Temperature: 0.2,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return GenerateResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(buf))
	if err != nil {
		return GenerateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	// Anthropic messages API uses a different header; add both so
	// OpenAI-compatible endpoints and Anthropic direct both work.
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("aiquery: request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return GenerateResult{}, fmt.Errorf("aiquery: upstream %d: %s", resp.StatusCode, string(body))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return GenerateResult{}, fmt.Errorf("aiquery: parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return GenerateResult{}, fmt.Errorf("aiquery: empty response")
	}

	raw := parsed.Choices[0].Message.Content
	sql := PostProcess(raw)

	return GenerateResult{
		SQL:       sql,
		Model:     cfg.Model,
		LatencyMs: time.Since(start).Milliseconds(),
		TokensIn:  parsed.Usage.PromptTokens,
		TokensOut: parsed.Usage.CompletionTokens,
	}, nil
}

// ---- OpenAI chat-completions wire types ----

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float32       `json:"temperature,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

// PostProcess strips markdown code fences and surrounding prose so only
// raw SQL remains. Defensive against providers that ignore the
// "SQL only" instruction.
func PostProcess(raw string) string {
	s := strings.TrimSpace(raw)
	// Strip ```sql ... ``` or ``` ... ``` wrappers.
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

// ---- settings persistence ----

func (s *Service) readSetting(ctx context.Context, key string) (string, error) {
	type row struct {
		Value string `db:"value"`
	}
	rows, err := nucleus.Query[row](ctx, s.db.SQL(),
		"SELECT value FROM instance_settings WHERE key = $1 ORDER BY updated_at DESC LIMIT 1",
		key)
	if err != nil {
		return "", fmt.Errorf("aiquery: read setting: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].Value, nil
}

func (s *Service) writeSetting(ctx context.Context, key, value string) error {
	now := dbutil.IntParam(time.Now().UnixMilli())
	// MergeTree append-style upsert: insert a new version; read side
	// picks the newest row by updated_at.
	_, err := s.db.SQL().Exec(ctx,
		"INSERT INTO instance_settings (key, value, updated_at) VALUES ($1, $2, $3)",
		key, value, now)
	if err != nil {
		return fmt.Errorf("aiquery: write setting: %w", err)
	}
	return nil
}
