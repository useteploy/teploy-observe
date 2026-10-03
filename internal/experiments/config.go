package experiments

// Per-experiment analysis configuration (migration 063). The experiments
// table cannot take new columns (ALTER ADD COLUMN corrupts populated tables,
// open P1 L9), so metric kind, secondary goals, winsorizing and the planned
// horizon live in an append-only experiment_settings table holding one JSON
// document per write; the newest write wins. An experiment with no settings
// row behaves exactly as before 063: one binary conversion goal.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// Metric kinds. Binary is "did the user ever convert"; count is the number of
// events per exposed user; mean is the summed event value per exposed user
// (revenue per exposed user, so non-purchasers contribute zero).
const (
	KindBinary = "binary"
	KindCount  = "count"
	KindMean   = "mean"

	// PrimaryMetricKey is the metric_events key of the primary goal when it is
	// continuous. A binary primary keeps using experiment_conversions.
	PrimaryMetricKey = "primary"

	maxSecondaryGoals = 3
	maxMetricValueAbs = 1e12
	maxPlannedSample  = 1_000_000_000
)

var metricKeyRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// SecondaryGoal is one informational metric evaluated beside the primary. It
// never gates the winner.
type SecondaryGoal struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
}

// ExperimentConfig is the analysis configuration document.
type ExperimentConfig struct {
	// MetricKind of the primary goal: binary (default), count or mean.
	MetricKind string `json:"metric_kind"`
	// WinsorizePct clips continuous values to the pooled [p, 100-p]
	// percentiles; 0 (default) is off. Valid range (0, 50).
	WinsorizePct float64 `json:"winsorize_pct,omitempty"`
	// PlannedSamplePerArm is the design-time n from the sample-size endpoint.
	// Until every arm reaches it, results carry a peeking warning and no
	// winner is declared (unless AllowEarlyWinner).
	PlannedSamplePerArm int `json:"planned_sample_per_arm,omitempty"`
	// AllowEarlyWinner is the explicit override of the planned-sample gate.
	AllowEarlyWinner bool `json:"allow_early_winner,omitempty"`
	// Secondary goals, at most three.
	Secondary []SecondaryGoal `json:"secondary_goals,omitempty"`
}

// DefaultConfig is the pre-063 behaviour.
func DefaultConfig() ExperimentConfig { return ExperimentConfig{MetricKind: KindBinary} }

func validKind(k string) bool { return k == KindBinary || k == KindCount || k == KindMean }

// Normalize fills defaults and validates. It returns a copy.
func (c ExperimentConfig) Normalize() (ExperimentConfig, error) {
	if c.MetricKind == "" {
		c.MetricKind = KindBinary
	}
	if !validKind(c.MetricKind) {
		return c, fmt.Errorf("metric_kind must be binary, count or mean")
	}
	if math.IsNaN(c.WinsorizePct) || c.WinsorizePct < 0 || c.WinsorizePct >= 50 {
		return c, fmt.Errorf("winsorize_pct must be 0 (off) or in (0, 50)")
	}
	if c.WinsorizePct > 0 && c.MetricKind == KindBinary {
		return c, fmt.Errorf("winsorize_pct applies to count and mean metrics only")
	}
	if c.PlannedSamplePerArm < 0 || c.PlannedSamplePerArm > maxPlannedSample {
		return c, fmt.Errorf("planned_sample_per_arm out of range")
	}
	if len(c.Secondary) > maxSecondaryGoals {
		return c, fmt.Errorf("at most %d secondary goals", maxSecondaryGoals)
	}
	seen := map[string]bool{PrimaryMetricKey: true}
	sec := make([]SecondaryGoal, len(c.Secondary))
	for i, g := range c.Secondary {
		if !metricKeyRE.MatchString(g.Key) {
			return c, fmt.Errorf("secondary goal key must match %s", metricKeyRE.String())
		}
		if seen[g.Key] {
			return c, fmt.Errorf("duplicate or reserved secondary goal key %q", g.Key)
		}
		seen[g.Key] = true
		if g.Kind == "" {
			g.Kind = KindBinary
		}
		if !validKind(g.Kind) {
			return c, fmt.Errorf("secondary goal %q: kind must be binary, count or mean", g.Key)
		}
		if len(g.Name) > 120 {
			return c, fmt.Errorf("secondary goal %q: name too long", g.Key)
		}
		sec[i] = g
	}
	c.Secondary = sec
	return c, nil
}

