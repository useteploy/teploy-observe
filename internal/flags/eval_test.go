package flags

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func intp(n int) *int { return &n }

func compactJSON(b []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return string(b)
	}
	return out.String()
}

func TestOperators(t *testing.T) {
	ctx := map[string]string{
		"age": "42", "ver": " 7.5 ", "name": "Alice Smith", "empty": "", "junk": "abc",
		"nan": "NaN", "inf": "Inf", "hex": "0x10", "big": "1e400", "neg": "-3",
	}
	cases := []struct {
		name string
		r    TargetingRule
		want bool
	}{
		{"gt true", TargetingRule{"age", "gt", 41.0}, true},
		{"gt equal", TargetingRule{"age", "gt", 42.0}, false},
		{"gte equal", TargetingRule{"age", "gte", 42.0}, true},
		{"lt", TargetingRule{"age", "lt", 43.0}, true},
		{"lte equal", TargetingRule{"age", "lte", 42.0}, true},
		{"lte below", TargetingRule{"age", "lte", 41.0}, false},
		{"numeric string rule value", TargetingRule{"age", "gt", "41"}, true},
		{"whitespace tolerated", TargetingRule{"ver", "gte", 7.5}, true},
		{"negative", TargetingRule{"neg", "lt", 0.0}, true},
		{"unparseable attribute fails closed", TargetingRule{"junk", "gt", 0.0}, false},
		{"unparseable attribute lt also fails closed", TargetingRule{"junk", "lt", 100.0}, false},
		{"NaN attribute rejected", TargetingRule{"nan", "lt", 1.0}, false},
		{"Inf attribute rejected", TargetingRule{"inf", "gt", 1.0}, false},
		{"overflow attribute rejected", TargetingRule{"big", "gt", 1.0}, false},
		{"bare hex not a number", TargetingRule{"hex", "gte", 0.0}, false},
		{"empty attribute fails closed", TargetingRule{"empty", "lt", 5.0}, false},
		{"missing attribute fails closed", TargetingRule{"nope", "gt", 0.0}, false},
		{"bad rule value fails closed", TargetingRule{"age", "gt", "x"}, false},
		{"bool rule value fails closed", TargetingRule{"age", "gt", true}, false},
		{"starts_with", TargetingRule{"name", "starts_with", "Ali"}, true},
		{"starts_with case sensitive", TargetingRule{"name", "starts_with", "ali"}, false},
		{"ends_with", TargetingRule{"name", "ends_with", "Smith"}, true},
		{"ends_with miss", TargetingRule{"name", "ends_with", "Alice"}, false},
		{"starts_with empty value never matches", TargetingRule{"name", "starts_with", ""}, false},
		{"starts_with non-string value never matches", TargetingRule{"name", "starts_with", 1.0}, false},
		{"is_set present", TargetingRule{Attribute: "name", Operator: "is_set"}, true},
		{"is_set empty string", TargetingRule{Attribute: "empty", Operator: "is_set"}, false},
		{"is_set absent", TargetingRule{Attribute: "nope", Operator: "is_set"}, false},
		{"not_set absent", TargetingRule{Attribute: "nope", Operator: "not_set"}, true},
		{"not_set empty string", TargetingRule{Attribute: "empty", Operator: "not_set"}, true},
		{"not_set present", TargetingRule{Attribute: "name", Operator: "not_set"}, false},
		{"neq missing attribute stays closed", TargetingRule{"nope", "neq", "x"}, false},
		{"not_in missing attribute stays closed", TargetingRule{"nope", "not_in", []any{"x"}}, false},
		{"unknown operator", TargetingRule{"name", "regex_match", ".*"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchRule(c.r, ctx); got != c.want {
				t.Fatalf("matchRule(%+v) = %v, want %v", c.r, got, c.want)
			}
		})
	}
}

