package flags

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

type FlagService struct {
	db *nucleus.Client

	// fetchFlag is the config-read seam Evaluate goes through. Production
	// runs the flagsLatest query; tests inject read failures so an
	// unavailable store is distinguishable from an absent flag (O08).
	fetchFlag        func(ctx context.Context, siteID, flagKey string) ([]FeatureFlag, error)
	unavailableTotal atomic.Int64
	invalidTotal     atomic.Int64
	quarantineSeen   sync.Map // flagID "|" part -> struct{}{} — log-once gate

	// persistEval writes one flag_evaluations row. A seam so the dedupe
	// path is testable without a store; nil-db services (hermetic unit
	// tests) record nothing.
	persistEval func(ctx context.Context, siteID, flagKey, userID, variant string) error
	dedup       *evalDedup
	evalWritten atomic.Int64
	evalDeduped atomic.Int64
	evalFailed  atomic.Int64
}

func NewFlagService(db *nucleus.Client) *FlagService {
	s := &FlagService{db: db, dedup: newEvalDedup(DefaultEvalDedupWindow, DefaultEvalDedupMax)}
	s.persistEval = func(ctx context.Context, siteID, flagKey, userID, variant string) error {
		if s.db == nil {
			return nil
		}
		_, err := s.db.SQL().Exec(ctx,
			`INSERT INTO flag_evaluations (eval_id, tenant_id, site_id, flag_key, user_id, variant, timestamp)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
			genID(), siteID, flagKey, userID, variant, time.Now().UTC().UnixMilli())
		return err
	}
	s.fetchFlag = func(ctx context.Context, siteID, flagKey string) ([]FeatureFlag, error) {
		return nucleus.Query[FeatureFlag](ctx, s.db.SQL(),
			`SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct,
			COALESCE(variants, '') AS variants, COALESCE(targeting, '') AS targeting, created_at, version
		 FROM `+flagsLatest("site_id = $1 AND flag_key = $2"), siteID, flagKey)
	}
	return s
}

// WithEvalDedup sets the flag_evaluations write-dedupe window and cache
// bound (see evaldedup.go). A window <= 0 disables dedupe. Returns the
// receiver for fluent boot-time setup.
func (s *FlagService) WithEvalDedup(window time.Duration, max int) *FlagService {
	s.dedup = newEvalDedup(window, max)
	return s
}

// Stats exposes the O08 condition counters: how many evaluations could not
// read their config (unavailable) and how many were quarantined from
// evaluation by invalid stored config (total plus distinct flag|part
// signatures). Surfaced at /healthz under "flags".
func (s *FlagService) Stats() map[string]int64 {
	distinct := int64(0)
	s.quarantineSeen.Range(func(_, _ any) bool { distinct++; return true })
	return map[string]int64{
		"config_unavailable_total": s.unavailableTotal.Load(),
		"invalid_config_total":     s.invalidTotal.Load(),
		"invalid_config_distinct":  distinct,
		// flag_evaluations write accounting: rows written, evaluations
		// whose row was suppressed by the dedupe window, failed writes,
		// and the dedupe cache's current size / LRU evictions.
		"eval_rows_written_total": s.evalWritten.Load(),
		"eval_rows_deduped_total": s.evalDeduped.Load(),
		"eval_rows_failed_total":  s.evalFailed.Load(),
		"eval_dedup_entries":      int64(s.dedup.size()),
		"eval_dedup_evicted":      s.dedup.evictedTotal(),
	}
}

// FeatureFlag is the domain type with typed fields.
type FeatureFlag struct {
	FlagID      string    `json:"flag_id"`
	SiteID      string    `json:"site_id"`
	FlagKey     string    `json:"flag_key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	FlagType    string    `json:"flag_type"`
	Enabled     bool      `json:"enabled"`
	RolloutPct  int       `json:"rollout_pct"`
	Variants    string    `json:"variants"`
	Targeting   string    `json:"targeting"`
	CreatedAt   time.Time `json:"created_at"`
}

// Variant is one option in a multivariate flag.
type Variant struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	RolloutPct int    `json:"rollout_pct"`
	// Payload is optional JSON returned with the variant by evaluate
	// (<= MaxPayloadBytes compacted). Absent and null mean no payload.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Evaluation condition, carried additively on every SDK response (O08).
const (
	ReasonEvaluated   = "evaluated"
	ReasonUnavailable = "unavailable"
	ReasonInvalid     = "invalid"
)

// EvaluationResult is what the SDK receives.
type EvaluationResult struct {
	Enabled bool   `json:"enabled"`
	Variant string `json:"variant,omitempty"`
	// Payload is the chosen variant's JSON payload, when it has one.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Group is the key of the condition group that admitted the user
	// (groups-format targeting only).
	Group  string `json:"group,omitempty"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

func (s *FlagService) Create(ctx context.Context, siteID, flagKey, name, description, flagType, variants, targeting string, rolloutPct int) (*FeatureFlag, error) {
	id := genID()
	now := time.Now().UTC()
	nowMs := strconv.FormatInt(now.UnixMilli(), 10)
	if flagType == "" {
		flagType = "boolean"
	}
	if rolloutPct < 0 || rolloutPct > 100 {
		return nil, &ValidationError{Err: fmt.Errorf("rollout_pct must be between 0 and 100")}
	}

	// O08 write boundary: invalid rules must not be storable. The store's
	// JSONB column already refuses syntactically broken JSON (as an opaque
	// 500), but VALID JSON of the wrong shape — an unarrayed rule object —
	// or a semantically invalid ruleset (unknown operator, missing value)
	// stores fine and used to silently skip the targeting condition at
	// evaluation. Reject here, naming the error.
	if err := ValidateConfig(flagType, variants, targeting); err != nil {
		return nil, &ValidationError{Err: err}
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO feature_flags (flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, targeting, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'false', $7, NULLIF($8, ''), NULLIF($9, ''), $10, $11)`,
		id, siteID, flagKey, name, description, flagType,
		strconv.Itoa(rolloutPct), variants, targeting, nowMs, nowMs,
	)
	if err != nil {
		return nil, fmt.Errorf("create flag: %w", err)
	}
	s.appendHistory(ctx, id, FlagHistoryEntry{
		Timestamp:  now.UnixMilli(),
		Action:     "created",
		Enabled:    false,
		RolloutPct: rolloutPct,
		Variants:   variants,
		Targeting:  targeting,
	})
	return &FeatureFlag{
		FlagID: id, SiteID: siteID, FlagKey: flagKey, Name: name,
		Description: description, FlagType: flagType, Enabled: false,
		RolloutPct: rolloutPct, Variants: variants, Targeting: targeting,
		CreatedAt: now,
	}, nil
}

func (s *FlagService) List(ctx context.Context, siteID string) ([]FeatureFlag, error) {
	return nucleus.Query[FeatureFlag](ctx, s.db.SQL(),
		`SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct,
			COALESCE(variants, '') AS variants, COALESCE(targeting, '') AS targeting, created_at, version
		 FROM `+flagsLatest("site_id = $1")+`
		 ORDER BY created_at DESC`, siteID)
}

// FlagHistoryEntry captures a change to a flag for the audit log.
type FlagHistoryEntry struct {
	Timestamp  int64  `json:"timestamp"`
	Action     string `json:"action"` // "created" | "toggle" | "update"
	Enabled    bool   `json:"enabled"`
	RolloutPct int    `json:"rollout_pct,omitempty"`
	Variants   string `json:"variants,omitempty"`
	Targeting  string `json:"targeting,omitempty"`
	ChangedBy  string `json:"changed_by,omitempty"`
}

func historyKey(flagID string) string { return "flag_history:" + flagID }

// appendHistory adds an entry to the flag's history (KV-backed, bounded to 100 entries).
func (s *FlagService) appendHistory(ctx context.Context, flagID string, entry FlagHistoryEntry) {
	kv := s.db.KV()
	key := historyKey(flagID)
	raw, _ := kv.Get(ctx, key)
	var list []FlagHistoryEntry
	if raw != nil {
		_ = json.Unmarshal(raw, &list)
	}
	list = append([]FlagHistoryEntry{entry}, list...) // newest first
	if len(list) > 100 {
		list = list[:100]
	}
	updated, _ := json.Marshal(list)
	_ = kv.Set(ctx, key, updated)
}

// History returns the change log for a flag (most-recent first).
func (s *FlagService) History(ctx context.Context, flagID string) ([]FlagHistoryEntry, error) {
	kv := s.db.KV()
	raw, err := kv.Get(ctx, historyKey(flagID))
	if err != nil || raw == nil {
		return []FlagHistoryEntry{}, nil
	}
	var list []FlagHistoryEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		return []FlagHistoryEntry{}, nil
	}
	return list, nil
}

