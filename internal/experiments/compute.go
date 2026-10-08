package experiments

// The pure core of Results: raw per-user rows in, ExperimentResults out. The
// service fetches the rows (SQL, unverified offline); everything that decides
// a number or a gate happens here so it is unit-testable without a database.

import (
	"encoding/json"
	"sort"
)

// exposureRow is one user's exposure to one variant inside the running
// interval, with the earliest such exposure time (epoch ms).
type exposureRow struct {
	User    string
	Variant string
	First   int64
}

// userVariant is a distinct (user, variant) conversion attribution row.
type userVariant struct {
	User    string
	Variant string
}

// metricEvent is one count/mean/secondary observation.
type metricEvent struct {
	User  string
	TS    int64
	Value float64
}

// ResultsOptions are per-request analysis options.
type ResultsOptions struct {
	// AllowEarly waives the planned-sample part of the horizon for this
	// request (the winner gate and the peeking warning). The min_sample floor
	// is never waived. The experiment's allow_early_winner setting has the
	// same effect persistently.
	AllowEarly bool
}

type resultInputs struct {
	Exp      Experiment
	Cfg      ExperimentConfig
	Opts     ResultsOptions
	WindowMs int64
	// Exposures: distinct (user, variant) rows in the running interval.
	Exposures []exposureRow
	// BinaryConversions: distinct (user, variant) rows of the legacy
	// experiment_conversions join, used when the primary kind is binary.
	BinaryConversions []userVariant
	// Events by metric key ("primary" or a secondary key).
	Events map[string][]metricEvent
	// Truncated marks metric keys whose event read hit the row cap.
	Truncated map[string]bool
}

// MetricResult is one evaluated goal (primary-continuous detail or a
// secondary). Secondary metrics are informational and never gate the winner.
type MetricResult struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	// Role is "primary" or "secondary".
	Role string `json:"role"`
	// GatesWinner is true only for the primary metric.
	GatesWinner bool            `json:"gates_winner"`
	Variants    []VariantResult `json:"variants"`
	Analysis    AnalysisResult  `json:"analysis"`
	Note        string          `json:"note,omitempty"`
}

// cohortOf collapses exposure rows into one entry per user. A user exposed to
// more than one variant is contaminated: excluded from every statistic and
// counted separately. Returns the clean cohort and the contaminated count.
func cohortOf(rows []exposureRow) (map[string]exposureRow, int) {
	first := make(map[string]exposureRow, len(rows))
	bad := make(map[string]bool)
	for _, r := range rows {
		prev, ok := first[r.User]
		switch {
		case !ok:
			first[r.User] = r
		case prev.Variant != r.Variant:
			bad[r.User] = true
		case r.First < prev.First:
			first[r.User] = r
		}
	}
	for u := range bad {
		delete(first, u)
	}
	return first, len(bad)
}

// qualifyingValues maps each cohort user with at least one event inside its
// attribution window [first exposure, first exposure + window] to its metric
// value: 1 for binary, the event count for count, the value sum for mean.
func qualifyingValues(kind string, events []metricEvent, cohort map[string]exposureRow, windowMs int64) map[string]float64 {
	out := make(map[string]float64)
	for _, e := range events {
		c, ok := cohort[e.User]
		if !ok || e.TS < c.First || e.TS > c.First+windowMs {
			continue
		}
		switch kind {
		case KindBinary:
			out[e.User] = 1
		case KindCount:
			out[e.User]++
		default:
			out[e.User] += e.Value
		}
	}
	return out
}

// evaluateMetric builds the per-arm results and analysis for one goal over
// the clean cohort. variants is the ordered arm list (control first) with
// Exposures filled; vals maps users to qualifying values (see
// qualifyingValues). Binary goals use the chi-square/Fisher pipeline;
// count/mean goals use Welch with optional winsorizing.
func evaluateMetric(kind string, winsorPct float64, variants []VariantResult, cohort map[string]exposureRow, vals map[string]float64, weights []float64, h horizonSpec) ([]VariantResult, AnalysisResult) {
	arms := append([]VariantResult(nil), variants...)
	idx := make(map[string]int, len(arms))
	for i, a := range arms {
		idx[a.Variant] = i
	}
	for u := range vals {
		arms[idx[cohort[u].Variant]].Conversions++
	}
	for i := range arms {
		a := &arms[i]
		if a.Exposures > 0 {
			a.ConversionRate = float64(a.Conversions) / float64(a.Exposures)
		}
		a.WilsonLow, a.WilsonHigh = wilsonInterval(a.Conversions, a.Exposures, zAlphaTwoSided005)
	}
	if kind == KindBinary {
		return arms, analyzeWithHorizon(arms, weights, h)
	}

	// Continuous: every exposed user contributes (zero when no events).
	perArm := make([][]float64, len(arms))
	// Sorted user order keeps float summation, and so the output, identical
	// between identical calls.
	users := make([]string, 0, len(cohort))
	for u := range cohort {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		i := idx[cohort[u].Variant]
		perArm[i] = append(perArm[i], vals[u])
	}
	if winsorPct > 0 {
		winsorizeArms(perArm, winsorPct)
	}
	sums := make([]contSummary, len(arms))
	for i := range arms {
		sums[i] = summarize(perArm[i])
		m, sd := sums[i].Mean, sums[i].stdDev()
		arms[i].Mean, arms[i].StdDev = &m, &sd
	}
	an := analyzeContinuous(arms, sums, weights, h)
	return arms, an
}

// continuousScores returns each arm's mean for winnerByScore.
func continuousScores(arms []VariantResult) []float64 {
	s := make([]float64, len(arms))
	for i, a := range arms {
		if a.Mean != nil {
			s[i] = *a.Mean
		}
	}
	return s
}

