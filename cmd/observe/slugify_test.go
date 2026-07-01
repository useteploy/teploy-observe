package main

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"usage-sync":          "usage-sync",
		"Usage Sync":          "usage-sync",
		"usage-aggregation-daily": "usage-aggregation-daily",
		"  Reset Daily Quotas  ": "reset-daily-quotas",
		"Model_Discovery!!!":  "model-discovery",
		"":                    "",
	}
	for input, want := range cases {
		if got := slugify(input); got != want {
			t.Errorf("slugify(%q) = %q, want %q", input, got, want)
		}
	}
}
