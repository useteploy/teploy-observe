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

	"github.com/useteploy/teploy-observe/internal/dbutil"
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
	TotalCalls   string `json:"total_calls" db:"total_calls"`
	TotalTokens  string `json:"total_tokens" db:"total_tokens"`
	TotalCostUSD string `json:"total_cost_usd" db:"total_cost_usd"`
	AvgLatencyMs string `json:"avg_latency_ms" db:"avg_latency_ms"`
	ErrorCount   string `json:"error_count" db:"error_count"`
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

	// Scan as native ints/floats and stringify in Go.
	//
	// Nucleus (per dogfood findings #6, #23, #24) currently:
	//   - returns ZERO rows for `SELECT CAST(COUNT(*) AS TEXT)` against
	//     an empty filtered result set (should be one row, value 0),
	//   - emits the empty string when stringifying any BIGINT aggregate.
	//
	// COALESCE alone doesn't help — the cast-to-text layer is what's
	// broken. So we scan into native types (which Nucleus serializes
	// correctly via the binary protocol), then format on the way out.
	// Empty result sets fall through to the zero-value LLMStats below
	// with explicit "0" strings.
	type rawRow struct {
		Calls   int64   `db:"calls"`
		Tokens  int64   `db:"tokens"`
		Cost    float64 `db:"cost"`
		Latency float64 `db:"latency"`
		Errors  int64   `db:"errors"`
	}

	rows, err := nucleus.Query[rawRow](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS calls,
			COALESCE(SUM(CAST(total_tokens AS BIGINT)), 0) AS tokens,
			COALESCE(SUM(CAST(cost_usd AS DOUBLE)), 0) AS cost,
			COALESCE(AVG(CAST(latency_ms AS BIGINT)), 0) AS latency,
			COALESCE(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), 0) AS errors
		 FROM llm_traces WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return &LLMStats{}, err
	}

	// Empty filter result: dogfood #24 means Nucleus may return zero rows
	// even though COUNT(*) is a scalar aggregate. Synthesize the zeros so
	// the UI renders "0" not blanks.
	if len(rows) == 0 {
		return &LLMStats{
			TotalCalls: "0", TotalTokens: "0",
			TotalCostUSD: "0", AvgLatencyMs: "0", ErrorCount: "0",
		}, nil
	}

	r := rows[0]
	return &LLMStats{
		TotalCalls:   strconv.FormatInt(r.Calls, 10),
		TotalTokens:  strconv.FormatInt(r.Tokens, 10),
		TotalCostUSD: strconv.FormatFloat(r.Cost, 'f', -1, 64),
		AvgLatencyMs: strconv.FormatFloat(r.Latency, 'f', -1, 64),
		ErrorCount:   strconv.FormatInt(r.Errors, 10),
	}, nil
}

func (s *LLMService) ModelBreakdown(ctx context.Context, siteID string, from, to time.Time) ([]ModelStats, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Scan natively for the same dogfood reason as Stats() — CAST(... AS
	// TEXT) over an aggregate returns the empty string from Nucleus.
	type rawRow struct {
		Model        string  `db:"model"`
		Provider     string  `db:"provider"`
		CallCount    int64   `db:"call_count"`
		TotalTokens  int64   `db:"total_tokens"`
		TotalCostUSD float64 `db:"total_cost_usd"`
		AvgLatencyMs float64 `db:"avg_latency_ms"`
	}
	rows, err := nucleus.Query[rawRow](ctx, s.db.SQL(),
		`SELECT model, provider,
			COUNT(*) AS call_count,
			COALESCE(SUM(CAST(total_tokens AS BIGINT)), 0) AS total_tokens,
			COALESCE(SUM(CAST(cost_usd AS DOUBLE)), 0) AS total_cost_usd,
			COALESCE(AVG(CAST(latency_ms AS BIGINT)), 0) AS avg_latency_ms
		 FROM llm_traces WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 GROUP BY model, provider
		 ORDER BY call_count DESC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}
	out := make([]ModelStats, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelStats{
			Model:        r.Model,
			Provider:     r.Provider,
			CallCount:    strconv.FormatInt(r.CallCount, 10),
			TotalTokens:  strconv.FormatInt(r.TotalTokens, 10),
			TotalCostUSD: strconv.FormatFloat(r.TotalCostUSD, 'f', -1, 64),
			AvgLatencyMs: strconv.FormatFloat(r.AvgLatencyMs, 'f', -1, 64),
		})
	}
	return out, nil
}

func (s *LLMService) RecentTraces(ctx context.Context, siteID string, limit int) ([]LLMTrace, error) {
	if limit <= 0 {
		limit = 20
	}
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
		"gpt-4":           {0.03, 0.06},
		"gpt-4-turbo":     {0.01, 0.03},
		"gpt-4o":          {0.005, 0.015},
		"gpt-3.5-turbo":   {0.0005, 0.0015},
		"claude-3-opus":   {0.015, 0.075},
		"claude-3-sonnet": {0.003, 0.015},
		"claude-3-haiku":  {0.00025, 0.00125},
	}
	p, ok := prices[model]
	if !ok {
		p = pricing{0.001, 0.002}
	} // default
	return (float64(promptTokens)/1000)*p.input + (float64(completionTokens)/1000)*p.output
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
