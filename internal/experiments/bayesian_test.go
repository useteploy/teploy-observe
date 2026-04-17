package experiments

import "testing"

func TestBayesianWinnerClear(t *testing.T) {
	// Clear winner: variant has 30% CVR (300/1000), control 10% (100/1000).
	// Probability-to-beat should be very close to 1.
	v := []VariantResult{
		{Variant: "control", Exposures: 1000, Conversions: 100},
		{Variant: "A", Exposures: 1000, Conversions: 300},
	}
	computeBayesianProbabilities(v)
	if v[1].ProbBeatControl < 0.99 {
		t.Errorf("expected P(A > control) ≈ 1, got %v", v[1].ProbBeatControl)
	}
	if v[0].ProbBeatControl != 0 {
		t.Errorf("control should be 0, got %v", v[0].ProbBeatControl)
	}
}

func TestBayesianWinnerTight(t *testing.T) {
	// Near-tie: both 10% CVR, so probability should be ~0.5.
	v := []VariantResult{
		{Variant: "control", Exposures: 1000, Conversions: 100},
		{Variant: "A", Exposures: 1000, Conversions: 100},
	}
	computeBayesianProbabilities(v)
	if v[1].ProbBeatControl < 0.35 || v[1].ProbBeatControl > 0.65 {
		t.Errorf("expected P(A > control) ~0.5, got %v", v[1].ProbBeatControl)
	}
}

func TestBayesianLoser(t *testing.T) {
	// A is worse: 5% vs 15% control.
	v := []VariantResult{
		{Variant: "control", Exposures: 1000, Conversions: 150},
		{Variant: "A", Exposures: 1000, Conversions: 50},
	}
	computeBayesianProbabilities(v)
	if v[1].ProbBeatControl > 0.01 {
		t.Errorf("expected P(A > control) ~0, got %v", v[1].ProbBeatControl)
	}
}