func TestValidateNewOperators(t *testing.T) {
	bad := []string{
		`[{"attribute":"a","operator":"gt","value":"x"}]`,
		`[{"attribute":"a","operator":"gt","value":true}]`,
		`[{"attribute":"a","operator":"lt","value":null}]`,
		`[{"attribute":"a","operator":"gte","value":"NaN"}]`,
		`[{"attribute":"a","operator":"starts_with","value":""}]`,
		`[{"attribute":"a","operator":"ends_with","value":3}]`,
		`[{"attribute":"a","operator":"regex_match","value":"(a+)+$"}]`,
	}
	for _, b := range bad {
		if _, err := ValidateTargeting(b); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	good := []string{
		`[{"attribute":"a","operator":"gt","value":3}]`,
		`[{"attribute":"a","operator":"lte","value":"3.5"}]`,
		`[{"attribute":"a","operator":"is_set"}]`,
		`[{"attribute":"a","operator":"not_set"}]`,
		`[{"attribute":"a","operator":"starts_with","value":"x"}]`,
	}
	for _, g := range good {
		if _, err := ValidateTargeting(g); err != nil {
			t.Errorf("rejected %s: %v", g, err)
		}
	}
}

// --- groups ---

func mvFlag() FlagDefinition {
	return FlagDefinition{
		Key: "f", Type: "multivariate", Enabled: true, RolloutPct: 100,
		Variants: []Variant{
			{Key: "a", RolloutPct: 50}, {Key: "b", RolloutPct: 50, Payload: json.RawMessage(`{"color":"red"}`)},
		},
	}
}

func TestGroupsFirstMatchWins(t *testing.T) {
	d := mvFlag()
	d.Groups = []ConditionGroup{
		{Key: "staff", Conditions: []TargetingRule{{"email", "ends_with", "@corp.example"}}, Variant: "b"},
		{Key: "pro", Conditions: []TargetingRule{{"plan", "eq", "pro"}}, Variant: "a"},
		{Key: "rest"},
	}
	// Staff who is also pro: first group wins -> b, with its payload.
	r := EvaluateDefinition(d, "u", map[string]string{"email": "x@corp.example", "plan": "pro"})
	if !r.Enabled || r.Group != "staff" || r.Variant != "b" || string(r.Payload) != `{"color":"red"}` {
		t.Fatalf("got %+v", r)
	}
	r = EvaluateDefinition(d, "u", map[string]string{"plan": "pro"})
	if r.Group != "pro" || r.Variant != "a" || r.Payload != nil {
		t.Fatalf("got %+v", r)
	}
	r = EvaluateDefinition(d, "u", nil) // catch-all group, hash-chosen variant
	if !r.Enabled || r.Group != "rest" || r.Variant == "" {
		t.Fatalf("got %+v", r)
	}
	// Reordering changes the winner.
	d.Groups[0], d.Groups[1] = d.Groups[1], d.Groups[0]
	r = EvaluateDefinition(d, "u", map[string]string{"email": "x@corp.example", "plan": "pro"})
	if r.Group != "pro" {
		t.Fatalf("after reorder got %+v", r)
	}
}

func TestGroupsNoMatchAndRolloutFallthrough(t *testing.T) {
	d := FlagDefinition{Key: "f", Type: "boolean", Enabled: true, RolloutPct: 100, Groups: []ConditionGroup{
		{Key: "g", Conditions: []TargetingRule{{"plan", "eq", "pro"}}},
	}}
	r := EvaluateDefinition(d, "u", map[string]string{"plan": "free"})
	if r.Enabled || r.Detail != "targeting" {
		t.Fatalf("no-match: %+v", r)
	}
	// A matched group with rollout 0 admits nobody and reports group_rollout.
	d.Groups[0].RolloutPct = intp(0)
	r = EvaluateDefinition(d, "u", map[string]string{"plan": "pro"})
	if r.Enabled || r.Detail != "group_rollout" {
		t.Fatalf("rollout 0: %+v", r)
	}
	// Rollout miss falls through to a later group.
	d.Groups = append(d.Groups, ConditionGroup{Key: "all"})
	r = EvaluateDefinition(d, "u", map[string]string{"plan": "pro"})
	if !r.Enabled || r.Group != "all" {
		t.Fatalf("fallthrough: %+v", r)
	}
}

func TestGroupBucketsIndependent(t *testing.T) {
	const n = 20000
	var a, b, both int
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("user-%d", i)
		ia := Bucket(groupSalt("f", "A"), u) <= 50
		ib := Bucket(groupSalt("f", "B"), u) <= 50
		if ia {
			a++
		}
		if ib {
			b++
		}
		if ia && ib {
			both++
		}
	}
	// Fixed user ids: the outcome is deterministic. Marginals ~50%, joint
	// ~25% if (and only if) the two salts decorrelate.
	pa, pb, pj := float64(a)/n, float64(b)/n, float64(both)/n
	if pa < 0.48 || pa > 0.52 || pb < 0.48 || pb > 0.52 {
		t.Fatalf("marginals off: %.3f %.3f", pa, pb)
	}
	if pj < 0.235 || pj > 0.265 {
		t.Fatalf("joint %.3f not ~0.25: groups are correlated", pj)
	}
	// Same salt would be perfectly correlated -- guard the guard.
	same := 0
	for i := 0; i < 200; i++ {
		u := fmt.Sprintf("user-%d", i)
		if (Bucket(groupSalt("f", "A"), u) <= 50) == (Bucket(groupSalt("f", "A"), u) <= 50) {
			same++
		}
	}
	if same != 200 {
		t.Fatal("bucket not deterministic")
	}
	// Group salt is distinct from the flag-rollout and variant salts.
	if groupSalt("f", "A") == "f" || groupSalt("f", "A") == "f:variant" {
		t.Fatal("group salt collides")
	}
}

