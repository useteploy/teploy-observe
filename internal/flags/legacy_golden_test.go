package flags

// Golden vectors for LEGACY stored configs (flat targeting array, variants
// without payloads). They were generated from the evaluator as it stood
// before condition groups, extra operators and payloads were added, and must
// never change: a diff here means existing users were reassigned.
//
// Regenerate ONLY deliberately: OBSERVE_FLAGS_GOLDEN_UPDATE=1 go test ./internal/flags -run LegacyGolden

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

type legacyFixture struct {
	Name      string `json:"name"`
	FlagKey   string `json:"flag_key"`
	FlagType  string `json:"flag_type"`
	Enabled   bool   `json:"enabled"`
	RolloutPc int    `json:"rollout_pct"`
	Variants  string `json:"variants"`
	Targeting string `json:"targeting"`
}

type legacyVector struct {
	Fixture string            `json:"fixture"`
	UserID  string            `json:"user_id"`
	Context map[string]string `json:"context,omitempty"`
	Enabled bool              `json:"enabled"`
	Variant string            `json:"variant,omitempty"`
	Reason  string            `json:"reason"`
	Detail  string            `json:"detail,omitempty"`
}

const legacyFixturesPath = "testdata/legacy_fixtures.json"
const legacyVectorsPath = "testdata/legacy_golden.json"

func legacyUsers() []string {
	u := []string{"", "alice", "bob", "carol", "user-with-a-really-long-identifier-0123456789"}
	for i := 0; i < 40; i++ {
		u = append(u, fmt.Sprintf("u%d", i))
	}
	return u
}

var legacyContexts = []map[string]string{
	nil,
	{"plan": "pro", "country": "US"},
	{"plan": "free", "country": "DE"},
	{"plan": "pro", "country": "FR", "email": "a@corp.example"},
}

func evalLegacy(t *testing.T, f legacyFixture, user string, c map[string]string) *EvaluationResult {
	t.Helper()
	svc := NewFlagService(nil)
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) {
		return []FeatureFlag{{
			FlagID: "id-" + f.Name, SiteID: "s", FlagKey: f.FlagKey, FlagType: f.FlagType,
			Enabled: f.Enabled, RolloutPct: f.RolloutPc, Variants: f.Variants, Targeting: f.Targeting,
		}}, nil
	}
	res, err := svc.Evaluate(context.Background(), "s", f.FlagKey, user, c)
	if err != nil {
		t.Fatalf("evaluate %s: %v", f.Name, err)
	}
	return res
}

func TestLegacyGolden(t *testing.T) {
	raw, err := os.ReadFile(legacyFixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []legacyFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OBSERVE_FLAGS_GOLDEN_UPDATE") == "1" {
		var out []legacyVector
		for _, f := range fixtures {
			for _, u := range legacyUsers() {
				for _, c := range legacyContexts {
					r := evalLegacy(t, f, u, c)
					out = append(out, legacyVector{f.Name, u, c, r.Enabled, r.Variant, r.Reason, r.Detail})
				}
			}
		}
		b, _ := json.MarshalIndent(out, "", " ")
		if err := os.WriteFile(legacyVectorsPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	rawV, err := os.ReadFile(legacyVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var vectors []legacyVector
	if err := json.Unmarshal(rawV, &vectors); err != nil {
		t.Fatal(err)
	}
	byName := map[string]legacyFixture{}
	for _, f := range fixtures {
		byName[f.Name] = f
	}
	if len(vectors) < 1000 {
		t.Fatalf("golden file suspiciously small: %d vectors", len(vectors))
	}
	for _, v := range vectors {
		f, ok := byName[v.Fixture]
		if !ok {
			t.Fatalf("vector references unknown fixture %q", v.Fixture)
		}
		r := evalLegacy(t, f, v.UserID, v.Context)
		if r.Enabled != v.Enabled || r.Variant != v.Variant || r.Reason != v.Reason || r.Detail != v.Detail {
			t.Fatalf("legacy drift: fixture=%s user=%q ctx=%v got {%v %q %q %q} want {%v %q %q %q}",
				v.Fixture, v.UserID, v.Context, r.Enabled, r.Variant, r.Reason, r.Detail, v.Enabled, v.Variant, v.Reason, v.Detail)
		}
	}
}
