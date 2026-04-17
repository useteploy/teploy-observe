package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/observe/internal/dbutil"
)

type LLMService struct {
	db *nucleus.Client
}

func NewLLMService(db *nucleus.Client) *LLMService {
	return &LLMService{db: db}
}

// LLMTrace represents a single AI model call.
type LLMTrace struct {
	TraceID          string `json:"trace_id" db:"trace_id"`
	TenantID         string `json:"-" db:"tenant_id"`
	SiteID           string `json:"site_id" db:"site_id"`
	SessionID        string `json:"session_id" db:"session_id"`
	SpanID           string `json:"span_id" db:"span_id"`
	Timestamp        int64  `json:"timestamp" db:"timestamp"`
	Model            string `json:"model" db:"model"`
	Provider         string `json:"provider" db:"provider"`
	Operation        string `json:"operation" db:"operation"`
	PromptTokens     string `json:"prompt_tokens" db:"prompt_tokens"`
	CompletionTokens string `json:"completion_tokens" db:"completion_tokens"`
	TotalTokens      string `json:"total_tokens" db:"total_tokens"`
	CostUSD          string `json:"cost_usd" db:"cost_usd"`
	LatencyMs        string `json:"latency_ms" db:"latency_ms"`
	Status           string `json:"status" db:"status"`
	ErrorMessage     string `json:"error_message" db:"error_message"`
	Prompt           string `json:"prompt" db:"prompt"`
	Completion       string `json:"completion" db:"completion"`
	Metadata         string `json:"metadata" db:"metadata"`
}

// LLMInput is the ingestion request body.
type LLMInput struct {
	SiteID           string         `json:"site_id"`
	SessionID        string         `json:"session_id"`
	SpanID           string         `json:"span_id"`
	Model            string         `json:"model"`
	Provider         string         `json:"provider"`
	Operation        string         `json:"operation"`
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	CostUSD          float64        `json:"cost_usd"`
	LatencyMs        int            `json:"latency_ms"`
	Status           string         `json:"status"`
	ErrorMessage     string         `json:"error_message"`
	Prompt           string         `json:"prompt"`
	Completion       string         `json:"completion"`
	Metadata         map[string]any `json:"metadata"`
}

type LLMResponse struct {
	OK      bool   `json:"ok"`
	TraceID string `json:"trace_id"`
}

func (s *LLMService) Ingest(ctx context.Context, input LLMInput) (LLMResponse, error) {
	id := genID()
	now := time.Now().UTC().UnixMilli()
	totalTokens := input.PromptTokens + input.CompletionTokens

	if input.Operation == "" {
		input.Operation = "completion"
	}
	if input.Status == "" {
		input.Status = "ok"
	}

	metaJSON := ""
	if input.Metadata != nil {
		raw, _ := json.Marshal(input.Metadata)
		metaJSON = string(raw)
	}

	// Auto-calculate cost if not provided
	cost := input.CostUSD
	if cost == 0 && totalTokens > 0 {
		cost = estimateCost(input.Model, input.PromptTokens, input.CompletionTokens)
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO llm_traces (trace_id, tenant_id, site_id, session_id, span_id, timestamp,
			model, provider, operation, prompt_tokens, completion_tokens, total_tokens,
			cost_usd, latency_ms, status, error_message, prompt, completion, metadata)
		 VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		id, input.SiteID, input.SessionID, input.SpanID, now,
		input.Model, input.Provider, input.Operation,
		strconv.Itoa(input.PromptTokens), strconv.Itoa(input.CompletionTokens), strconv.Itoa(totalTokens),
		fmt.Sprintf("%.6f", cost), strconv.Itoa(input.LatencyMs),
		input.Status, input.ErrorMessage, input.Prompt, input.Completion, metaJSON,
	)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("insert llm trace: %w", err)
	}
	return LLMResponse{OK: true, TraceID: id}, nil
}

// LLMStats returns aggregate LLM usage statistics.
type LLMStats struct {
	TotalCalls       string `json:"total_calls" db:"total_calls"`
	TotalTokens      string `json:"total_tokens" db:"total_tokens"`
	TotalCostUSD     string `json:"total_cost_usd" db:"total_cost_usd"`
	AvgLatencyMs     string `json:"avg_latency_ms" db:"avg_latency_ms"`
	ErrorCount       string `json:"error_count" db:"error_count"`
}

