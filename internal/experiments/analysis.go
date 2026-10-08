package experiments

// O09 analysis layer (decided defaults 2026-09-23, DELEGATED_DECISIONS
// section 4). Everything downstream of the raw per-arm counts lives here so
// the statistics in stats.go stay pure and golden-pinned, and the reporting
// semantics (per-arm horizon gate, SRM diagnostic, omnibus test with Fisher
// fallback, Holm-corrected pairwise winner claims, Wilson/Newcombe
// intervals, and the always-present winner-rule trace) are testable as one
// honest pipeline.

import (
	"fmt"
	"strconv"
)

const (
	// alphaOmnibus is the two-sided significance level for the omnibus test
	// and the Holm family (decided: 0.05, fixed-horizon).
	alphaOmnibus = 0.05
	// alphaSRM is the sample-ratio-mismatch goodness-of-fit level (decided:
	// 0.001 - a 1-in-1000 false alarm is the industry-standard warn
	// threshold; an SRM is a "do not trust these results" diagnostic, and
	// it had better be sure before it shouts).
	alphaSRM = 0.001
	// defaultConversionWindowHours bounds how long after an exposure a
	// conversion still attributes to it.
	defaultConversionWindowHours = 72
)

// SRMDiagnostic is the sample-ratio-mismatch check: observed arm counts vs
// the declared allocation.
type SRMDiagnostic struct {
	Detected  bool    `json:"detected"`
	ChiSquare float64 `json:"chi_square"`
	PValue    float64 `json:"p_value"`
	Note      string  `json:"note,omitempty"`
}

// PairwiseResult is one variant's comparison against the control arm.
type PairwiseResult struct {
	Variant       string  `json:"variant"`
	LiftAbsolute  float64 `json:"lift_absolute"`
	LiftRelative  float64 `json:"lift_relative"`
	CiLow         float64 `json:"ci_low"`
	CiHigh        float64 `json:"ci_high"`
	PValue        float64 `json:"p_value"`
	HolmAdjustedP float64 `json:"holm_adjusted_p"`
	Significant   bool    `json:"significant"` // survives Holm at alphaOmnibus
	UsedFisher    bool    `json:"used_fisher"`
	// T and DF are set for continuous metrics (Welch's t statistic and its
	// Welch-Satterthwaite degrees of freedom); absent for binary arms.
	T  float64 `json:"t,omitempty"`
	DF float64 `json:"df,omitempty"`
}

// AnalysisResult is the honest reporting layer: every gate that stands
// between raw counts and a winner claim, with the reasons visible.
type AnalysisResult struct {
	HorizonMet      bool             `json:"horizon_met"`
	MinArmExposures int64            `json:"min_arm_exposures"`
	MinSamplePerArm int              `json:"min_sample_per_arm"`
	Test            string           `json:"test"` // "chi-square" | "chi-square-yates" | "fisher-exact" | "none"
	TestNote        string           `json:"test_note,omitempty"`
	ChiSquare       float64          `json:"chi_square"`
	DF              int              `json:"df"`
	PValue          float64          `json:"p_value"`
	SRM             SRMDiagnostic    `json:"srm"`
	Pairwise        []PairwiseResult `json:"pairwise_vs_control"`
	WinnerRule      string           `json:"winner_rule"`
	// PlannedSamplePerArm is the design-time per-arm n from the MDE
	// sample-size endpoint, when the experiment declares one. The horizon is
	// the larger of it and min_sample unless HorizonOverridden.
	PlannedSamplePerArm int `json:"planned_sample_per_arm,omitempty"`
	// HorizonOverridden is true when the planned-sample part of the horizon
	// was waived (explicit override). The min_sample floor is never waived.
	HorizonOverridden bool `json:"horizon_overridden,omitempty"`
}

// horizonSpec is the fixed-horizon contract for one analysis: the winner gate
// stays shut until every arm reaches Required().
type horizonSpec struct {
	MinSample int // experiment min_sample, per arm; never waived
	Planned   int // design-time per-arm n (0 = none declared)
	Floor     int // metric-kind floor (continuous metrics)
	Override  bool
}

// Required is the per-arm n at which the horizon is met.
func (h horizonSpec) Required() int {
	n := h.MinSample
	if h.Floor > n {
		n = h.Floor
	}
	if !h.Override && h.Planned > n {
		n = h.Planned
	}
	return n
}

// peekingWarning is the fixed-horizon anti-peeking message, empty once the
// horizon is met or when no arm has data yet.
func peekingWarning(minArm int64, anyData bool, h horizonSpec) string {
	req := h.Required()
	if !anyData || minArm >= int64(req) {
		return ""
	}
	msg := fmt.Sprintf("results requested before the planned sample size (smallest arm %d of %d per arm). "+
		"This analysis is fixed-horizon, not sequential: repeatedly checking p-values and stopping when one dips below 0.05 inflates the false-positive rate. "+
		"Treat these numbers as a progress report and decide at the planned horizon.", minArm, req)
	if h.Override {
		msg += " The planned-sample gate is overridden; the min_sample floor still applies."
	}
	return msg
}

