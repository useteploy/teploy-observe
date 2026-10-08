package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

type LLMService struct {
	db            *nucleus.Client
	logger        *slog.Logger
	cat           catalogCache
	aggregateRead func(context.Context, string, ...any) (aggregateRows, error)
}

func NewLLMService(db *nucleus.Client) *LLMService {
	return &LLMService{db: db, logger: slog.Default()}
}

// Token/cost provenance values (O14: an estimated number must say
// estimated everywhere it renders).
const (
	TokenSourceReported  = "reported"
	TokenSourceEstimated = "estimated"

	CostSourceReported  = "reported"
	CostSourceEstimated = "estimated"
)

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
	TokenSource      string `json:"token_source" db:"token_source"`
	CostUSD          string `json:"cost_usd" db:"cost_usd"`
	CostSource       string `json:"cost_source" db:"cost_source"`
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
	TokenSource      string         `json:"token_source"`
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

// maxTokensPerTrace and friends bound telemetry numerics at the boundary
// (audit F28): negative or overflowed token/cost/latency values corrupted
// usage reports and cost totals, and an int overflow in PromptTokens+
// CompletionTokens wrapped to a bogus total.
const (
	maxTokensPerTrace = 100_000_000
	maxLatencyMs      = 24 * 60 * 60 * 1000
	maxCostUSD        = 1_000_000
)

func validateInput(input *LLMInput) error {
	if input.PromptTokens < 0 || input.CompletionTokens < 0 ||
		input.PromptTokens > maxTokensPerTrace || input.CompletionTokens > maxTokensPerTrace ||
		input.CompletionTokens > maxTokensPerTrace-input.PromptTokens {
		return fmt.Errorf("invalid token counts (each >= 0, sum <= %d)", maxTokensPerTrace)
	}
	if input.LatencyMs < 0 || input.LatencyMs > maxLatencyMs {
		return fmt.Errorf("invalid latency_ms (must be in [0, %d])", maxLatencyMs)
	}
	if input.CostUSD < 0 || input.CostUSD > maxCostUSD || math.IsNaN(input.CostUSD) || math.IsInf(input.CostUSD, 0) {
		return fmt.Errorf("invalid cost_usd (must be a finite number in [0, %d])", maxCostUSD)
	}
	switch input.TokenSource {
	case "", TokenSourceReported, TokenSourceEstimated:
	default:
		return fmt.Errorf("invalid token_source (must be 'reported' or 'estimated')")
	}
	return nil
}

func (s *LLMService) Ingest(ctx context.Context, input LLMInput) (LLMResponse, error) {
	if err := validateInput(&input); err != nil {
		return LLMResponse{}, err
	}
	id := genID()
	now := time.Now().UTC().UnixMilli()
	totalTokens := input.PromptTokens + input.CompletionTokens

	if input.Operation == "" {
		input.Operation = "completion"
	}
	if input.Status == "" {
		input.Status = "ok"
	}

	metaJSON := "null"
	if input.Metadata != nil {
		if raw, err := json.Marshal(input.Metadata); err == nil {
			metaJSON = string(raw)
		}
	}

	// Token provenance (O14): counts arriving in the payload are REPORTED
	// by construction; 'estimated' is the producer's own declaration (it
	// derived the counts, e.g. chars/4). The server never invents counts.
	tokenSource := input.TokenSource
	if tokenSource == "" && totalTokens > 0 {
		tokenSource = TokenSourceReported
	}

	// Cost provenance: a caller-supplied cost is reported; an auto-derived
	// cost is ESTIMATED from the versioned catalog and labeled so - the
	// number is a catalog computation, never an invoice.
	cost := input.CostUSD
	costSource := ""
	if cost > 0 {
		costSource = CostSourceReported
	} else if totalTokens > 0 {
		in, out, _ := resolveCatalog(s.catalog(ctx), input.Provider, input.Model)
		cost = (float64(input.PromptTokens)/1000)*in + (float64(input.CompletionTokens)/1000)*out
		costSource = CostSourceEstimated
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO llm_traces (trace_id, tenant_id, site_id, session_id, span_id, timestamp,
			model, provider, operation, prompt_tokens, completion_tokens, total_tokens,
			token_source, cost_usd, cost_source, latency_ms, status, error_message, prompt, completion, metadata)
		 VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		id, input.SiteID, input.SessionID, input.SpanID, now,
		input.Model, input.Provider, input.Operation,
		strconv.Itoa(input.PromptTokens), strconv.Itoa(input.CompletionTokens), strconv.Itoa(totalTokens),
		tokenSource, fmt.Sprintf("%.6f", cost), costSource, strconv.Itoa(input.LatencyMs),
		input.Status, input.ErrorMessage, input.Prompt, input.Completion, metaJSON,
	)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("insert llm trace: %w", err)
	}
	return LLMResponse{OK: true, TraceID: id}, nil
}

