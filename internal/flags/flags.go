package flags

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type FlagService struct {
	db *nucleus.Client
}

func NewFlagService(db *nucleus.Client) *FlagService {
	return &FlagService{db: db}
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
}

// EvaluationResult is what the SDK receives.
type EvaluationResult struct {
	Enabled bool   `json:"enabled"`
	Variant string `json:"variant,omitempty"`
}

func (s *FlagService) Create(ctx context.Context, siteID, flagKey, name, description, flagType, variants, targeting string, rolloutPct int) (*FeatureFlag, error) {
	id := genID()
	now := time.Now().UTC()
	nowMs := strconv.FormatInt(now.UnixMilli(), 10)
	if flagType == "" {
		flagType = "boolean"
	}
	if rolloutPct <= 0 {
		rolloutPct = 100
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO feature_flags (flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, targeting, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'false', $7, $8, $9, $10, $11)`,
		id, siteID, flagKey, name, description, flagType,
		strconv.Itoa(rolloutPct), variants, targeting, nowMs, nowMs,
	)
	if err != nil {
		return nil, fmt.Errorf("create flag: %w", err)
	}
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
		 FROM feature_flags WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *FlagService) Toggle(ctx context.Context, flagID string, enabled bool) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	val := "false"
	if enabled {
		val = "true"
	}
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO feature_flags (flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct, variants, targeting, created_at, version)
		 SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, $2, rollout_pct, variants, targeting, created_at, $3
		 FROM feature_flags WHERE flag_id = $1`,
		flagID, val, now)
	return err
}

// TargetingRule defines a condition for flag targeting.
type TargetingRule struct {
	Attribute string `json:"attribute"`
	Operator  string `json:"operator"`
	Value     any    `json:"value"`
}

func matchesTargeting(rules []TargetingRule, userCtx map[string]string) bool {
	for _, r := range rules {
		actual, ok := userCtx[r.Attribute]
		if !ok {
			return false
		}
		switch r.Operator {
		case "eq":
			if actual != fmt.Sprintf("%v", r.Value) {
				return false
			}
		case "neq":
			if actual == fmt.Sprintf("%v", r.Value) {
				return false
			}
		case "in":
			vals := toStringSlice(r.Value)
			found := false
			for _, v := range vals {
				if actual == v {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "not_in":
			vals := toStringSlice(r.Value)
			for _, v := range vals {
				if actual == v {
					return false
				}
			}
		case "contains":
			if !strings.Contains(actual, fmt.Sprintf("%v", r.Value)) {
				return false
			}
		}
	}
	return true
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
func (s *FlagService) Evaluate(ctx context.Context, siteID, flagKey, userID string, userCtx map[string]string) (*EvaluationResult, error) {
	rows, err := nucleus.Query[FeatureFlag](ctx, s.db.SQL(),
		`SELECT flag_id, tenant_id, site_id, flag_key, name, description, flag_type, enabled, rollout_pct,
			COALESCE(variants, '') AS variants, COALESCE(targeting, '') AS targeting, created_at, version
		 FROM feature_flags WHERE site_id = $1 AND flag_key = $2`, siteID, flagKey)
	if err != nil || len(rows) == 0 {
		return &EvaluationResult{Enabled: false}, nil
	}

	flag := rows[0]
	if !flag.Enabled {
		return &EvaluationResult{Enabled: false}, nil
	}

	if flag.RolloutPct < 100 {
		hash := hashUser(flagKey, userID)
		if hash > flag.RolloutPct {
			return &EvaluationResult{Enabled: false}, nil
		}
	}

	if flag.Targeting != "" && flag.Targeting != "[]" && flag.Targeting != "null" {
		var rules []TargetingRule
		if err := json.Unmarshal([]byte(flag.Targeting), &rules); err == nil && len(rules) > 0 {
			if !matchesTargeting(rules, userCtx) {
				return &EvaluationResult{Enabled: false}, nil
			}
		}
	}

	result := &EvaluationResult{Enabled: true}

	if flag.FlagType == "multivariate" && flag.Variants != "" {
		var variants []Variant
		json.Unmarshal([]byte(flag.Variants), &variants)
		if len(variants) > 0 {
			hash := hashUser(flagKey+":variant", userID)
			cumulative := 0
			for _, v := range variants {
				cumulative += v.RolloutPct
				if hash <= cumulative {
					result.Variant = v.Key
					break
				}
			}
			if result.Variant == "" {
				result.Variant = variants[0].Key
			}
		}
	}

	// Fire-and-forget: evaluation tracking is best-effort, must not block response.
	evalID := genID()
	_, _ = s.db.SQL().Exec(ctx,
		`INSERT INTO flag_evaluations (eval_id, tenant_id, site_id, flag_key, user_id, variant, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
		evalID, siteID, flagKey, userID, result.Variant, time.Now().UTC().UnixMilli())

	return result, nil
}

func hashUser(flagKey, userID string) int {
	h := sha256.Sum256([]byte(flagKey + ":" + userID))
	val := int(h[0])<<8 | int(h[1])
	return int(math.Abs(float64(val%100))) + 1
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
