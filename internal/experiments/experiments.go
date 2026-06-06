package experiments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
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
}

type ExperimentResults struct {
	Experiment  Experiment      `json:"experiment"`
	Variants    []VariantResult `json:"variants"`
	Significant bool            `json:"significant"`
	Winner      string          `json:"winner"`
}

type VariantResult struct {
	Variant        string  `json:"variant"`
	Exposures      int64   `json:"exposures"`
	Conversions    int64   `json:"conversions"`
	ConversionRate float64 `json:"conversion_rate"`
	// ProbBeatControl is the Bayesian probability that this variant has a higher
	// true conversion rate than the control variant. Zero for the control itself.
	ProbBeatControl float64 `json:"prob_beat_control"`
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
			started_at, ended_at, created_at, version
		 FROM experiments WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *ExperimentService) Start(ctx context.Context, experimentID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, variants, started_at, ended_at, created_at, version)
		 SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, 'running', min_sample, variants, $2, '0', created_at, $3
		 FROM experiments WHERE experiment_id = $1`,
		experimentID, now, now)
	return err
}

func (s *ExperimentService) Stop(ctx context.Context, experimentID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, variants, started_at, ended_at, created_at, version)
		 SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, 'completed', min_sample, variants, started_at, $2, created_at, $3
		 FROM experiments WHERE experiment_id = $1`,
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

// Results computes experiment results with statistical significance.
func (s *ExperimentService) Results(ctx context.Context, experimentID, siteID string) (*ExperimentResults, error) {
	exps, err := nucleus.Query[Experiment](ctx, s.db.SQL(),
		`SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample,
			COALESCE(variants, '') AS variants,
			started_at, ended_at, created_at, version
		 FROM experiments WHERE experiment_id = $1 AND site_id = $2`, experimentID, siteID)
	if err != nil || len(exps) == 0 {
		return nil, fmt.Errorf("experiment not found")
	}

	type cntRow struct {
		Variant string `db:"variant"`
		Count   string `db:"count"`
	}

	// Exposures: distinct users per variant. ORDER BY variant gives a stable
	// slice so the Bayesian control selection below is deterministic.
	expRows, err := nucleus.Query[cntRow](ctx, s.db.SQL(),
		`SELECT variant, CAST(COUNT(DISTINCT user_id) AS TEXT) AS count
		 FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2
		 GROUP BY variant ORDER BY variant`,
		experimentID, siteID)
	if err != nil {
		return nil, err
	}

	// Conversions: distinct converting users per variant.
	convRows, err := nucleus.Query[cntRow](ctx, s.db.SQL(),
		`SELECT variant, CAST(COUNT(DISTINCT user_id) AS TEXT) AS count
		 FROM experiment_conversions
		 WHERE experiment_id = $1 AND site_id = $2
		 GROUP BY variant ORDER BY variant`,
		experimentID, siteID)
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
		variants = append(variants, VariantResult{Variant: r.Variant, Exposures: total, Conversions: conv, ConversionRate: rate})
	}

	// Put the declared control variant (first entry in the experiment's variants
	// JSON, or one keyed "control") at index 0 so the Bayesian comparison is
	// against the true control rather than whatever sorted first.
	orderControlFirst(variants, controlKey(exps[0].Variants))

	// Bayesian: compute probability each variant beats the control (index 0).
	// Uses Beta(1+conv, 1+nonconv) conjugate prior with a 4000-sample Monte Carlo.
	if len(variants) >= 2 {
		computeBayesianProbabilities(variants)
	}

	// Significance/winner are gated on the configured minimum sample so a tiny
	// sample cannot falsely report a winner.
	significant := false
	winner := ""
	if len(variants) >= 2 {
		var totalExposures int64
		minArm := int64(1<<62 - 1)
		for _, v := range variants {
			totalExposures += v.Exposures
			if v.Exposures < minArm {
				minArm = v.Exposures
			}
		}
		minSample := int64(exps[0].MinSample)
		if totalExposures >= minSample && minArm > 0 {
			significant = chiSquaredSignificant(variants)
			if significant {
				best := variants[0]
				for _, v := range variants[1:] {
					if v.ConversionRate > best.ConversionRate {
						best = v
					}
				}
				winner = best.Variant
			}
		}
	}

	return &ExperimentResults{
		Experiment:  exps[0],
		Variants:    variants,
		Significant: significant,
		Winner:      winner,
	}, nil
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

// chiSquaredSignificant performs a simplified chi-squared test for 2+ variants.
func chiSquaredSignificant(variants []VariantResult) bool {
	if len(variants) < 2 {
		return false
	}
	totalExposures := int64(0)
	totalConversions := int64(0)
	for _, v := range variants {
		totalExposures += v.Exposures
		totalConversions += v.Conversions
	}
	if totalExposures == 0 {
		return false
	}

	overallRate := float64(totalConversions) / float64(totalExposures)
	df := len(variants) - 1
	// Yates continuity correction for the 2x2 (df=1) case reduces small-sample
	// over-rejection. Only applied for two variants where it is well-defined.
	yates := df == 1
	chiSq := 0.0
	for _, v := range variants {
		if v.Exposures == 0 {
			continue
		}
		expected := float64(v.Exposures) * overallRate
		notExpected := float64(v.Exposures) * (1 - overallRate)
		if expected > 0 {
			d := math.Abs(float64(v.Conversions) - expected)
			if yates {
				d = math.Max(0, d-0.5)
			}
			chiSq += d * d / expected
		}
		if notExpected > 0 {
			d := math.Abs(float64(v.Exposures-v.Conversions) - notExpected)
			if yates {
				d = math.Max(0, d-0.5)
			}
			chiSq += d * d / notExpected
		}
	}

	criticals := []float64{0, 3.84, 5.99, 7.81, 9.49, 11.07}
	if df < len(criticals) {
		return chiSq > criticals[df]
	}
	return chiSq > 3.84
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