// LLMStats returns aggregate LLM usage statistics. The cost fields are
// split by provenance (O14): total is everything, estimated is the subset
// derived from the catalog - an estimated number must be labelable
// everywhere it renders, so the split rides every surface that sums cost.
type LLMStats struct {
	ProvenanceStats
	TotalCalls   string `json:"total_calls" db:"total_calls"`
	TotalTokens  string `json:"total_tokens" db:"total_tokens"`
	TotalCostUSD string `json:"total_cost_usd" db:"total_cost_usd"`
	// EstimatedCostUSD: the catalog-derived subset of TotalCostUSD.
	EstimatedCostUSD string `json:"estimated_cost_usd" db:"estimated_cost_usd"`
	// ReportedCostUSD: the caller-supplied subset.
	ReportedCostUSD string `json:"reported_cost_usd" db:"reported_cost_usd"`
	// CostUnattributed: rows whose cost_source is '' (legacy, pre-054).
	CostUnattributed string `json:"cost_unattributed" db:"cost_unattributed"`
	AvgLatencyMs     string `json:"avg_latency_ms" db:"avg_latency_ms"`
	ErrorCount       string `json:"error_count" db:"error_count"`
}

type ModelStats struct {
	ProvenanceStats
	ReportedCostUSD  string `json:"reported_cost_usd"`
	CostUnattributed string `json:"cost_unattributed"`
	Model            string `json:"model" db:"model"`
	Provider         string `json:"provider" db:"provider"`
	CallCount        string `json:"call_count" db:"call_count"`
	TotalTokens      string `json:"total_tokens" db:"total_tokens"`
	TotalCostUSD     string `json:"total_cost_usd" db:"total_cost_usd"`
	// EstimatedCostUSD: the catalog-derived subset of TotalCostUSD.
	EstimatedCostUSD string `json:"estimated_cost_usd" db:"estimated_cost_usd"`
	AvgLatencyMs     string `json:"avg_latency_ms" db:"avg_latency_ms"`
}