// SecondaryEvaluationsNote is the multiple-comparison caveat attached to any
// result that evaluates more than one metric.
func (c ExperimentConfig) multipleComparisonNote() string {
	if len(c.Secondary) == 0 {
		return ""
	}
	return fmt.Sprintf("%d secondary metric(s) are shown beside the primary. Only the primary gates the winner. "+
		"Holm correction applies across the arms of each metric, not across metrics, so a secondary metric crossing p<0.05 "+
		"is exploratory: with several metrics some will cross by chance. Treat secondary results as hypotheses for a follow-up experiment.",
		len(c.Secondary))
}

// ---------------------------------------------------------------------------
// Storage.

// SaveConfig validates and appends a settings document for an experiment of
// the site. The experiment must exist for that site.
func (s *ExperimentService) SaveConfig(ctx context.Context, experimentID, siteID string, cfg ExperimentConfig) (ExperimentConfig, error) {
	cfg, err := cfg.Normalize()
	if err != nil {
		return cfg, err
	}
	if _, err := s.loadExperiment(ctx, experimentID, siteID); err != nil {
		return cfg, err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO experiment_settings (setting_id, tenant_id, experiment_id, site_id, config, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5)`,
		genID(), experimentID, siteID, string(doc), time.Now().UTC().UnixMilli())
	if err != nil {
		return cfg, fmt.Errorf("save experiment settings: %w", err)
	}
	return cfg, nil
}

// LoadConfig returns the newest settings document, or DefaultConfig when the
// experiment has none (every pre-063 experiment).
func (s *ExperimentService) LoadConfig(ctx context.Context, experimentID, siteID string) (ExperimentConfig, error) {
	type row struct {
		Config string `db:"config"`
	}
	rows, err := nucleus.Query[row](ctx, s.db.SQL(),
		`SELECT config FROM experiment_settings
		 WHERE experiment_id = $1 AND site_id = $2
		 ORDER BY timestamp DESC, setting_id DESC LIMIT 1`,
		experimentID, siteID)
	if err != nil {
		return DefaultConfig(), err
	}
	if len(rows) == 0 {
		return DefaultConfig(), nil
	}
	var cfg ExperimentConfig
	if err := json.Unmarshal([]byte(rows[0].Config), &cfg); err != nil {
		return DefaultConfig(), fmt.Errorf("experiment settings corrupt: %w", err)
	}
	return cfg.Normalize()
}

// RecordMetric appends one metric event (a count/mean/secondary observation)
// for an exposed user. Like RecordConversion, an event from a user with no
// exposure is ignored. value must be finite; binary and count goals ignore it
// at analysis time but it is still validated and stored.
func (s *ExperimentService) RecordMetric(ctx context.Context, experimentID, siteID, userID, metric string, value float64) error {
	if metric != PrimaryMetricKey && !metricKeyRE.MatchString(metric) {
		return fmt.Errorf("metric must match %s", metricKeyRE.String())
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > maxMetricValueAbs {
		return fmt.Errorf("value must be a finite number with magnitude <= %g", maxMetricValueAbs)
	}
	type vrow struct {
		Variant string `db:"variant"`
	}
	rows, err := nucleus.Query[vrow](ctx, s.db.SQL(),
		`SELECT variant FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2 AND user_id = $3 LIMIT 1`,
		experimentID, siteID, userID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO experiment_metric_events (event_id, tenant_id, experiment_id, site_id, user_id, metric, value, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7)`,
		genID(), experimentID, siteID, userID, metric, strconv.FormatFloat(value, 'g', -1, 64), time.Now().UTC().UnixMilli())
	return err
}