func TestGroupRolloutProportion(t *testing.T) {
	d := FlagDefinition{Key: "prop", Type: "boolean", Enabled: true, RolloutPct: 100,
		Groups: []ConditionGroup{{Key: "g", RolloutPct: intp(30)}}}
	on := 0
	const n = 10000
	for i := 0; i < n; i++ {
		if EvaluateDefinition(d, fmt.Sprintf("p%d", i), nil).Enabled {
			on++
		}
	}
	if on < 2800 || on > 3200 {
		t.Fatalf("30%% group admitted %d of %d", on, n)
	}
}

func TestGroupDefaultKeyIsPositional(t *testing.T) {
	d := FlagDefinition{Key: "f", Type: "boolean", Enabled: true, RolloutPct: 100,
		Groups: []ConditionGroup{{RolloutPct: intp(0)}, {}}}
	r := EvaluateDefinition(d, "u", nil)
	if r.Group != "g1" {
		t.Fatalf("got %+v", r)
	}
}

func TestParseGroupsValidation(t *testing.T) {
	bad := map[string]string{
		"empty groups":    `{"groups":[]}`,
		"unknown field":   `{"groups":[{"conditions":[],"bogus":1}]}`,
		"unknown top":     `{"groups":[{}],"extra":1}`,
		"bad pct":         `{"groups":[{"rollout_pct":101}]}`,
		"negative pct":    `{"groups":[{"rollout_pct":-1}]}`,
		"dup key":         `{"groups":[{"key":"a"},{"key":"a"}]}`,
		"dup default key": `{"groups":[{"key":"g1"},{}]}`,
		"bad op":          `{"groups":[{"conditions":[{"attribute":"a","operator":"regex","value":"x"}]}]}`,
		"no attr":         `{"groups":[{"conditions":[{"operator":"eq","value":"x"}]}]}`,
		"trailing":        `{"groups":[{}]} {}`,
		"long key":        `{"groups":[{"key":"` + strings.Repeat("k", 65) + `"}]}`,
	}
	for name, raw := range bad {
		if _, _, err := parseTargetingConfig(raw); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
	var many []string
	for i := 0; i < maxGroups+1; i++ {
		many = append(many, `{}`)
	}
	if _, _, err := parseTargetingConfig(`{"groups":[` + strings.Join(many, ",") + `]}`); err == nil {
		t.Error("too many groups accepted")
	}
	// A legacy-shaped object (no "groups" key) keeps the legacy error.
	_, _, err := parseTargetingConfig(`{"attribute":"plan"}`)
	if err == nil || !strings.Contains(err.Error(), "targeting: invalid JSON") {
		t.Errorf("legacy object error changed: %v", err)
	}
}

func TestValidateConfigVariantOverride(t *testing.T) {
	tg := `{"groups":[{"variant":"b"}]}`
	vs := `[{"key":"a","rollout_pct":50},{"key":"b","rollout_pct":50}]`
	if err := ValidateConfig("multivariate", vs, tg); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig("boolean", vs, tg); err == nil {
		t.Error("override on non-multivariate accepted")
	}
	if err := ValidateConfig("multivariate", `[{"key":"a","rollout_pct":50}]`, tg); err == nil {
		t.Error("override naming undeclared variant accepted")
	}
}

// --- payloads ---

func TestPayloadValidation(t *testing.T) {
	ok := `[{"key":"a","rollout_pct":100,"payload":{"x":[1,2,{"y":null}]}}]`
	vs, err := ValidateVariants(ok)
	if err != nil || string(vs[0].Payload) != `{"x":[1,2,{"y":null}]}` {
		t.Fatalf("%v %s", err, vs[0].Payload)
	}
	big := `[{"key":"a","rollout_pct":100,"payload":"` + strings.Repeat("x", MaxPayloadBytes) + `"}]`
	if _, err := ValidateVariants(big); err == nil {
		t.Error("oversized payload accepted")
	}
	edge := `[{"key":"a","rollout_pct":100,"payload":"` + strings.Repeat("x", MaxPayloadBytes-2) + `"}]`
	if _, err := ValidateVariants(edge); err != nil {
		t.Errorf("payload at exactly the limit rejected: %v", err)
	}
	// Whitespace does not count against the limit (compacted).
	ws := `[{"key":"a","rollout_pct":100,"payload":{"a":` + strings.Repeat(" ", 5000) + `1}}]`
	if _, err := ValidateVariants(ws); err != nil {
		t.Errorf("whitespace counted: %v", err)
	}
	// Syntactically broken payload is a JSON error for the whole list.
	if _, err := ValidateVariants(`[{"key":"a","rollout_pct":1,"payload":{bad}}]`); err == nil {
		t.Error("invalid JSON accepted")
	}
	// null payload means none.
	d := mvFlag()
	d.Variants = []Variant{{Key: "a", RolloutPct: 100, Payload: json.RawMessage("null")}}
	if r := EvaluateDefinition(d, "u", nil); r.Payload != nil {
		t.Errorf("null payload leaked: %s", r.Payload)
	}
}

func TestInvalidDefinitionAnswersInvalid(t *testing.T) {
	r := EvaluateDefinition(FlagDefinition{Key: "f", Enabled: true, RolloutPct: 100, Invalid: "bad"}, "u", nil)
	if r.Enabled || r.Reason != ReasonInvalid {
		t.Fatalf("%+v", r)
	}
}

func TestBuildConfigQuarantinesInvalid(t *testing.T) {
	rows := []FeatureFlag{
		{FlagKey: "z", FlagType: "boolean", Enabled: true, RolloutPct: 100, Targeting: `[{"attribute":"a","operator":"eq","value":"1"}]`},
		{FlagKey: "bad", FlagType: "boolean", Enabled: true, RolloutPct: 100, Targeting: `{"nope":1}`},
	}
	b := BuildConfig("s", rows, time.Unix(0, 0))
	if len(b.Flags) != 2 || b.Flags[0].Key != "bad" || b.Flags[0].Invalid == "" || len(b.Flags[0].Rules) != 0 {
		t.Fatalf("%+v", b.Flags)
	}
	if b.Flags[1].Invalid != "" || len(b.Flags[1].Rules) != 1 {
		t.Fatalf("%+v", b.Flags[1])
	}
	if b.ETag() != BuildConfig("s", rows, time.Unix(0, 0).Add(5*time.Second)).ETag() {
		t.Error("etag depends on generation time")
	}
	if r := EvaluateDefinition(b.Flags[0], "u", nil); r.Enabled || r.Reason != ReasonInvalid {
		t.Errorf("quarantined definition evaluated: %+v", r)
	}
}

// --- conformance fixture ---

type conformanceCase struct {
	Name    string            `json:"name"`
	Flag    FlagDefinition    `json:"flag"`
	UserID  string            `json:"user_id"`
	Context map[string]string `json:"context,omitempty"`
	Expect  EvaluationResult  `json:"expect"`
}

type bucketVector struct {
	Salt   string `json:"salt"`
	UserID string `json:"user_id"`
	Bucket int    `json:"bucket"`
}

type conformanceFile struct {
	Description string            `json:"description"`
	Buckets     []bucketVector    `json:"bucket_vectors"`
	Cases       []conformanceCase `json:"cases"`
}

const conformancePath = "testdata/conformance.json"

func conformanceInputs() []conformanceCase {
	pro := []TargetingRule{{"plan", "eq", "pro"}}
	mv := mvFlag()
	var cs []conformanceCase
	add := func(name string, f FlagDefinition, user string, c map[string]string) {
		cs = append(cs, conformanceCase{Name: name, Flag: f, UserID: user, Context: c})
	}
	add("disabled", FlagDefinition{Key: "k", Type: "boolean", RolloutPct: 100}, "u1", nil)
	add("boolean on", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100}, "u1", nil)
	for _, u := range []string{"u1", "u2", "u3", "u4", "u5", "u6"} {
		add("rollout 40 "+u, FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 40}, u, nil)
		add("variants "+u, mv, u, nil)
	}
	add("legacy rules match", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Rules: pro}, "u1", map[string]string{"plan": "pro"})
	add("legacy rules miss", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Rules: pro}, "u1", map[string]string{"plan": "free"})
	add("legacy rules missing attr", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Rules: []TargetingRule{{"plan", "neq", "free"}}}, "u1", nil)
	add("numeric gt", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Rules: []TargetingRule{{"age", "gte", 18.0}}}, "u1", map[string]string{"age": "18"})
	add("numeric unparseable", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Rules: []TargetingRule{{"age", "gte", 18.0}}}, "u1", map[string]string{"age": "NaN"})
	add("not_set", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Rules: []TargetingRule{{Attribute: "beta", Operator: "not_set"}}}, "u1", map[string]string{"beta": ""})
	grp := mv
	grp.Groups = []ConditionGroup{
		{Key: "staff", Conditions: []TargetingRule{{"email", "ends_with", "@corp.example"}}, Variant: "b"},
		{Key: "half", Conditions: pro, RolloutPct: intp(50)},
		{Key: "tail", RolloutPct: intp(10)},
	}
	for _, u := range []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8"} {
		add("groups pro "+u, grp, u, map[string]string{"plan": "pro"})
		add("groups other "+u, grp, u, map[string]string{"plan": "free"})
	}
	add("groups staff", grp, "u1", map[string]string{"email": "a@corp.example", "plan": "pro"})
	add("invalid", FlagDefinition{Key: "k", Type: "boolean", Enabled: true, RolloutPct: 100, Invalid: "stored targeting failed validation: x"}, "u1", nil)
	return cs
}