func (s *FlagService) Toggle(ctx context.Context, flagID string, enabled bool) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	val := "false"
	if enabled {
		val = "true"
	}
	// Strictly-monotonic version (the 70f6eff version-tie defect): a
	// same-millisecond create+toggle must not tie, or Evaluate serves the
	// pre-toggle value arbitrarily.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO feature_flags (flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, targeting, created_at, version)
		 SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, $2, rollout_pct, NULLIF(CAST(variants AS TEXT), ''), NULLIF(CAST(targeting AS TEXT), ''), created_at,
		        GREATEST(CAST($3 AS BIGINT), version + 1)
		 FROM `+flagsLatest("flag_id = $1"),
		flagID, val, now)
	if err != nil {
		return err
	}
	s.appendHistory(ctx, flagID, FlagHistoryEntry{
		Timestamp: time.Now().UTC().UnixMilli(),
		Action:    "toggle",
		Enabled:   enabled,
	})
	return nil
}

// TargetingRule defines a condition for flag targeting.
type TargetingRule struct {
	Attribute string `json:"attribute"`
	Operator  string `json:"operator"`
	Value     any    `json:"value"`
}

// ValidationError marks a config rejection at the write boundary so the
// handler can answer 400 naming the error instead of storing it.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// ValidateTargeting parses and sanity-checks a LEGACY flat targeting ruleset
// (the groups format is handled by parseTargetingConfig). Empty,
// "null" and "[]" mean "no restrictions" and are valid. Anything else must
// be a JSON array of rules with a non-empty attribute, a known operator
// and a present value — the same operators matchesTargeting enforces.
func ValidateTargeting(raw string) ([]TargetingRule, error) {
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	var rules []TargetingRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("targeting: invalid JSON: %w", err)
	}
	if err := validateRules(rules, "rule"); err != nil {
		return nil, err
	}
	return rules, nil
}

// ValidateVariants parses and sanity-checks a variant list. Empty means
// "no variants" and is valid. Otherwise each entry needs a non-empty,
// unique key and a rollout_pct within 0..100.
func ValidateVariants(raw string) ([]Variant, error) {
	if raw == "" {
		return nil, nil
	}
	var variants []Variant
	if err := json.Unmarshal([]byte(raw), &variants); err != nil {
		return nil, fmt.Errorf("variants: invalid JSON: %w", err)
	}
	seen := map[string]bool{}
	for i, v := range variants {
		if v.Key == "" {
			return nil, fmt.Errorf("variants: entry %d: key is required", i)
		}
		if seen[v.Key] {
			return nil, fmt.Errorf("variants: duplicate key %q", v.Key)
		}
		seen[v.Key] = true
		if v.RolloutPct < 0 || v.RolloutPct > 100 {
			return nil, fmt.Errorf("variants: entry %d: rollout_pct %d out of range 0..100", i, v.RolloutPct)
		}
		if len(v.Payload) > 0 {
			var compact bytes.Buffer
			if err := json.Compact(&compact, v.Payload); err != nil {
				return nil, fmt.Errorf("variants: entry %d: payload is not valid JSON: %w", i, err)
			}
			if compact.Len() > MaxPayloadBytes {
				return nil, fmt.Errorf("variants: entry %d: payload is %d bytes, limit %d", i, compact.Len(), MaxPayloadBytes)
			}
			variants[i].Payload = json.RawMessage(compact.Bytes())
		}
	}
	return variants, nil
}

// clampDetail bounds the response detail so a pathological stored value
// cannot balloon the evaluation response.
func clampDetail(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// errorClass reduces a read failure to the class the response carries;
// the full error goes to the server log, not the wire.
func errorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "storage"
	}
}

func toStringSlice(v any) []string {
	switch val := v.(type) {
	case []any:
		out := make([]string, len(val))
		for i, item := range val {
			out[i] = fmt.Sprintf("%v", item)
		}
		return out
	case []string:
		return val
	default:
		return []string{fmt.Sprintf("%v", v)}
	}
}

// Evaluate checks a flag for a given user.
//
// O08 three-condition contract — availability, absence and validity are
// distinct, and the response says which one answered:
//
//   - evaluated: a real decision from valid config. A false here is a
//     valid answer (absent flag, disabled, rollout miss, targeting miss),
//     never an error.
//   - unavailable: the config read failed. The store error is logged and
//     counted server-side; the response carries only its class.
//   - invalid: the stored config failed validation. The flag is
//     QUARANTINED from evaluation — never evaluated as unrestricted —
//     the condition is counted, and the first occurrence per flag+part
//     is logged.
//
// Fail-safe default (documented per-flag contract): unavailable and
// invalid both answer Enabled=false. Flags gate feature exposure and the
// create-path default is disabled, so neither an outage nor a corrupt
// config may widen exposure. A per-flag fail-open override (the
// kill-switch pattern) needs a stored default readable WITHOUT the config
// it guards (it cannot live in the targeting JSON: an unavailable or
// invalid config is exactly when it is needed) plus an SDK contract, so it
// stays an O08 residual. SDKs that evaluate locally from
// GET /api/v1/flags/config can keep their last-known config instead.
func (s *FlagService) Evaluate(ctx context.Context, siteID, flagKey, userID string, userCtx map[string]string) (*EvaluationResult, error) {
	rows, err := s.fetchFlag(ctx, siteID, flagKey)
	if err != nil {
		s.unavailableTotal.Add(1)
		log.Printf("[flags] evaluate %s/%s: config unavailable (%v) — answering fail-safe default (disabled), not a decision", siteID, flagKey, err)
		return &EvaluationResult{
			Enabled: false,
			Reason:  ReasonUnavailable,
			Detail:  "flags config read failed: " + errorClass(err),
		}, nil
	}
	if len(rows) == 0 {
		return &EvaluationResult{Enabled: false, Reason: ReasonEvaluated, Detail: "flag not found"}, nil
	}

	flag := rows[0]

	// Read boundary: validate BEFORE the enabled short-circuit so a corrupt
	// config surfaces even while the flag is disabled (operators learn
	// without enabling first), and so invalid rules can never be evaluated
	// as "no restrictions".
	def, cerr := ParseDefinition(flag)
	if cerr != nil {
		return s.quarantine(flag, cerr.Part, cerr.Err), nil
	}

	r := EvaluateDefinition(def, userID, userCtx)
	result := &r
	if !result.Enabled {
		return result, nil
	}

	// Evaluation tracking is best-effort and never changes the answer.
	// Reached only for real decisions (unavailable and invalid return
	// above). At most one row per (site, flag, variant, user) per dedupe
	// window; nothing reads flag_evaluations for experiment exposure
	// counts (those come from experiment_exposures), see evaldedup.go.
	s.recordEvaluation(ctx, siteID, flagKey, userID, result.Variant)

	return result, nil
}

// recordEvaluation applies the dedupe window and writes at most one
// flag_evaluations row. Failures are counted, forgotten (so the next
// evaluation retries) and never surfaced to the caller.
func (s *FlagService) recordEvaluation(ctx context.Context, siteID, flagKey, userID, variant string) {
	if s.persistEval == nil {
		return
	}
	key := evalKey(siteID, flagKey, variant, userID)
	if !s.dedup.admit(key) {
		s.evalDeduped.Add(1)
		return
	}
	if err := s.persistEval(ctx, siteID, flagKey, userID, variant); err != nil {
		s.dedup.forget(key)
		s.evalFailed.Add(1)
		return
	}
	s.evalWritten.Add(1)
}

// quarantine refuses evaluation of a flag whose stored config failed
// validation: counts the condition, logs the first occurrence per
// flag+part (the public evaluate endpoint would otherwise flood the log),
// and answers the documented fail-safe default with reason "invalid".
func (s *FlagService) quarantine(flag FeatureFlag, part string, err error) *EvaluationResult {
	s.invalidTotal.Add(1)
	sig := flag.FlagID + "|" + part
	if _, seen := s.quarantineSeen.LoadOrStore(sig, struct{}{}); !seen {
		log.Printf("[flags] flag %q (%s) quarantined: %v — evaluation refused, answering fail-safe default (disabled)", flag.FlagKey, flag.FlagID, err)
	}
	return &EvaluationResult{
		Enabled: false,
		Reason:  ReasonInvalid,
		Detail:  clampDetail(fmt.Sprintf("stored %s failed validation: %v", part, err)),
	}
}

// hashUser is the pre-groups name for Bucket, kept for existing tests.
func hashUser(flagKey, userID string) int { return Bucket(flagKey, userID) }

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
