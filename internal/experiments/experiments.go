package experiments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

type ExperimentService struct {
	db *nucleus.Client
}

func NewExperimentService(db *nucleus.Client) *ExperimentService {
	return &ExperimentService{db: db}
}

// Experiment is the domain type with typed fields.
type Experiment struct {
	ExperimentID string    `json:"experiment_id"`
	SiteID       string    `json:"site_id"`
	Name         string    `json:"name"`
	FlagKey      string    `json:"flag_key"`
	GoalMetric   string    `json:"goal_metric"`
	GoalValue    string    `json:"goal_value"`
	Status       string    `json:"status"`
	MinSample    int       `json:"min_sample"`
	Variants     string    `json:"variants"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	CreatedAt    time.Time `json:"created_at"`
	// ConversionWindowHours bounds how long after an exposure a conversion
	// still attributes (O09 input semantics; 048, default 72).
	ConversionWindowHours int `json:"conversion_window_hours"`
}

type ExperimentResults struct {
	Experiment  Experiment      `json:"experiment"`
	Variants    []VariantResult `json:"variants"`
	Significant bool            `json:"significant"`
	Winner      string          `json:"winner"`
	// Analysis is the O09 honest reporting layer: the horizon gate, SRM
	// diagnostic, omnibus test, Holm-corrected pairwise comparisons, and the
	// winner-rule trace. Significant/Winner above remain as the compact
	// summary: Significant = horizon met AND no SRM AND omnibus p < 0.05;
	// Winner additionally requires the winning arm's pairwise comparison to
	// survive Holm.
	Analysis AnalysisResult `json:"analysis"`
}

type VariantResult struct {
	Variant        string  `json:"variant"`
	Exposures      int64   `json:"exposures"`
	Conversions    int64   `json:"conversions"`
	ConversionRate float64 `json:"conversion_rate"`
	// ProbBeatControl is the Bayesian probability that this variant has a higher
	// true conversion rate than the control variant. Zero for the control itself.
	ProbBeatControl float64 `json:"prob_beat_control"`
	// WilsonLow/WilsonHigh are the 95% Wilson score interval bounds on the
	// arm's conversion rate (O09: estimates always carry uncertainty).
	WilsonLow  float64 `json:"wilson_low"`
	WilsonHigh float64 `json:"wilson_high"`
}

func (s *ExperimentService) Create(ctx context.Context, siteID, name, flagKey, goalMetric, goalValue, variants string, minSample int) (*Experiment, error) {
	id := genID()
	now := time.Now().UTC()
	nowMs := strconv.FormatInt(now.UnixMilli(), 10)
	if minSample <= 0 {
		minSample = 100
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, variants, started_at, ended_at, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'draft', $7, $8, '0', '0', $9, $10)`,
		id, siteID, name, flagKey, goalMetric, goalValue, strconv.Itoa(minSample), variants, nowMs, nowMs,
	)
	if err != nil {
		return nil, fmt.Errorf("create experiment: %w", err)
	}
	return &Experiment{
		ExperimentID: id, SiteID: siteID, Name: name, FlagKey: flagKey,
		GoalMetric: goalMetric, GoalValue: goalValue, Status: "draft",
		MinSample: minSample, Variants: variants, CreatedAt: now,
	}, nil
}

func (s *ExperimentService) List(ctx context.Context, siteID string) ([]Experiment, error) {
	return nucleus.Query[Experiment](ctx, s.db.SQL(),
		`SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample,
			COALESCE(variants, '') AS variants,
			started_at, ended_at, created_at, version,
			COALESCE(CAST(conversion_window_hours AS TEXT), '72') AS conversion_window_hours
		 FROM `+experimentsLatest("site_id = $1")+` ORDER BY created_at DESC`, siteID)
}

func (s *ExperimentService) Start(ctx context.Context, experimentID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// started_at carries the wall-clock start; the VERSION is the monotonic
	// stamp (the 70f6eff version-tie defect), so a same-millisecond
	// create+start resolves running.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, variants, started_at, ended_at, created_at, version)
		 SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, 'running', min_sample, variants, $2, '0', created_at,
		        GREATEST(CAST($3 AS BIGINT), version + 1)
		 FROM `+experimentsLatest("experiment_id = $1"),
		experimentID, now, now)
	return err
}

func (s *ExperimentService) Stop(ctx context.Context, experimentID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// ended_at carries the wall-clock end; same monotonic version stamp as
	// Start, so a start+stop inside one millisecond resolves completed.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, variants, started_at, ended_at, created_at, version)
		 SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, 'completed', min_sample, variants, started_at, $2, created_at,
		        GREATEST(CAST($3 AS BIGINT), version + 1)
		 FROM `+experimentsLatest("experiment_id = $1"),
		experimentID, now, now)
	return err
}

