package main

import (
	"context"
	"testing"
)

// TestExperimentSampleSizeHandler pins the O09 design-time endpoint: the
// recommended n matches the scipy-derived golden (0.10 + 0.02 -> 3841/arm),
// defaults are alpha=0.05/power=0.80, and invalid inputs refuse with
// actionable errors rather than a fabricated n.
func TestExperimentSampleSizeHandler(t *testing.T) {
	h := experimentSampleSizeHandler()

	out, err := h(context.Background(), experimentSampleSizeInput{Baseline: 0.10, MDE: 0.02})
	if err != nil {
		t.Fatalf("golden case: %v", err)
	}
	if out.NPerArm != 3841 {
		t.Errorf("p1=0.10 mde=0.02: n/arm = %d, want 3841 (scipy golden)", out.NPerArm)
	}
	if out.Alpha != 0.05 || out.Power != 0.80 {
		t.Errorf("defaults not applied: alpha=%v power=%v", out.Alpha, out.Power)
	}

	// Explicit alpha/power change the answer (and stay self-reported).
	out, err = h(context.Background(), experimentSampleSizeInput{Baseline: 0.10, MDE: 0.02, Alpha: 0.01, Power: 0.9})
	if err != nil {
		t.Fatalf("strict case: %v", err)
	}
	if out.NPerArm <= 3841 {
		t.Errorf("alpha=0.01/power=0.9 must need MORE than alpha=0.05/power=0.8, got %d", out.NPerArm)
	}
	if out.Alpha != 0.01 || out.Power != 0.9 {
		t.Errorf("explicit alpha/power not echoed: %+v", out)
	}

	for name, in := range map[string]experimentSampleSizeInput{
		"baseline zero":    {Baseline: 0, MDE: 0.02},
		"baseline one":     {Baseline: 1, MDE: 0.02},
		"mde over ceiling": {Baseline: 0.9, MDE: 0.2},
		"mde zero":         {Baseline: 0.1, MDE: 0},
		// (alpha=0 and power=0 are the NOT-SET defaults, not refusals.)
		"alpha one":        {Baseline: 0.1, MDE: 0.02, Alpha: 1},
		"alpha negative":   {Baseline: 0.1, MDE: 0.02, Alpha: -0.05},
		"power below half": {Baseline: 0.1, MDE: 0.02, Power: 0.3},
		"power one":        {Baseline: 0.1, MDE: 0.02, Power: 1},
	} {
		if _, err := h(context.Background(), in); err == nil {
			t.Errorf("%s: expected a refusal, got a sample size", name)
		}
	}
}
