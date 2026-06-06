package flags

import "testing"

func TestHashUser_DeterministicAndBounded(t *testing.T) {
	a := hashUser("flag", "user-1")
	b := hashUser("flag", "user-1")
	if a != b {
		t.Fatalf("hashUser not deterministic: %d != %d", a, b)
	}
	for _, u := range []string{"u1", "u2", "u3", "x", "", "really-long-user-id-value"} {
		v := hashUser("f", u)
		if v < 1 || v > 100 {
			t.Fatalf("hashUser(%q) = %d, want 1..100", u, v)
		}
	}
}

func TestMatchesTargeting(t *testing.T) {
	ctx := map[string]string{"plan": "pro", "country": "US"}
	cases := []struct {
		name  string
		rules []TargetingRule
		want  bool
	}{
		{"eq match", []TargetingRule{{Attribute: "plan", Operator: "eq", Value: "pro"}}, true},
		{"eq no match", []TargetingRule{{Attribute: "plan", Operator: "eq", Value: "free"}}, false},
		{"neq", []TargetingRule{{Attribute: "plan", Operator: "neq", Value: "free"}}, true},
		{"in", []TargetingRule{{Attribute: "country", Operator: "in", Value: []any{"US", "CA"}}}, true},
		{"not_in", []TargetingRule{{Attribute: "country", Operator: "not_in", Value: []any{"DE"}}}, true},
		{"missing attribute", []TargetingRule{{Attribute: "age", Operator: "eq", Value: "x"}}, false},
		{"unknown operator fails closed", []TargetingRule{{Attribute: "plan", Operator: "regex", Value: ".*"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchesTargeting(c.rules, ctx); got != c.want {
				t.Fatalf("matchesTargeting = %v, want %v", got, c.want)
			}
		})
	}
}
