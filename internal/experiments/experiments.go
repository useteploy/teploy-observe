package experiments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

type Experiment struct {
	ExperimentID string `json:"experiment_id" db:"experiment_id"`
	TenantID     string `json:"-" db:"tenant_id"`
	SiteID       string `json:"site_id" db:"site_id"`
	Name         string `json:"name" db:"name"`
	FlagKey      string `json:"flag_key" db:"flag_key"`
	GoalMetric   string `json:"goal_metric" db:"goal_metric"`
	GoalValue    string `json:"goal_value" db:"goal_value"`
	Status       string `json:"status" db:"status"` // draft, running, completed
	MinSample    string `json:"min_sample" db:"min_sample"`
	StartedAt    string `json:"started_at" db:"started_at"`
	EndedAt      string `json:"ended_at" db:"ended_at"`
	CreatedAt    string `json:"created_at" db:"created_at"`
	Version      string `json:"-" db:"version"`
}

type ExperimentResults struct {
	Experiment Experiment      `json:"experiment"`
	Variants   []VariantResult `json:"variants"`
	Significant bool           `json:"significant"`
	Winner      string         `json:"winner"`
}

type VariantResult struct {
	Variant     string  `json:"variant"`
	Exposures   int64   `json:"exposures"`
	Conversions int64   `json:"conversions"`
	Rate        float64 `json:"rate"`
}

func (s *ExperimentService) Create(ctx context.Context, siteID, name, flagKey, goalMetric, goalValue, minSample string) (*Experiment, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	if minSample == "" {
		minSample = "100"
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, started_at, ended_at, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'draft', $7, '0', '0', $8, $9)`,
		id, siteID, name, flagKey, goalMetric, goalValue, minSample, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create experiment: %w", err)
	}
	return &Experiment{ExperimentID: id, SiteID: siteID, Name: name, FlagKey: flagKey, GoalMetric: goalMetric, GoalValue: goalValue, Status: "draft", MinSample: minSample, CreatedAt: now}, nil
}

func (s *ExperimentService) List(ctx context.Context, siteID string) ([]Experiment, error) {
	return nucleus.Query[Experiment](ctx, s.db.SQL(),
		`SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, started_at, ended_at, created_at, version
		 FROM experiments WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *ExperimentService) Start(ctx context.Context, experimentID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, started_at, ended_at, created_at, version)
		 SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, 'running', min_sample, $2, '0', created_at, $3
		 FROM experiments WHERE experiment_id = $1`,
		experimentID, now, now)
	return err
}

func (s *ExperimentService) Stop(ctx context.Context, experimentID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiments (experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, started_at, ended_at, created_at, version)
		 SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, 'completed', min_sample, started_at, $2, created_at, $3
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

// RecordConversion marks an exposure as converted.
func (s *ExperimentService) RecordConversion(ctx context.Context, experimentID, siteID, userID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO experiment_exposures (exposure_id, tenant_id, experiment_id, site_id, user_id, variant, converted, timestamp)
		 SELECT exposure_id, tenant_id, experiment_id, site_id, user_id, variant, 'true', $3
		 FROM experiment_exposures WHERE experiment_id = $1 AND user_id = $2`,
		experimentID, userID, now)
	return err
}

// Results computes experiment results with statistical significance.
func (s *ExperimentService) Results(ctx context.Context, experimentID, siteID string) (*ExperimentResults, error) {
	exps, err := nucleus.Query[Experiment](ctx, s.db.SQL(),
		`SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample, started_at, ended_at, created_at, version
		 FROM experiments WHERE experiment_id = $1 AND site_id = $2`, experimentID, siteID)
	if err != nil || len(exps) == 0 {
		return nil, fmt.Errorf("experiment not found")
	}

	type row struct {
		Variant   string `db:"variant"`
		Total     string `db:"total"`
		Converted string `db:"converted"`
	}

	rows, err := nucleus.Query[row](ctx, s.db.SQL(),
		`SELECT variant,
			CAST(COUNT(*) AS TEXT) AS total,
			CAST(SUM(CASE WHEN converted = 'true' THEN 1 ELSE 0 END) AS TEXT) AS converted
		 FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2
		 GROUP BY variant`,
		experimentID, siteID)
	if err != nil {
		return nil, err
	}

	var variants []VariantResult
	for _, r := range rows {
		total, _ := strconv.ParseInt(r.Total, 10, 64)
		conv, _ := strconv.ParseInt(r.Converted, 10, 64)
		rate := 0.0
		if total > 0 {
			rate = float64(conv) / float64(total) * 100
		}
		variants = append(variants, VariantResult{Variant: r.Variant, Exposures: total, Conversions: conv, Rate: rate})
	}

	// Simple significance check (chi-squared approximation)
	significant := false
	winner := ""
	if len(variants) >= 2 {
		significant = chiSquaredSignificant(variants)
		best := variants[0]
		for _, v := range variants[1:] {
			if v.Rate > best.Rate {
				best = v
			}
		}
		if significant {
			winner = best.Variant
		}
	}

	return &ExperimentResults{
		Experiment:  exps[0],
		Variants:    variants,
		Significant: significant,
		Winner:      winner,
	}, nil
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
	chiSq := 0.0
	for _, v := range variants {
		if v.Exposures == 0 {
			continue
		}
		expected := float64(v.Exposures) * overallRate
		notExpected := float64(v.Exposures) * (1 - overallRate)
		if expected > 0 {
			chiSq += math.Pow(float64(v.Conversions)-expected, 2) / expected
		}
		if notExpected > 0 {
			chiSq += math.Pow(float64(v.Exposures-v.Conversions)-notExpected, 2) / notExpected
		}
	}

	// df = k-1, p<0.05 critical values: df1=3.84, df2=5.99, df3=7.81
	df := len(variants) - 1
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
