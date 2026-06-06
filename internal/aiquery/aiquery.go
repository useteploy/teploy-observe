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

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/secretbox"
)

const (
	settingsKey = "ai_query_config"
	defaultURL  = "https://api.openai.com/v1/chat/completions"
)

// Config holds the admin-supplied LLM connection settings. Provider is
// informational ("openai", "ollama", "compatible"); every provider must speak
// the OpenAI chat-completions wire format. Anthropic is supported only via an
// OpenAI-compatible proxy, not its native /v1/messages API.
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
	// Encrypt the API key at rest. Fail closed: refuse to persist a secret in
	// plaintext rather than silently leaking it into instance_settings.
	if cfg.APIKey != "" {
		enc, err := secretbox.Encrypt(cfg.APIKey)
		if err != nil {
			return fmt.Errorf("aiquery: cannot store API key: %w (set OBSERVE_SECRET_KEY)", err)
		}
		cfg.APIKey = enc
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
	if c.APIKey != "" {
		dec, err := secretbox.Decrypt(c.APIKey)
		if err != nil {
			return Config{}, fmt.Errorf("aiquery: decrypt API key: %w", err)
		}
		c.APIKey = dec
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
	// All supported providers speak the OpenAI chat-completions wire format
	// (Anthropic is reached via an OpenAI-compatible proxy, not directly), so
	// only the Bearer header is sent. Sending Anthropic-only headers here was
	// misleading: this code can neither build nor parse Anthropic's native API.
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("aiquery: request: %w", err)
	}
	defer resp.Body.Close()

	// Bound the upstream body and do not reflect raw upstream content to the
	// caller (it can contain the provider's own error detail / echoed input).
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		s.logger.Error("aiquery upstream error", "status", resp.StatusCode, "body", string(body))
		return GenerateResult{}, fmt.Errorf("aiquery: upstream returned %d", resp.StatusCode)
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
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	// Strip the trailing fence.
	if idx := strings.LastIndex(s, "```"); idx != -1 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	// Strip a leading "sql" language tag (the only tag we emit/expect),
	// whether the code follows on the same line or the next. We don't strip an
	// arbitrary leading word because "```SELECT 1```" has no language tag and
	// SELECT must survive.
	if low := strings.ToLower(s); strings.HasPrefix(low, "sql") {
		rest := s[3:]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r' {
			s = strings.TrimSpace(rest)
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