// analyze runs the full decided pipeline over per-arm counts, with the
// declared allocation weights (empty/zero weights mean uniform) and the
// per-arm horizon.
//
// The fixed-horizon anti-peeking control is the horizon gate: no winner
// badge before every arm reaches minSamplePerArm. Estimates and intervals
// are always computed and displayed regardless.
func analyze(arms []VariantResult, weights []float64, minSamplePerArm int) AnalysisResult {
	return analyzeWithHorizon(arms, weights, horizonSpec{MinSample: minSamplePerArm})
}

// analyzeWithHorizon is analyze with the full horizon contract (planned
// sample size and its override).
func analyzeWithHorizon(arms []VariantResult, weights []float64, h horizonSpec) AnalysisResult {
	minSamplePerArm := h.Required()
	res := AnalysisResult{MinSamplePerArm: minSamplePerArm, Test: "none", PlannedSamplePerArm: h.Planned, HorizonOverridden: h.Override && h.Planned > 0}
	k := len(arms)
	if k < 2 {
		res.WinnerRule = "fewer than two arms have exposures"
		return res
	}

	exposures := make([]int64, k)
	conversions := make([]int64, k)
	for i, a := range arms {
		exposures[i] = a.Exposures
		conversions[i] = a.Conversions
	}
	applyHorizonAndSRM(&res, exposures, weights, minSamplePerArm)

	// Omnibus test with Fisher fallback for two arms and small cells.
	omni := chiSquareOmnibus(exposures, conversions)
	res.ChiSquare = omni.Stat
	res.DF = omni.DF
	res.PValue = omni.PValue
	switch {
	case omni.SmallCells && k == 2:
		res.Test = "fisher-exact"
		res.PValue = fisherExact2x2TwoSided(conversions[0], exposures[0]-conversions[0], conversions[1], exposures[1]-conversions[1])
	case omni.SmallCells:
		res.Test = "none"
		res.TestNote = fmt.Sprintf("an expected cell count is below 5 with %d arms; the chi-square approximation is unreliable and the Fisher fallback is only defined for two arms - collect more data", k)
		res.PValue = 1
	case omni.UsedYates:
		res.Test = "chi-square-yates"
	default:
		res.Test = "chi-square"
	}

	// Pairwise vs control with Holm correction across the family.
	control := arms[0]
	rawP := make([]float64, 0, k-1)
	fisherUsed := make([]bool, 0, k-1)
	for _, v := range arms[1:] {
		if control.Exposures == 0 || v.Exposures == 0 {
			rawP = append(rawP, 1)
			fisherUsed = append(fisherUsed, false)
			continue
		}
		var p float64
		usedFisher := false
		pair := chiSquareOmnibus([]int64{control.Exposures, v.Exposures}, []int64{control.Conversions, v.Conversions})
		if pair.SmallCells {
			p = fisherExact2x2TwoSided(control.Conversions, control.Exposures-control.Conversions, v.Conversions, v.Exposures-v.Conversions)
			usedFisher = true
		} else {
			p = pair.PValue
		}
		rawP = append(rawP, p)
		fisherUsed = append(fisherUsed, usedFisher)
	}
	adj := holmAdjusted(rawP)
	for i, v := range arms[1:] {
		lift := v.ConversionRate - control.ConversionRate
		lo, hi := newcombeDifferenceCI(control.Conversions, control.Exposures, v.Conversions, v.Exposures, zAlphaTwoSided005)
		pr := PairwiseResult{
			Variant:       v.Variant,
			LiftAbsolute:  lift,
			CiLow:         lo,
			CiHigh:        hi,
			PValue:        rawP[i],
			HolmAdjustedP: adj[i],
			Significant:   adj[i] <= alphaOmnibus,
			UsedFisher:    fisherUsed[i],
		}
		if control.Conversions > 0 && control.Exposures > 0 {
			pr.LiftRelative = lift / (float64(control.Conversions) / float64(control.Exposures))
		}
		res.Pairwise = append(res.Pairwise, pr)
	}

	// The gates, in order, with the first failure named. Winner requires:
	// horizon met, no SRM, omnibus significance, and the winning arm's
	// pairwise comparison surviving Holm.
	res.WinnerRule = "winner requires: per-arm horizon, no SRM, omnibus p<" +
		strconv.FormatFloat(alphaOmnibus, 'f', -1, 64) + ", winner pairwise vs control surviving Holm"
	switch {
	case !res.HorizonMet:
		res.WinnerRule += fmt.Sprintf(" - waiting for horizon (min arm %d of %d)", res.MinArmExposures, minSamplePerArm)
	case res.SRM.Detected:
		res.WinnerRule += " - SRM detected: " + res.SRM.Note
	case res.Test == "none":
		res.WinnerRule += " - " + res.TestNote
	case res.PValue >= alphaOmnibus:
		res.WinnerRule += fmt.Sprintf(" - omnibus not significant (p=%.4g)", res.PValue)
	}
	return res
}