func (s *LLMService) Stats(ctx context.Context, siteID string, from, to time.Time) (*LLMStats, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Preserve raw result presence and text at the owned scanner boundary.
	// Invalid/missing aggregate cells refuse before constructing a success DTO.
	type rawRow struct {
		InvalidNumericRows   int64   `db:"invalid_numeric_rows"`
		LegacyCostUSD        float64 `db:"legacy_cost_usd"`
		ReportedTokensIn     int64   `db:"reported_tokens_in"`
		ReportedTokensOut    int64   `db:"reported_tokens_out"`
		ReportedTokensTotal  int64   `db:"reported_tokens_total"`
		ReportedCostCalls    int64   `db:"reported_cost_calls"`
		ReportedTokenCalls   int64   `db:"reported_token_calls"`
		EstimatedTokensIn    int64   `db:"estimated_tokens_in"`
		EstimatedTokensOut   int64   `db:"estimated_tokens_out"`
		EstimatedTokensTotal int64   `db:"estimated_tokens_total"`
		EstimatedCostCalls   int64   `db:"estimated_cost_calls"`
		EstimatedTokenCalls  int64   `db:"estimated_token_calls"`
		LegacyTokensIn       int64   `db:"legacy_tokens_in"`
		LegacyTokensOut      int64   `db:"legacy_tokens_out"`
		LegacyTokensTotal    int64   `db:"legacy_tokens_total"`
		LegacyCostCalls      int64   `db:"legacy_cost_calls"`
		LegacyTokenCalls     int64   `db:"legacy_token_calls"`
		Calls                int64   `db:"calls"`
		Tokens               int64   `db:"tokens"`
		Cost                 float64 `db:"cost"`
		Estimated            float64 `db:"estimated"`
		Reported             float64 `db:"reported"`
		Unattributed         int64   `db:"unattributed"`
		Latency              float64 `db:"latency"`
		Errors               int64   `db:"errors"`
	}

	rows, err := queryAggregate[rawRow](ctx, s,
		`SELECT `+invalidLLMNumericsSQL+`COALESCE(SUM(CASE WHEN COALESCE(cost_source,'') NOT IN ('reported','estimated') THEN CAST(cost_usd AS DOUBLE) ELSE 0 END),0) AS legacy_cost_usd,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN CAST(prompt_tokens AS BIGINT) ELSE 0 END),0) AS reported_tokens_in,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN CAST(completion_tokens AS BIGINT) ELSE 0 END),0) AS reported_tokens_out,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN CAST(total_tokens AS BIGINT) ELSE 0 END),0) AS reported_tokens_total,
COALESCE(SUM(CASE WHEN cost_source = 'reported' THEN 1 ELSE 0 END),0) AS reported_cost_calls,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN 1 ELSE 0 END),0) AS reported_token_calls,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN CAST(prompt_tokens AS BIGINT) ELSE 0 END),0) AS estimated_tokens_in,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN CAST(completion_tokens AS BIGINT) ELSE 0 END),0) AS estimated_tokens_out,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN CAST(total_tokens AS BIGINT) ELSE 0 END),0) AS estimated_tokens_total,
COALESCE(SUM(CASE WHEN cost_source = 'estimated' THEN 1 ELSE 0 END),0) AS estimated_cost_calls,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN 1 ELSE 0 END),0) AS estimated_token_calls,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN CAST(prompt_tokens AS BIGINT) ELSE 0 END),0) AS legacy_tokens_in,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN CAST(completion_tokens AS BIGINT) ELSE 0 END),0) AS legacy_tokens_out,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN CAST(total_tokens AS BIGINT) ELSE 0 END),0) AS legacy_tokens_total,
COALESCE(SUM(CASE WHEN COALESCE(cost_source,'') NOT IN ('reported','estimated') THEN 1 ELSE 0 END),0) AS legacy_cost_calls,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN 1 ELSE 0 END),0) AS legacy_token_calls,
COUNT(*) AS calls,
			COALESCE(SUM(CAST(total_tokens AS BIGINT)), 0) AS tokens,
			COALESCE(SUM(CAST(cost_usd AS DOUBLE)), 0) AS cost,
			COALESCE(SUM(CASE WHEN cost_source = 'estimated' THEN CAST(cost_usd AS DOUBLE) ELSE 0 END), 0) AS estimated,
			COALESCE(SUM(CASE WHEN cost_source = 'reported' THEN CAST(cost_usd AS DOUBLE) ELSE 0 END), 0) AS reported,
			COALESCE(SUM(CASE WHEN COALESCE(cost_source,'') NOT IN ('reported','estimated') THEN 1 ELSE 0 END), 0) AS unattributed,
			COALESCE(AVG(CAST(latency_ms AS BIGINT)), 0) AS latency,
			COALESCE(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), 0) AS errors
		 FROM llm_traces WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return &LLMStats{}, err
	}

	if len(rows) == 0 {
		if err := s.confirmEmptyWindow(ctx, siteID, fromMs, toMs); err != nil {
			return nil, err
		}
		return &LLMStats{
			ProvenanceStats: (provenanceRow{}).dto(),
			TotalCalls:      "0", TotalTokens: "0",
			TotalCostUSD: "0", EstimatedCostUSD: "0", ReportedCostUSD: "0", CostUnattributed: "0",
			AvgLatencyMs: "0", ErrorCount: "0",
		}, nil
	}

	if len(rows) != 1 {
		return nil, fmt.Errorf("llm stats: expected one aggregate row")
	}
	r := rows[0]
	if r.InvalidNumericRows != 0 {
		return nil, fmt.Errorf("llm stats: invalid legacy numeric rows (%d)", r.InvalidNumericRows)
	}
	return &LLMStats{
		ProvenanceStats:  (provenanceRow{LegacyCostUSD: r.LegacyCostUSD, ReportedTokensIn: r.ReportedTokensIn, ReportedTokensOut: r.ReportedTokensOut, ReportedTokensTotal: r.ReportedTokensTotal, ReportedCostCalls: r.ReportedCostCalls, ReportedTokenCalls: r.ReportedTokenCalls, EstimatedTokensIn: r.EstimatedTokensIn, EstimatedTokensOut: r.EstimatedTokensOut, EstimatedTokensTotal: r.EstimatedTokensTotal, EstimatedCostCalls: r.EstimatedCostCalls, EstimatedTokenCalls: r.EstimatedTokenCalls, LegacyTokensIn: r.LegacyTokensIn, LegacyTokensOut: r.LegacyTokensOut, LegacyTokensTotal: r.LegacyTokensTotal, LegacyCostCalls: r.LegacyCostCalls, LegacyTokenCalls: r.LegacyTokenCalls}).dto(),
		TotalCalls:       strconv.FormatInt(r.Calls, 10),
		TotalTokens:      strconv.FormatInt(r.Tokens, 10),
		TotalCostUSD:     strconv.FormatFloat(r.Cost, 'f', -1, 64),
		EstimatedCostUSD: strconv.FormatFloat(r.Estimated, 'f', -1, 64),
		ReportedCostUSD:  strconv.FormatFloat(r.Reported, 'f', -1, 64),
		CostUnattributed: strconv.FormatInt(r.Unattributed, 10),
		AvgLatencyMs:     strconv.FormatFloat(r.Latency, 'f', -1, 64),
		ErrorCount:       strconv.FormatInt(r.Errors, 10),
	}, nil
}

func (s *LLMService) ModelBreakdown(ctx context.Context, siteID string, from, to time.Time) ([]ModelStats, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Use the same required raw-text aggregate boundary as Stats().
	type rawRow struct {
		InvalidNumericRows   int64   `db:"invalid_numeric_rows"`
		LegacyCostUSD        float64 `db:"legacy_cost_usd"`
		ReportedTokensIn     int64   `db:"reported_tokens_in"`
		ReportedTokensOut    int64   `db:"reported_tokens_out"`
		ReportedTokensTotal  int64   `db:"reported_tokens_total"`
		ReportedCostCalls    int64   `db:"reported_cost_calls"`
		ReportedTokenCalls   int64   `db:"reported_token_calls"`
		EstimatedTokensIn    int64   `db:"estimated_tokens_in"`
		EstimatedTokensOut   int64   `db:"estimated_tokens_out"`
		EstimatedTokensTotal int64   `db:"estimated_tokens_total"`
		EstimatedCostCalls   int64   `db:"estimated_cost_calls"`
		EstimatedTokenCalls  int64   `db:"estimated_token_calls"`
		LegacyTokensIn       int64   `db:"legacy_tokens_in"`
		LegacyTokensOut      int64   `db:"legacy_tokens_out"`
		LegacyTokensTotal    int64   `db:"legacy_tokens_total"`
		LegacyCostCalls      int64   `db:"legacy_cost_calls"`
		LegacyTokenCalls     int64   `db:"legacy_token_calls"`
		Model                string  `db:"model"`
		Provider             string  `db:"provider"`
		CallCount            int64   `db:"call_count"`
		TotalTokens          int64   `db:"total_tokens"`
		TotalCostUSD         float64 `db:"total_cost_usd"`
		Estimated            float64 `db:"estimated_cost_usd"`
		Reported             float64 `db:"reported_cost_usd"`
		Unattributed         int64   `db:"unattributed"`
		AvgLatencyMs         float64 `db:"avg_latency_ms"`
	}
	rows, err := queryAggregate[rawRow](ctx, s,
		`SELECT model, provider, `+invalidLLMNumericsSQL+`
COALESCE(SUM(CASE WHEN COALESCE(cost_source,'') NOT IN ('reported','estimated') THEN CAST(cost_usd AS DOUBLE) ELSE 0 END),0) AS legacy_cost_usd,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN CAST(prompt_tokens AS BIGINT) ELSE 0 END),0) AS reported_tokens_in,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN CAST(completion_tokens AS BIGINT) ELSE 0 END),0) AS reported_tokens_out,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN CAST(total_tokens AS BIGINT) ELSE 0 END),0) AS reported_tokens_total,
COALESCE(SUM(CASE WHEN cost_source = 'reported' THEN 1 ELSE 0 END),0) AS reported_cost_calls,
COALESCE(SUM(CASE WHEN token_source = 'reported' THEN 1 ELSE 0 END),0) AS reported_token_calls,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN CAST(prompt_tokens AS BIGINT) ELSE 0 END),0) AS estimated_tokens_in,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN CAST(completion_tokens AS BIGINT) ELSE 0 END),0) AS estimated_tokens_out,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN CAST(total_tokens AS BIGINT) ELSE 0 END),0) AS estimated_tokens_total,
COALESCE(SUM(CASE WHEN cost_source = 'estimated' THEN 1 ELSE 0 END),0) AS estimated_cost_calls,
COALESCE(SUM(CASE WHEN token_source = 'estimated' THEN 1 ELSE 0 END),0) AS estimated_token_calls,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN CAST(prompt_tokens AS BIGINT) ELSE 0 END),0) AS legacy_tokens_in,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN CAST(completion_tokens AS BIGINT) ELSE 0 END),0) AS legacy_tokens_out,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN CAST(total_tokens AS BIGINT) ELSE 0 END),0) AS legacy_tokens_total,
COALESCE(SUM(CASE WHEN COALESCE(cost_source,'') NOT IN ('reported','estimated') THEN 1 ELSE 0 END),0) AS legacy_cost_calls,
COALESCE(SUM(CASE WHEN COALESCE(token_source,'') NOT IN ('reported','estimated') THEN 1 ELSE 0 END),0) AS legacy_token_calls,

			COUNT(*) AS call_count,
			COALESCE(SUM(CAST(total_tokens AS BIGINT)), 0) AS total_tokens,
			COALESCE(SUM(CAST(cost_usd AS DOUBLE)), 0) AS total_cost_usd,
			COALESCE(SUM(CASE WHEN cost_source = 'estimated' THEN CAST(cost_usd AS DOUBLE) ELSE 0 END), 0) AS estimated_cost_usd,
 COALESCE(SUM(CASE WHEN cost_source = 'reported' THEN CAST(cost_usd AS DOUBLE) ELSE 0 END),0) AS reported_cost_usd,
 COALESCE(SUM(CASE WHEN COALESCE(cost_source,'') NOT IN ('reported','estimated') THEN 1 ELSE 0 END),0) AS unattributed,
			COALESCE(AVG(CAST(latency_ms AS BIGINT)), 0) AS avg_latency_ms
		 FROM llm_traces WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 GROUP BY model, provider
		 ORDER BY call_count DESC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if err := s.confirmEmptyWindow(ctx, siteID, fromMs, toMs); err != nil {
			return nil, err
		}
	}
	out := make([]ModelStats, 0, len(rows))
	for _, r := range rows {
		if r.InvalidNumericRows != 0 {
			return nil, fmt.Errorf("llm model %q: invalid legacy numeric rows (%d)", r.Model, r.InvalidNumericRows)
		}
		out = append(out, ModelStats{
			ProvenanceStats:  (provenanceRow{LegacyCostUSD: r.LegacyCostUSD, ReportedTokensIn: r.ReportedTokensIn, ReportedTokensOut: r.ReportedTokensOut, ReportedTokensTotal: r.ReportedTokensTotal, ReportedCostCalls: r.ReportedCostCalls, ReportedTokenCalls: r.ReportedTokenCalls, EstimatedTokensIn: r.EstimatedTokensIn, EstimatedTokensOut: r.EstimatedTokensOut, EstimatedTokensTotal: r.EstimatedTokensTotal, EstimatedCostCalls: r.EstimatedCostCalls, EstimatedTokenCalls: r.EstimatedTokenCalls, LegacyTokensIn: r.LegacyTokensIn, LegacyTokensOut: r.LegacyTokensOut, LegacyTokensTotal: r.LegacyTokensTotal, LegacyCostCalls: r.LegacyCostCalls, LegacyTokenCalls: r.LegacyTokenCalls}).dto(),
			ReportedCostUSD:  strconv.FormatFloat(r.Reported, 'f', -1, 64),
			CostUnattributed: strconv.FormatInt(r.Unattributed, 10),
			Model:            r.Model,
			Provider:         r.Provider,
			CallCount:        strconv.FormatInt(r.CallCount, 10),
			TotalTokens:      strconv.FormatInt(r.TotalTokens, 10),
			TotalCostUSD:     strconv.FormatFloat(r.TotalCostUSD, 'f', -1, 64),
			EstimatedCostUSD: strconv.FormatFloat(r.Estimated, 'f', -1, 64),
			AvgLatencyMs:     strconv.FormatFloat(r.AvgLatencyMs, 'f', -1, 64),
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
			COALESCE(token_source, '') AS token_source,
			cost_usd,
			COALESCE(cost_source, '') AS cost_source,
			latency_ms, status, error_message,
			COALESCE(prompt, '') AS prompt, COALESCE(completion, '') AS completion,
			COALESCE(metadata, '') AS metadata
		 FROM llm_traces WHERE site_id = $1
		 ORDER BY timestamp DESC LIMIT %d`, limit),
		siteID,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ProvenanceStats partitions cost and tokens independently by their stored
// sources. Empty/NULL/unrecognized sources remain legacy/unattributed; amounts
// never infer provenance. All successful values are explicit decimal strings.
type ProvenanceStats struct {
	LegacyCostUSD        string `json:"legacy_cost_usd"`
	ReportedTokensIn     string `json:"reported_tokens_in"`
	ReportedTokensOut    string `json:"reported_tokens_out"`
	ReportedTokensTotal  string `json:"reported_tokens_total"`
	ReportedCostCalls    string `json:"reported_cost_calls"`
	ReportedTokenCalls   string `json:"reported_token_calls"`
	EstimatedTokensIn    string `json:"estimated_tokens_in"`
	EstimatedTokensOut   string `json:"estimated_tokens_out"`
	EstimatedTokensTotal string `json:"estimated_tokens_total"`
	EstimatedCostCalls   string `json:"estimated_cost_calls"`
	EstimatedTokenCalls  string `json:"estimated_token_calls"`
	LegacyTokensIn       string `json:"legacy_tokens_in"`
	LegacyTokensOut      string `json:"legacy_tokens_out"`
	LegacyTokensTotal    string `json:"legacy_tokens_total"`
	LegacyCostCalls      string `json:"legacy_cost_calls"`
	LegacyTokenCalls     string `json:"legacy_token_calls"`
}
type provenanceRow struct {
	LegacyCostUSD        float64 `db:"legacy_cost_usd"`
	ReportedTokensIn     int64   `db:"reported_tokens_in"`
	ReportedTokensOut    int64   `db:"reported_tokens_out"`
	ReportedTokensTotal  int64   `db:"reported_tokens_total"`
	ReportedCostCalls    int64   `db:"reported_cost_calls"`
	ReportedTokenCalls   int64   `db:"reported_token_calls"`
	EstimatedTokensIn    int64   `db:"estimated_tokens_in"`
	EstimatedTokensOut   int64   `db:"estimated_tokens_out"`
	EstimatedTokensTotal int64   `db:"estimated_tokens_total"`
	EstimatedCostCalls   int64   `db:"estimated_cost_calls"`
	EstimatedTokenCalls  int64   `db:"estimated_token_calls"`
	LegacyTokensIn       int64   `db:"legacy_tokens_in"`
	LegacyTokensOut      int64   `db:"legacy_tokens_out"`
	LegacyTokensTotal    int64   `db:"legacy_tokens_total"`
	LegacyCostCalls      int64   `db:"legacy_cost_calls"`
	LegacyTokenCalls     int64   `db:"legacy_token_calls"`
}

func (r provenanceRow) dto() ProvenanceStats {
	return ProvenanceStats{
		LegacyCostUSD:        strconv.FormatFloat(r.LegacyCostUSD, 'f', -1, 64),
		ReportedTokensIn:     strconv.FormatInt(r.ReportedTokensIn, 10),
		ReportedTokensOut:    strconv.FormatInt(r.ReportedTokensOut, 10),
		ReportedTokensTotal:  strconv.FormatInt(r.ReportedTokensTotal, 10),
		ReportedCostCalls:    strconv.FormatInt(r.ReportedCostCalls, 10),
		ReportedTokenCalls:   strconv.FormatInt(r.ReportedTokenCalls, 10),
		EstimatedTokensIn:    strconv.FormatInt(r.EstimatedTokensIn, 10),
		EstimatedTokensOut:   strconv.FormatInt(r.EstimatedTokensOut, 10),
		EstimatedTokensTotal: strconv.FormatInt(r.EstimatedTokensTotal, 10),
		EstimatedCostCalls:   strconv.FormatInt(r.EstimatedCostCalls, 10),
		EstimatedTokenCalls:  strconv.FormatInt(r.EstimatedTokenCalls, 10),
		LegacyTokensIn:       strconv.FormatInt(r.LegacyTokensIn, 10),
		LegacyTokensOut:      strconv.FormatInt(r.LegacyTokensOut, 10),
		LegacyTokensTotal:    strconv.FormatInt(r.LegacyTokensTotal, 10),
		LegacyCostCalls:      strconv.FormatInt(r.LegacyCostCalls, 10),
		LegacyTokenCalls:     strconv.FormatInt(r.LegacyTokenCalls, 10),
	}
}