func computeResults(in resultInputs) *ExperimentResults {
	exp, cfg := in.Exp, in.Cfg
	cohort, contaminated := cohortOf(in.Exposures)

	// Per-variant clean exposure counts, sorted by variant name (the legacy
	// ORDER BY variant) before the declared control is moved to the front.
	counts := make(map[string]int64)
	var declared []struct {
		Key        string  `json:"key"`
		RolloutPct float64 `json:"rollout_pct"`
		Weight     float64 `json:"weight"`
	}
	invalidDeclaration := exp.Variants != "" && ValidateExperimentDefinition(exp.Variants, 0) != nil
	json.Unmarshal([]byte(exp.Variants), &declared)
	keyedWeights := make(map[string]float64)
	for _, v := range declared {
		counts[v.Key] = 0
		w := v.RolloutPct
		if v.Weight > 0 {
			w = v.Weight
		}
		keyedWeights[v.Key] = w
	}
	unknown := false
	for _, c := range cohort {
		if len(declared) > 0 {
			if _, ok := counts[c.Variant]; !ok {
				unknown = true
			}
		}
		counts[c.Variant]++
	}
	names := make([]string, 0, len(counts))
	for v := range counts {
		names = append(names, v)
	}
	sort.Strings(names)
	base := make([]VariantResult, len(names))
	for i, v := range names {
		base[i] = VariantResult{Variant: v, Exposures: counts[v]}
	}
	orderControlFirst(base, controlKey(exp.Variants))

	minSample := exp.MinSample
	if minSample <= 0 {
		minSample = 100
	}
	h := horizonSpec{
		MinSample: minSample,
		Planned:   cfg.PlannedSamplePerArm,
		Override:  in.Opts.AllowEarly || cfg.AllowEarlyWinner,
	}
	var weights []float64
	if len(declared) > 0 {
		weights = make([]float64, len(base))
		for i, a := range base {
			weights[i] = keyedWeights[a.Variant]
		}
	}

	// Primary goal.
	var primaryVals map[string]float64
	if cfg.MetricKind == KindBinary {
		primaryVals = make(map[string]float64)
		for _, c := range in.BinaryConversions {
			if cc, ok := cohort[c.User]; ok && cc.Variant == c.Variant {
				primaryVals[c.User] = 1
			}
		}
	} else {
		primaryVals = qualifyingValues(cfg.MetricKind, in.Events[PrimaryMetricKey], cohort, in.WindowMs)
	}
	variants, analysis := evaluateMetric(cfg.MetricKind, cfg.WinsorizePct, base, cohort, primaryVals, weights, h)

	var winner string
	var significant bool
	if cfg.MetricKind == KindBinary {
		// Bayesian: probability each variant beats the control (index 0),
		// Beta(1+conv, 1+nonconv) with a Monte Carlo seeded from the experiment
		// id and the data (see bayesianSeed). Display-only: it never gates the
		// winner (2026-09-23 decision: the fixed-horizon frequentist gates do).
		if len(variants) >= 2 {
			computeBayesianProbabilities(variants, bayesianSeed(exp.ExperimentID, variants))
		}
		winner = winnerFrom(analysis, variants)
		significant = analysis.HorizonMet && !analysis.SRM.Detected &&
			analysis.Test != "none" && analysis.PValue < alphaOmnibus
	} else {
		winner = winnerByScore(analysis, variants, continuousScores(variants))
		significant = analysis.HorizonMet && !analysis.SRM.Detected &&
			analysis.Test != "none" && analysis.PValue < alphaOmnibus
	}
	if unknown || invalidDeclaration {
		winner, significant = "", false
		analysis.WinnerRule += " - unknown exposure arm or invalid declaration; assignment is invalid"
	}
	if in.Truncated[PrimaryMetricKey] {
		winner, significant = "", false
		analysis.WinnerRule += " - metric events were truncated at the read cap; results are not reliable"
	}

	res := &ExperimentResults{
		Experiment:        exp,
		Variants:          variants,
		Significant:       significant,
		Winner:            winner,
		Analysis:          analysis,
		MetricKind:        cfg.MetricKind,
		ContaminatedUsers: contaminated,
		PeekingWarning:    peekingWarning(analysis.MinArmExposures, len(variants) >= 2, horizonSpec{MinSample: minSample, Planned: h.Planned, Floor: floorFor(cfg.MetricKind), Override: h.Override}),
	}
	if cfg.MetricKind != KindBinary || cfg.WinsorizePct > 0 || cfg.PlannedSamplePerArm > 0 || cfg.AllowEarlyWinner || len(cfg.Secondary) > 0 {
		c := cfg
		res.Settings = &c
	}

	// Secondary goals: same cohort, same machinery, never gating.
	for _, g := range cfg.Secondary {
		vals := qualifyingValues(g.Kind, in.Events[g.Key], cohort, in.WindowMs)
		arms, an := evaluateMetric(g.Kind, cfg.WinsorizePct, base, cohort, vals, weights, h)
		an.WinnerRule = "secondary metric - informational only, does not gate the winner; " + an.WinnerRule
		mr := MetricResult{
			Key: g.Key, Name: g.Name, Kind: g.Kind, Role: "secondary",
			Variants: arms, Analysis: an,
			Note: "secondary: excluded from winner selection; uncorrected across metrics",
		}
		if in.Truncated[g.Key] {
			mr.Note += "; events truncated at the read cap, results are not reliable"
		}
		res.Secondary = append(res.Secondary, mr)
	}
	res.MultipleComparisonNote = cfg.multipleComparisonNote()
	return res
}

func floorFor(kind string) int {
	if kind == KindBinary {
		return 0
	}
	return minContinuousN
}