func conformanceBuckets() []bucketVector {
	var out []bucketVector
	for _, s := range []string{"k", "k:variant", "k:grp:half", "flag-with:colons"} {
		for _, u := range []string{"", "u1", "alice", "ünïcode-ユーザー"} {
			out = append(out, bucketVector{s, u, Bucket(s, u)})
		}
	}
	return out
}

func TestConformanceFixture(t *testing.T) {
	if os.Getenv("OBSERVE_FLAGS_GOLDEN_UPDATE") == "1" {
		f := conformanceFile{
			Description: "Flag evaluation test vectors for SDK authors. For each case, evaluate `flag` for `user_id` and `context`; the result must equal `expect` (enabled, variant, payload, group, reason, detail). bucket_vectors pin the hash. Generated by the Go reference evaluator (internal/flags/eval.go); regenerate only deliberately.",
			Buckets:     conformanceBuckets(),
			Cases:       conformanceInputs(),
		}
		for i := range f.Cases {
			f.Cases[i].Expect = EvaluateDefinition(f.Cases[i].Flag, f.Cases[i].UserID, f.Cases[i].Context)
		}
		b, _ := json.MarshalIndent(f, "", " ")
		if err := os.WriteFile(conformancePath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(conformancePath)
	if err != nil {
		t.Fatal(err)
	}
	var f conformanceFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 30 || len(f.Buckets) < 10 {
		t.Fatalf("fixture too small: %d cases %d buckets", len(f.Cases), len(f.Buckets))
	}
	for _, bv := range f.Buckets {
		if got := Bucket(bv.Salt, bv.UserID); got != bv.Bucket {
			t.Errorf("Bucket(%q,%q) = %d, want %d", bv.Salt, bv.UserID, got, bv.Bucket)
		}
	}
	outcomes := map[string]bool{}
	for _, c := range f.Cases {
		got := EvaluateDefinition(c.Flag, c.UserID, c.Context)
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(c.Expect)
		if string(g) != string(w) {
			t.Errorf("%s: got %s want %s", c.Name, g, w)
		}
		outcomes[fmt.Sprintf("%v/%s/%s", got.Enabled, got.Variant, got.Group)] = true
	}
	// The fixture must actually exercise differing outcomes.
	if len(outcomes) < 6 {
		t.Errorf("fixture covers only %d distinct outcomes", len(outcomes))
	}
	// Hand-checked anchors independent of the generator.
	byName := map[string]conformanceCase{}
	for _, c := range f.Cases {
		byName[c.Name] = c
	}
	if e := byName["groups staff"].Expect; e.Group != "staff" || e.Variant != "b" || compactJSON(e.Payload) != `{"color":"red"}` {
		t.Errorf("staff anchor: %+v", e)
	}
	if e := byName["disabled"].Expect; e.Enabled || e.Detail != "flag disabled" {
		t.Errorf("disabled anchor: %+v", e)
	}
	if e := byName["legacy rules missing attr"].Expect; e.Enabled {
		t.Errorf("neq on missing attribute must fail closed: %+v", e)
	}
}