// applyHorizonAndSRM fills the per-arm horizon gate and the SRM diagnostic,
// which depend only on the exposure counts (shared by binary and continuous).
func applyHorizonAndSRM(res *AnalysisResult, exposures []int64, weights []float64, required int) {
	k := len(exposures)
	res.MinArmExposures = exposures[0]
	for _, n := range exposures {
		if n < res.MinArmExposures {
			res.MinArmExposures = n
		}
	}
	res.HorizonMet = res.MinArmExposures >= int64(required)

	// SRM: goodness-of-fit against the declared allocation.
	if w := append([]float64(nil), weights...); len(w) == k {
		stat, p := srmGoodnessOfFit(exposures, w)
		res.SRM = SRMDiagnostic{ChiSquare: stat, PValue: p}
		if p < alphaSRM {
			res.SRM.Detected = true
			res.SRM.Note = "assignment is broken; do not trust these results"
		}
	}
}

// analyzeContinuous is the continuous-metric (count / mean) counterpart of
// analyzeWithHorizon: the same horizon and SRM gates, then Welch's t-test of
// every arm against the control (arms[0]) with Holm correction across the
// arms. sums are the per-arm value summaries aligned with arms. There is no
// omnibus test: Holm over the pairwise family already controls the
// family-wise error, and the headline p_value is the smallest Holm-adjusted
// p. Higher is better; the winner is the highest-mean arm whose comparison
// survives Holm.
func analyzeContinuous(arms []VariantResult, sums []contSummary, weights []float64, h horizonSpec) AnalysisResult {
	h.Floor = minContinuousN
	res := AnalysisResult{MinSamplePerArm: h.Required(), Test: "none", PlannedSamplePerArm: h.Planned, HorizonOverridden: h.Override && h.Planned > 0}
	k := len(arms)
	if k < 2 {
		res.WinnerRule = "fewer than two arms have exposures"
		return res
	}
	exposures := make([]int64, k)
	for i, a := range arms {
		exposures[i] = a.Exposures
	}
	applyHorizonAndSRM(&res, exposures, weights, res.MinSamplePerArm)
	res.Test = "welch-t"

	rawP := make([]float64, 0, k-1)
	welch := make([]WelchResult, 0, k-1)
	for i := 1; i < k; i++ {
		w := welchTest(sums[0], sums[i], alphaOmnibus)
		welch = append(welch, w)
		rawP = append(rawP, w.PValue)
	}
	adj := holmAdjusted(rawP)
	minAdj := 1.0
	for i, w := range welch {
		pr := PairwiseResult{
			Variant:       arms[i+1].Variant,
			LiftAbsolute:  w.Diff,
			CiLow:         w.CILow,
			CiHigh:        w.CIHigh,
			PValue:        w.PValue,
			HolmAdjustedP: adj[i],
			Significant:   adj[i] <= alphaOmnibus,
			T:             w.T,
			DF:            w.DF,
		}
		if sums[0].Mean != 0 {
			pr.LiftRelative = w.Diff / sums[0].Mean
		}
		if adj[i] < minAdj {
			minAdj = adj[i]
		}
		res.Pairwise = append(res.Pairwise, pr)
	}
	res.PValue = minAdj

	res.WinnerRule = "winner requires: per-arm horizon (min " + strconv.Itoa(res.MinSamplePerArm) +
		" per arm), no SRM, winner's Welch t-test vs control surviving Holm at " + strconv.FormatFloat(alphaOmnibus, 'f', -1, 64)
	switch {
	case !res.HorizonMet:
		res.WinnerRule += fmt.Sprintf(" - waiting for horizon (min arm %d of %d)", res.MinArmExposures, res.MinSamplePerArm)
	case res.SRM.Detected:
		res.WinnerRule += " - SRM detected: " + res.SRM.Note
	case res.PValue > alphaOmnibus:
		res.WinnerRule += fmt.Sprintf(" - no arm distinguishable from control (smallest Holm p=%.4g)", res.PValue)
	}
	return res
}

// winnerFrom returns the winner arm given the analysis and arms (control at
// index 0): the best observed conversion-rate arm whose pairwise-vs-control
// comparison survived Holm. Empty string when no winner may be claimed. If
// the single best-rate arm fails Holm there is no winner: the omnibus fired
// but no specific arm is distinguishable from control.
func winnerFrom(res AnalysisResult, arms []VariantResult) string {
	scores := make([]float64, len(arms))
	for i, a := range arms {
		scores[i] = a.ConversionRate
	}
	return winnerByScore(res, arms, scores)
}

// winnerByScore is winnerFrom over an arbitrary higher-is-better score per arm
// (conversion rate for binary goals, the arm mean for continuous goals).
func winnerByScore(res AnalysisResult, arms []VariantResult, scores []float64) string {
	if !res.HorizonMet || res.SRM.Detected || res.Test == "none" || res.PValue >= alphaOmnibus {
		return ""
	}
	bestIdx := -1
	for i := range arms[1:] {
		if bestIdx == -1 || scores[1+i] > scores[1+bestIdx] {
			bestIdx = i
		}
	}
	if bestIdx == -1 || scores[1+bestIdx] <= scores[0] || !res.Pairwise[bestIdx].Significant {
		return ""
	}
	return arms[1+bestIdx].Variant
}
