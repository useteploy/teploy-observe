package flags

import (
	"context"
	"testing"
)

func TestAuditFlagWriteContract(t *testing.T) {
	for _, kind := range []string{"string", "number", "json", "other"} {
		if err := ValidateConfig(kind, `[{"key":"a","rollout_pct":100}]`, ""); err == nil {
			t.Fatalf("accepted %s", kind)
		}
	}
	if err := ValidateConfig("multivariate", `[{"key":"a","value":"x","weight":100}]`, ""); err == nil {
		t.Fatal("inert fields accepted")
	}
	svc := NewFlagService(nil)
	for _, pct := range []int{-1, 101} {
		if _, err := svc.Create(context.Background(), "s", "f", "f", "", "boolean", "", "", pct); err == nil {
			t.Fatal(pct)
		}
	}
	def := FlagDefinition{Key: "f", Type: "boolean", Enabled: true, RolloutPct: 0}
	for _, id := range []string{"a", "b", "c"} {
		r := EvaluateDefinition(def, id, nil)
		if r.Enabled {
			t.Fatal(r)
		}
	}
}
