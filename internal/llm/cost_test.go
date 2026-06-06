package llm

import "testing"

// TestEstimateCost_PrefixMatch guards the dated/versioned model pricing fix:
// real model strings carry date/version suffixes and must resolve to their
// family price by longest-prefix, not fall to the wrong default.
func TestEstimateCost_PrefixMatch(t *testing.T) {
	// cost with 1000 prompt + 0 completion tokens == the per-1K input price.
	cases := []struct {
		model     string
		wantInput float64
	}{
		{"gpt-4o", 0.005},
		{"gpt-4o-2024-08-06", 0.005},        // dated → gpt-4o
		{"gpt-4o-mini", 0.00015},            // more specific than gpt-4o
		{"gpt-4o-mini-2024-07-18", 0.00015}, // dated mini
		{"gpt-4-turbo-2024-04-09", 0.01},    // turbo before gpt-4
		{"gpt-4", 0.03},
		{"claude-3-5-sonnet-20241022", 0.003},
		{"claude-3-opus-20240229", 0.015},
		{"some-unknown-model", 0.001}, // conservative default
	}
	for _, c := range cases {
		got := estimateCost(c.model, 1000, 0)
		if got != c.wantInput {
			t.Errorf("estimateCost(%q, 1000, 0) = %v, want %v", c.model, got, c.wantInput)
		}
	}
}
