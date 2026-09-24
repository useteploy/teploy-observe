package llm

import "testing"

// TestResolveCatalog_PrefixMatch guards the dated/versioned model pricing
// fix: real model strings carry date/version suffixes and must resolve to
// their family price by longest-prefix, not fall to the wrong default.
// The catalog path resolves over the compiled-in fallback entries - the
// same numbers the seeded table carries.
func TestResolveCatalog_PrefixMatch(t *testing.T) {
	entries := builtinFallback()
	// cost with 1000 prompt + 0 completion tokens == the per-1K input price.
	cases := []struct {
		model     string
		wantInput float64
	}{
		{"gpt-4o", 0.005},
		{"gpt-4o-2024-08-06", 0.005},        // dated -> gpt-4o
		{"gpt-4o-mini", 0.00015},            // more specific than gpt-4o
		{"gpt-4o-mini-2024-07-18", 0.00015}, // dated mini
		{"gpt-4-turbo-2024-04-09", 0.01},    // turbo before gpt-4
		{"gpt-4", 0.03},
		{"claude-3-5-sonnet-20241022", 0.003},
		{"claude-3-opus-20240229", 0.015},
		{"some-unknown-model", 0.001}, // conservative default
	}
	for _, c := range cases {
		in, _, matched := resolveCatalog(entries, "", c.model)
		if in != c.wantInput {
			t.Errorf("resolveCatalog(%q) input = %v, want %v", c.model, in, c.wantInput)
		}
		if c.model == "some-unknown-model" && matched {
			t.Errorf("unknown model reported a catalog match")
		}
	}
}

// TestResolveCatalog_ProviderSpecificWins: a provider-scoped row beats the
// provider-agnostic one for the same prefix.
func TestResolveCatalog_ProviderSpecificWins(t *testing.T) {
	entries := []CatalogEntry{
		{Provider: "", ModelPrefix: "gpt-4o", InputPer1k: 0.005, OutputPer1k: 0.015},
		{Provider: "azure", ModelPrefix: "gpt-4o", InputPer1k: 0.006, OutputPer1k: 0.018},
	}
	in, _, matched := resolveCatalog(entries, "azure", "gpt-4o-2024-08-06")
	if !matched || in != 0.006 {
		t.Fatalf("azure resolution = %v (matched %v), want the provider-scoped 0.006", in, matched)
	}
	in, _, _ = resolveCatalog(entries, "openai", "gpt-4o-2024-08-06")
	if in != 0.005 {
		t.Fatalf("other-provider resolution = %v, want the generic 0.005", in)
	}
}
