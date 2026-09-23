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
}

// analyze runs the full decided pipeline over per-arm counts, with the
// declared allocation weights (empty/zero weights mean uniform) and the
// per-arm horizon.
//
// The fixed-horizon anti-peeking control is the horizon gate: no winner
// badge before every arm reaches minSamplePerArm. Estimates and intervals
// are always computed and displayed regardless.
func analyze(arms []VariantResult, weights []float64, minSamplePerArm int) AnalysisResult {
	res := AnalysisResult{MinSamplePerArm: minSamplePerArm, Test: "none"}
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
	res.MinArmExposures = exposures[0]
	for _, n := range exposures {
		if n < res.MinArmExposures {
			res.MinArmExposures = n
		}
	}
	res.HorizonMet = res.MinArmExposures >= int64(minSamplePerArm)

	// SRM: goodness-of-fit against the declared allocation.
	if w := append([]float64(nil), weights...); len(w) == k {
		stat, p := srmGoodnessOfFit(exposures, w)
		res.SRM = SRMDiagnostic{ChiSquare: stat, PValue: p}
		if p < alphaSRM {
			res.SRM.Detected = true
			res.SRM.Note = "assignment is broken; do not trust these results"
		}
	}

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
		lift := float64(v.Conversions)/float64(v.Exposures) - float64(control.Conversions)/float64(control.Exposures)
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

// winnerFrom returns the winner arm given the analysis and arms (control at
// index 0): the best observed conversion-rate arm whose pairwise-vs-control
// comparison survived Holm. Empty string when no winner may be claimed. If
// the single best-rate arm fails Holm there is no winner: the omnibus fired
// but no specific arm is distinguishable from control.
func winnerFrom(res AnalysisResult, arms []VariantResult) string {
	if !res.HorizonMet || res.SRM.Detected || res.Test == "none" || res.PValue >= alphaOmnibus {
		return ""
	}
	bestIdx := -1
	for i, v := range arms[1:] {
		if bestIdx == -1 || v.ConversionRate > arms[1+bestIdx].ConversionRate {
			bestIdx = i
		}
	}
	if bestIdx == -1 || !res.Pairwise[bestIdx].Significant {
		return ""
	}
	return arms[1+bestIdx].Variant
}