type ModelStats struct {
	Model        string `json:"model" db:"model"`
	Provider     string `json:"provider" db:"provider"`
	CallCount    string `json:"call_count" db:"call_count"`
	TotalTokens  string `json:"total_tokens" db:"total_tokens"`
	TotalCostUSD string `json:"total_cost_usd" db:"total_cost_usd"`
	AvgLatencyMs string `json:"avg_latency_ms" db:"avg_latency_ms"`
}

func (s *LLMService) Stats(ctx context.Context, siteID string, from, to time.Time) (*LLMStats, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	type rawRow struct {
		Calls    string `db:"calls"`
		Tokens   string `db:"tokens"`
		Cost     string `db:"cost"`
		Latency  string `db:"latency"`
		Errors   string `db:"errors"`
	}

	rows, err := nucleus.Query[rawRow](ctx, s.db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS calls,
			CAST(SUM(CAST(total_tokens AS BIGINT)) AS TEXT) AS tokens,
			CAST(SUM(CAST(cost_usd AS BIGINT)) AS TEXT) AS cost,
			CAST(AVG(CAST(latency_ms AS BIGINT)) AS TEXT) AS latency,
			CAST(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) AS TEXT) AS errors
		 FROM llm_traces WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`,
		siteID, fromMs, toMs,
	)
	if err != nil || len(rows) == 0 {
		return &LLMStats{}, err
	}

	return &LLMStats{
		TotalCalls: rows[0].Calls, TotalTokens: rows[0].Tokens,
		TotalCostUSD: rows[0].Cost, AvgLatencyMs: rows[0].Latency,
		ErrorCount: rows[0].Errors,
	}, nil
}

func (s *LLMService) ModelBreakdown(ctx context.Context, siteID string, from, to time.Time) ([]ModelStats, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[ModelStats](ctx, s.db.SQL(),
		`SELECT model, provider,
			CAST(COUNT(*) AS TEXT) AS call_count,
			CAST(SUM(CAST(total_tokens AS BIGINT)) AS TEXT) AS total_tokens,
			CAST(SUM(CAST(cost_usd AS BIGINT)) AS TEXT) AS total_cost_usd,
			CAST(AVG(CAST(latency_ms AS BIGINT)) AS TEXT) AS avg_latency_ms
		 FROM llm_traces WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		 GROUP BY model, provider
		 ORDER BY call_count DESC`,
		siteID, fromMs, toMs,
	)
}

func (s *LLMService) RecentTraces(ctx context.Context, siteID string, limit int) ([]LLMTrace, error) {
	if limit <= 0 { limit = 20 }
	return nucleus.Query[LLMTrace](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT trace_id, tenant_id, site_id, session_id, span_id, timestamp,
			model, provider, operation, prompt_tokens, completion_tokens, total_tokens,
			cost_usd, latency_ms, status, error_message,
			COALESCE(prompt, '') AS prompt, COALESCE(completion, '') AS completion,
			COALESCE(metadata, '') AS metadata
		 FROM llm_traces WHERE site_id = $1
		 ORDER BY timestamp DESC LIMIT %d`, limit),
		siteID,
	)
}

// estimateCost provides rough cost estimates for common models.
func estimateCost(model string, promptTokens, completionTokens int) float64 {
	// Prices per 1K tokens (approximate, 2026 pricing)
	type pricing struct{ input, output float64 }
	prices := map[string]pricing{
		"gpt-4":         {0.03, 0.06},
		"gpt-4-turbo":   {0.01, 0.03},
		"gpt-4o":        {0.005, 0.015},
		"gpt-3.5-turbo": {0.0005, 0.0015},
		"claude-3-opus": {0.015, 0.075},
		"claude-3-sonnet": {0.003, 0.015},
		"claude-3-haiku": {0.00025, 0.00125},
	}
	p, ok := prices[model]
	if !ok { p = pricing{0.001, 0.002} } // default
	return (float64(promptTokens)/1000)*p.input + (float64(completionTokens)/1000)*p.output
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
