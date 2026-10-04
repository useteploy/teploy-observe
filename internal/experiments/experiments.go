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

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/identity"
)

type ExperimentService struct {
	db *nucleus.Client

	// Optional per-site privacy lookup for EventDistinctID; nil means the
	// helper falls back to the global salt (matches the ingest path for
	// unknown sites).
	privacy PrivacyLookup
	salt    string
}

// PrivacyLookup resolves a site's distinct_id hashing config: the per-site
// salt and whether the site stores raw distinct_ids. ok=false means the
// site is unknown. Satisfied by (*sites.SiteService).PrivacyConfig.
type PrivacyLookup func(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool)

// WithPrivacy installs the per-site lookup and the global fallback salt used
// by EventDistinctID. Returns the receiver for fluent boot-time setup.
func (s *ExperimentService) WithPrivacy(lookup PrivacyLookup, fallbackSalt string) *ExperimentService {
	s.privacy = lookup
	s.salt = fallbackSalt
	return s
}

// EventDistinctID derives, from a raw experiment user id, the exact
// distinct_id the event pipeline stores on events for the same user: the
// HMAC under the site's salt (or the raw value for a raw_distinct_id site),
// resolved the same way internal/ingest does (per-site config, global salt
// fallback for unknown sites).
//
// Identity contract (see docs/IDENTITY_MODEL_ADR.md): exposure and
// conversion rows keep the user_id the caller sent, verbatim, and are joined
// to each other on it. Events carry the hashed distinct_id. Anything that
// must join experiment rows to the events table (a goal measured from
// events, a per-user drill-down) has to hash the experiment user_id with
// this helper first, or send the already-hashed id to the expose/convert
// endpoints from the start. Stored values are deliberately not rewritten
// (D5: no silent history rewrites).
func (s *ExperimentService) EventDistinctID(ctx context.Context, siteID, rawUserID string) string {
	if rawUserID == "" {
		return ""
	}
	salt, rawOptIn := s.salt, false
	if s.privacy != nil {
		if ps, raw, ok := s.privacy(ctx, siteID); ok {
			salt, rawOptIn = ps, raw
		}
	}
	return identity.MaybeHashDistinctID(rawUserID, salt, rawOptIn)
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
	// MetricKind of the primary goal: binary | count | mean.
	MetricKind string `json:"metric_kind"`
	// ContaminatedUsers were exposed to more than one variant inside the
	// running interval. They are excluded from every count and statistic.
	ContaminatedUsers int `json:"contaminated_users"`
	// PeekingWarning is set while any arm is below the planned horizon: the
	// analysis is fixed-horizon, so early p-values are a progress report.
	PeekingWarning string `json:"peeking_warning"`
	// Settings echoes the analysis configuration when it is non-default.
	Settings *ExperimentConfig `json:"settings,omitempty"`
	// Secondary metrics: informational, never gate the winner.
	Secondary              []MetricResult `json:"secondary,omitempty"`
	MultipleComparisonNote string         `json:"multiple_comparison_note,omitempty"`
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
	// Mean/StdDev are the per-exposed-user mean and sample standard
	// deviation for count and mean metrics (absent for binary goals). For
	// those goals Conversions counts users with at least one event.
	Mean   *float64 `json:"mean,omitempty"`
	StdDev *float64 `json:"std_dev,omitempty"`
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

// firstExposureSQL selects the user's earliest exposure (attribution
// choice documented on RecordConversion).
const firstExposureSQL = `SELECT variant FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2 AND user_id = $3
		 ORDER BY timestamp ASC LIMIT 1`

// RecordConversion records that an exposed user converted. The conversion is
// stored in its own append-only table (not a row-copy back into exposures,
// which used to duplicate rows and corrupt counts). The user's variant is
// resolved from their exposure so Results can attribute the conversion without
// a join. A conversion with no prior exposure is ignored.
//
// Attribution is FIRST-exposure: the stored variant is the one the user was
// first assigned to, the same choice every first-touch experiment readout
// makes. It used to read the LATEST exposure, so a user exposed to several
// variants (an allocation change mid-run, a client that re-sends expose with
// a different arm) was attributed to whichever arm happened to expose last,
// and the answer moved as more exposures arrived. Results does not read this
// stored variant: a user exposed to more than one variant is contaminated and
// excluded from every count (reported as contaminated_users).
func (s *ExperimentService) RecordConversion(ctx context.Context, experimentID, siteID, userID string) error {
	type vrow struct {
		Variant string `db:"variant"`
	}
	rows, err := nucleus.Query[vrow](ctx, s.db.SQL(), firstExposureSQL,
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

// maxAnalysisRows bounds the per-user rows Results pulls into memory. A read
// that reaches it fails loudly rather than analysing a silent prefix.
const maxAnalysisRows = 2_000_000

func (s *ExperimentService) loadExperiment(ctx context.Context, experimentID, siteID string) (Experiment, error) {
	exps, err := nucleus.Query[Experiment](ctx, s.db.SQL(),
		`SELECT experiment_id, tenant_id, site_id, name, flag_key, goal_metric, goal_value, status, min_sample,
			COALESCE(variants, '') AS variants,
			started_at, ended_at, created_at, version,
			COALESCE(CAST(conversion_window_hours AS TEXT), '72') AS conversion_window_hours
		 FROM `+experimentsLatest("experiment_id = $1 AND site_id = $2"), experimentID, siteID)
	if err != nil || len(exps) == 0 {
		return Experiment{}, fmt.Errorf("experiment not found")
	}
	return exps[0], nil
}

// Results computes experiment results with the O09 decided analysis
// (2026-09-23): input semantics restrict exposures to the running interval
// and conversions to the declared conversion window after an exposure,
// deduped by user; reporting carries Wilson/Newcombe uncertainty, the
// per-arm horizon gate, SRM detection, the omnibus test with Fisher
// fallback, and Holm-corrected pairwise winner claims.
//
// A user exposed to more than one variant inside the interval is
// contaminated: excluded from every count and reported in
// contaminated_users. Primary and secondary goals of kind count/mean come
// from experiment_metric_events (see config.go).
//
// Late-arrival policy: a conversion recorded after the experiment stopped
// still counts when it falls inside the conversion window of an
// in-interval exposure (the window is anchored at exposure time).
func (s *ExperimentService) Results(ctx context.Context, experimentID, siteID string) (*ExperimentResults, error) {
	return s.ResultsWithOptions(ctx, experimentID, siteID, ResultsOptions{})
}

// ResultsWithOptions is Results with per-request options.
func (s *ExperimentService) ResultsWithOptions(ctx context.Context, experimentID, siteID string, opts ResultsOptions) (*ExperimentResults, error) {
	exp, err := s.loadExperiment(ctx, experimentID, siteID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.LoadConfig(ctx, experimentID, siteID)
	if err != nil {
		return nil, err
	}

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

	type expRow struct {
		User    string `db:"user_id"`
		Variant string `db:"variant"`
		First   string `db:"first_ts"`
	}
	rawExp, err := nucleus.Query[expRow](ctx, s.db.SQL(),
		`SELECT user_id, variant, CAST(MIN(timestamp) AS TEXT) AS first_ts
		 FROM experiment_exposures
		 WHERE experiment_id = $1 AND site_id = $2 AND timestamp >= $3 AND timestamp <= $4
		 GROUP BY user_id, variant LIMIT `+strconv.Itoa(maxAnalysisRows),
		experimentID, siteID, lower, upper)
	if err != nil {
		return nil, err
	}
	if len(rawExp) >= maxAnalysisRows {
		return nil, fmt.Errorf("experiment too large for in-process analysis (>= %d exposed users)", maxAnalysisRows)
	}
	in := resultInputs{Exp: exp, Cfg: cfg, Opts: opts, WindowMs: windowMs,
		Events: map[string][]metricEvent{}, Truncated: map[string]bool{}}
	for _, r := range rawExp {
		ts, _ := strconv.ParseInt(r.First, 10, 64)
		in.Exposures = append(in.Exposures, exposureRow{User: r.User, Variant: r.Variant, First: ts})
	}

	if cfg.MetricKind == KindBinary {
		// Conversions: distinct converting (user, variant), counted only when
		// the conversion falls within the conversion window after an
		// in-interval exposure of that user.
		type convRow struct {
			User    string `db:"user_id"`
			Variant string `db:"variant"`
		}
		rows, err := nucleus.Query[convRow](ctx, s.db.SQL(),
			`SELECT e.user_id AS user_id, e.variant AS variant
			 FROM experiment_exposures e
			 INNER JOIN experiment_conversions c
			   ON c.experiment_id = e.experiment_id AND c.site_id = e.site_id AND c.user_id = e.user_id
			 WHERE e.experiment_id = $1 AND e.site_id = $2 AND e.timestamp >= $3 AND e.timestamp <= $4
			   AND c.timestamp >= e.timestamp AND c.timestamp <= e.timestamp + CAST($5 AS BIGINT)
			 GROUP BY e.user_id, e.variant LIMIT `+strconv.Itoa(maxAnalysisRows),
			experimentID, siteID, lower, upper, windowMs)
		if err != nil {
			return nil, err
		}
		if len(rows) >= maxAnalysisRows {
			return nil, fmt.Errorf("experiment too large for in-process analysis (>= %d converting users)", maxAnalysisRows)
		}
		for _, r := range rows {
			in.BinaryConversions = append(in.BinaryConversions, userVariant{User: r.User, Variant: r.Variant})
		}
	}

	keys := make([]string, 0, 1+len(cfg.Secondary))
	if cfg.MetricKind != KindBinary {
		keys = append(keys, PrimaryMetricKey)
	}
	for _, g := range cfg.Secondary {
		keys = append(keys, g.Key)
	}
	for _, k := range keys {
		ev, truncated, err := s.readMetricEvents(ctx, experimentID, siteID, k, lower, upper+windowMs)
		if err != nil {
			return nil, err
		}
		in.Events[k] = ev
		in.Truncated[k] = truncated
	}

	return computeResults(in), nil
}

// readMetricEvents reads one metric's events in [lower, upperTS]. truncated
// reports that the row cap was reached (the caller refuses a winner).
func (s *ExperimentService) readMetricEvents(ctx context.Context, experimentID, siteID, metric string, lower, upperTS int64) ([]metricEvent, bool, error) {
	type evRow struct {
		User  string `db:"user_id"`
		TS    string `db:"ts"`
		Value string `db:"value"`
	}
	rows, err := nucleus.Query[evRow](ctx, s.db.SQL(),
		`SELECT user_id, CAST(timestamp AS TEXT) AS ts, value
		 FROM experiment_metric_events
		 WHERE experiment_id = $1 AND site_id = $2 AND metric = $3 AND timestamp >= $4 AND timestamp <= $5
		 LIMIT `+strconv.Itoa(maxAnalysisRows),
		experimentID, siteID, metric, lower, upperTS)
	if err != nil {
		return nil, false, err
	}
	out := make([]metricEvent, 0, len(rows))
	for _, r := range rows {
		ts, _ := strconv.ParseInt(r.TS, 10, 64)
		v, perr := strconv.ParseFloat(r.Value, 64)
		if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue // a corrupt value never poisons a mean
		}
		out = append(out, metricEvent{User: r.User, TS: ts, Value: v})
	}
	return out, len(rows) >= maxAnalysisRows, nil
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