// RecordExposure records that a user was exposed to a variant.
func (s *ExperimentService) RecordExposure(ctx context.Context, experimentID, siteID, userID, variant string) error {
	id := genID()
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiment_exposures (exposure_id, tenant_id, experiment_id, site_id, user_id, variant, converted, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, 'false', $6)`,
		id, experimentID, siteID, userID, variant, time.Now().UTC().UnixMilli())
	return err
}

// RecordConversion records that an exposed user converted. The conversion is
// stored in its own append-only table (not a row-copy back into exposures,
// which used to duplicate rows and corrupt counts). The user's variant is
// resolved from their exposure so Results can attribute the conversion without
// a join. A conversion with no prior exposure is ignored.
func (s *ExperimentService) RecordConversion(ctx context.Context, experimentID, siteID, userID string) error {
	type vrow struct {
		Variant string `db:"variant"`
	}
	rows, err := nucleus.Query[vrow](ctx, s.db.SQL(),
		`SELECT variant FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2 AND user_id = $3
		 ORDER BY timestamp DESC LIMIT 1`,
		experimentID, siteID, userID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil // no exposure to convert
	}

	id := genID()
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO experiment_conversions (conversion_id, tenant_id, experiment_id, site_id, user_id, variant, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
		id, experimentID, siteID, userID, rows[0].Variant, time.Now().UTC().UnixMilli())
	return err
}

// Results computes experiment results with the O09 decided analysis
// (2026-09-23): input semantics restrict exposures to the running interval
// and conversions to the declared conversion window after an exposure,
// deduped by user; reporting carries Wilson/Newcombe uncertainty, the
// per-arm horizon gate, SRM detection, the omnibus test with Fisher
// fallback, and Holm-corrected pairwise winner claims.
//
// Late-arrival policy: a conversion recorded after the experiment stopped
// still counts when it falls inside the conversion window of an
// in-interval exposure (the window is anchored at exposure time).
func (s *ExperimentService) Results(ctx context.Context, experimentID, siteID string) (*ExperimentResults, error) {
	exps, err := nucleus.Query[Experiment](ctx, s.db.SQL(),
		`SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample,
			COALESCE(variants, '') AS variants,
			started_at, ended_at, created_at, version,
			COALESCE(CAST(conversion_window_hours AS TEXT), '72') AS conversion_window_hours
		 FROM `+experimentsLatest("experiment_id = $1 AND site_id = $2"), experimentID, siteID)
	if err != nil || len(exps) == 0 {
		return nil, fmt.Errorf("experiment not found")
	}
	exp := exps[0]

	// Input semantics bounds: exposures count only inside the running
	// interval. started_at = 0 (draft, never started) keeps historical
	// pre-start rows; the upper bound is ended_at when completed, else now.
	lower := timeMsOrZero(exp.StartedAt)
	upper := timeMsOrZero(exp.EndedAt)
	if upper <= 0 {
		upper = time.Now().UTC().UnixMilli()
	}
	windowMs := int64(defaultConversionWindowHours) * 3600 * 1000
	if exp.ConversionWindowHours > 0 {
		windowMs = int64(exp.ConversionWindowHours) * 3600 * 1000
	}

	type cntRow struct {
		Variant string `db:"variant"`
		Count   string `db:"count"`
	}

	// Exposures: distinct users per variant, restricted to the running
	// interval. ORDER BY variant gives a stable slice so the Bayesian
	// control selection below is deterministic.
	expRows, err := nucleus.Query[cntRow](ctx, s.db.SQL(),
		`SELECT variant, CAST(COUNT(DISTINCT user_id) AS TEXT) AS count
		 FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2 AND timestamp >= $3 AND timestamp <= $4
		 GROUP BY variant ORDER BY variant`,
		experimentID, siteID, lower, upper)
	if err != nil {
		return nil, err
	}

	// Conversions: distinct converting users per variant, counted only when
	// the conversion falls within the conversion window after an
	// in-interval exposure of that user (any of them - the attributed
	// variant is the exposure whose window contains it).
	convRows, err := nucleus.Query[cntRow](ctx, s.db.SQL(),
		`SELECT e.variant AS variant, CAST(COUNT(DISTINCT e.user_id) AS TEXT) AS count
		 FROM experiment_exposures e
		 INNER JOIN experiment_conversions c
		   ON c.experiment_id = e.experiment_id AND c.site_id = e.site_id AND c.user_id = e.user_id
		 WHERE e.experiment_id = $1 AND e.site_id = $2 AND e.timestamp >= $3 AND e.timestamp <= $4
		   AND c.timestamp >= e.timestamp AND c.timestamp <= e.timestamp + CAST($5 AS BIGINT)
		 GROUP BY e.variant ORDER BY e.variant`,
		experimentID, siteID, lower, upper, windowMs)
	if err != nil {
		return nil, err
	}
	convByVariant := make(map[string]int64, len(convRows))
	for _, r := range convRows {
		c, _ := strconv.ParseInt(r.Count, 10, 64)
		convByVariant[r.Variant] = c
	}

	var variants []VariantResult
	for _, r := range expRows {
		total, _ := strconv.ParseInt(r.Count, 10, 64)
		conv := convByVariant[r.Variant]
		rate := 0.0
		if total > 0 {
			rate = float64(conv) / float64(total)
		}
		lo, hi := wilsonInterval(conv, total, zAlphaTwoSided005)
		variants = append(variants, VariantResult{
			Variant: r.Variant, Exposures: total, Conversions: conv,
			ConversionRate: rate, WilsonLow: lo, WilsonHigh: hi,
		})
	}

	// Put the declared control variant (first entry in the experiment's variants
	// JSON, or one keyed "control") at index 0 so the Bayesian comparison is
	// against the true control rather than whatever sorted first.
	orderControlFirst(variants, controlKey(exp.Variants))

	// Bayesian: compute probability each variant beats the control (index 0).
	// Uses Beta(1+conv, 1+nonconv) conjugate prior with a 4000-sample Monte Carlo.
	// Displayed as a labeled estimate; it never gates the winner (2026-09-23
	// decision: the fixed-horizon frequentist gates do).
	if len(variants) >= 2 {
		computeBayesianProbabilities(variants)
	}

	// O09 analysis: per-arm horizon, SRM, omnibus + Fisher fallback, Holm
	// pairwise. MinSample is per-arm (a 9,900/100 split must not declare).
	minSample := exp.MinSample
	if minSample <= 0 {
		minSample = 100
	}
	analysis := analyze(variants, allocationWeights(exp.Variants, len(variants)), minSample)
	winner := winnerFrom(analysis, variants)
	significant := analysis.HorizonMet && !analysis.SRM.Detected &&
		analysis.Test != "none" && analysis.PValue < alphaOmnibus

	return &ExperimentResults{
		Experiment:  exp,
		Variants:    variants,
		Significant: significant,
		Winner:      winner,
		Analysis:    analysis,
	}, nil
}

// timeMsOrZero maps the TEXT epoch-ms columns ('0' for unset) through their
// time.Time representation, treating pre-epoch/zero stamps as unset.
func timeMsOrZero(t time.Time) int64 {
	if t.IsZero() || t.Before(time.Unix(0, 0)) {
		return 0
	}
	return t.UnixMilli()
}

// allocationWeights extracts the declared arm allocation from the variants
// JSON ("rollout_pct" or "weight" per entry, normalized by the caller's GoF)
// and returns a slice matching the arms' order when every arm is covered;
// nil when the JSON does not declare weights for all arms (uniform assumed).
func allocationWeights(variantsJSON string, armCount int) []float64 {
	if variantsJSON == "" || armCount == 0 {
		return nil
	}
	var vs []struct {
		Key        string  `json:"key"`
		RolloutPct float64 `json:"rollout_pct"`
		Weight     float64 `json:"weight"`
	}
	if err := json.Unmarshal([]byte(variantsJSON), &vs); err != nil || len(vs) != armCount {
		return nil
	}
	weights := make([]float64, len(vs))
	for i, v := range vs {
		if v.Weight > 0 {
			weights[i] = v.Weight
		} else {
			weights[i] = v.RolloutPct
		}
	}
	return weights
}

// controlKey returns the key of the control variant: the one keyed "control"
// if present, else the first declared variant. Empty if variantsJSON is blank
// or unparseable.
func controlKey(variantsJSON string) string {
	if variantsJSON == "" {
		return ""
	}
	var vs []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(variantsJSON), &vs); err != nil || len(vs) == 0 {
		return ""
	}
	for _, v := range vs {
		if v.Key == "control" {
			return v.Key
		}
	}
	return vs[0].Key
}

// orderControlFirst moves the variant matching key to index 0, preserving the
// relative order of the rest. No-op if key is empty or not found.
func orderControlFirst(variants []VariantResult, key string) {
	if key == "" {
		return
	}
	for i, v := range variants {
		if v.Variant == key {
			if i != 0 {
				ctrl := variants[i]
				copy(variants[1:i+1], variants[0:i])
				variants[0] = ctrl
			}
			return
		}
	}
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
