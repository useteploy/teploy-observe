package experiments

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

const hourMs = int64(3600 * 1000)

// addUsers appends n clean users of a variant with first exposure at ts.
func addUsers(in *resultInputs, prefix, variant string, n int, ts int64) {
	for i := 0; i < n; i++ {
		in.Exposures = append(in.Exposures, exposureRow{User: fmt.Sprintf("%s%d", prefix, i), Variant: variant, First: ts})
	}
}

func baseInputs(minSample int) resultInputs {
	return resultInputs{
		Exp: Experiment{ExperimentID: "e1", SiteID: "s", MinSample: minSample,
			Variants: `[{"key":"control"},{"key":"b"}]`},
		Cfg:       DefaultConfig(),
		WindowMs:  72 * hourMs,
		Events:    map[string][]metricEvent{},
		Truncated: map[string]bool{},
	}
}

// Legacy binary experiments: the JSON is the pre-063 output byte for byte
// (golden produced by the pre-change code) once the added fields and the
// Bayesian display value (new seed) are set aside.
func TestLegacyBinaryJSONUnchanged(t *testing.T) {
	raw, err := os.ReadFile("testdata/legacy_results.golden")
	if err != nil {
		t.Fatal(err)
	}
	goldens := strings.Split(strings.TrimSpace(string(raw)), "\n")
	cases := [][][2]int64{{{4000, 400}, {4000, 480}}, {{3000, 300}, {3000, 360}, {3000, 330}}, {{50, 5}, {50, 9}}}
	names := []string{"control", "b", "c"}
	if len(goldens) != len(cases) {
		t.Fatalf("golden count %d", len(goldens))
	}
	for ci, arms := range cases {
		in := baseInputs(100)
		if len(arms) == 3 {
			in.Exp.Variants = `[{"key":"control"},{"key":"b"},{"key":"c"}]`
		}
		for ai, a := range arms {
			addUsers(&in, names[ai], names[ai], int(a[0]), 1000)
			for i := int64(0); i < a[1]; i++ {
				in.BinaryConversions = append(in.BinaryConversions, userVariant{User: fmt.Sprintf("%s%d", names[ai], i), Variant: names[ai]})
			}
		}
		res := computeResults(in)
		got := normalizeJSON(t, mustJSON(t, res), true)
		want := normalizeJSON(t, []byte(goldens[ci]), false)
		want["experiment"].(map[string]any)["variants"] = in.Exp.Variants
		{
			// The golden generator passed nil weights (no SRM block); the
			// real pipeline derives uniform weights from the variants JSON.
			delete(got["analysis"].(map[string]any), "srm")
			delete(want["analysis"].(map[string]any), "srm")
		}
		if !approxEqualJSON(got, want) {
			t.Fatalf("case %d: legacy JSON changed\n got: %s\nwant: %s", ci, mustJSON(t, got), mustJSON(t, want))
		}
	}
}

// approxEqualJSON is DeepEqual over parsed JSON with a 1e-12 relative
// tolerance on float leaves. The gc compiler fuses floating-point multiply
// add on arm64 but not amd64, so transcendent-derived leaves (p-values)
// differ by 1 ULP between architectures; the pin's contract is structural
// and numeric identity, not the last mantissa bit.
func approxEqualJSON(a, b any) bool {
	fa, aok := a.(float64)
	fb, bok := b.(float64)
	if aok || bok {
		return aok && bok && math.Abs(fa-fb) <= 1e-12*math.Max(math.Abs(fa), math.Abs(fb))
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok || bok {
		if !aok || !bok || len(am) != len(bm) {
			return false
		}
		for k, v := range am {
			if !approxEqualJSON(v, bm[k]) {
				return false
			}
		}
		return true
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok || bok {
		if !aok || !bok || len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !approxEqualJSON(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// normalizeJSON drops the fields added in 063 (when stripAdded) and the
// seed-dependent Bayesian display value.
func normalizeJSON(t *testing.T, b []byte, stripAdded bool) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if stripAdded {
		for _, k := range []string{"metric_kind", "contaminated_users", "peeking_warning", "settings", "secondary", "multiple_comparison_note"} {
			delete(m, k)
		}
	}
	for _, v := range m["variants"].([]any) {
		delete(v.(map[string]any), "prob_beat_control")
	}
	return m
}

func TestLegacyAddedFields(t *testing.T) {
	in := baseInputs(100)
	addUsers(&in, "c", "control", 150, 1000)
	addUsers(&in, "b", "b", 150, 1000)
	res := computeResults(in)
	m := normalizeJSON(t, mustJSON(t, res), false)
	if m["metric_kind"] != "binary" || m["contaminated_users"].(float64) != 0 || m["peeking_warning"] != "" {
		t.Fatalf("added fields: %v %v %v", m["metric_kind"], m["contaminated_users"], m["peeking_warning"])
	}
	if _, ok := m["settings"]; ok {
		t.Fatal("default config must not echo settings")
	}
}

func TestContaminatedUsersExcluded(t *testing.T) {
	in := baseInputs(1)
	in.Exp.Variants = `[{"key":"control"},{"key":"b"},{"key":"c"}]`
	addUsers(&in, "c", "control", 40, 1000)
	addUsers(&in, "b", "b", 40, 1000)
	// 5 users exposed to both arms (and one to all three) are contaminated.
	for i := 0; i < 5; i++ {
		u := fmt.Sprintf("x%d", i)
		in.Exposures = append(in.Exposures,
			exposureRow{User: u, Variant: "control", First: 1000},
			exposureRow{User: u, Variant: "b", First: 2000})
		in.BinaryConversions = append(in.BinaryConversions,
			userVariant{User: u, Variant: "control"}, userVariant{User: u, Variant: "b"})
	}
	in.Exposures = append(in.Exposures,
		exposureRow{User: "y", Variant: "control", First: 1}, exposureRow{User: "y", Variant: "b", First: 2}, exposureRow{User: "y", Variant: "c", First: 3})
	// repeated same-arm exposure rows are not contamination.
	in.Exposures = append(in.Exposures, exposureRow{User: "c0", Variant: "control", First: 5000})
	res := computeResults(in)
	if res.ContaminatedUsers != 6 {
		t.Fatalf("contaminated = %d, want 6", res.ContaminatedUsers)
	}
	for _, v := range res.Variants {
		if v.Variant != "c" && v.Exposures != 40 || v.Variant == "c" && v.Exposures != 0 {
			t.Fatalf("%s exposures = %d, contaminated users must be excluded", v.Variant, v.Exposures)
		}
		if v.Conversions != 0 {
			t.Fatalf("%s conversions = %d, contaminated conversions must be excluded", v.Variant, v.Conversions)
		}
	}
	if len(res.Variants) != 3 || res.Analysis.HorizonMet || res.Winner != "" {
		t.Fatalf("zero-exposure declared arm must remain and gate winner: %+v", res)
	}
}

func TestBayesianSeedDeterministicAndIndependent(t *testing.T) {
	mk := func() []VariantResult {
		return []VariantResult{{Variant: "control", Exposures: 500, Conversions: 50}, {Variant: "a", Exposures: 500, Conversions: 56}}
	}
	a, b := mk(), mk()
	computeBayesianProbabilities(a, bayesianSeed("exp-1", a))
	computeBayesianProbabilities(b, bayesianSeed("exp-1", b))
	if a[1].ProbBeatControl != b[1].ProbBeatControl {
		t.Fatal("identical calls must be identical")
	}
	if bayesianSeed("exp-1", mk()) == bayesianSeed("exp-2", mk()) {
		t.Fatal("different experiments must get different seeds")
	}
	d := mk()
	d[1].Conversions++
	if bayesianSeed("exp-1", mk()) == bayesianSeed("exp-1", d) {
		t.Fatal("different data must change the seed")
	}
	if bayesianSeed("exp-1", mk()) == 42 {
		t.Fatal("seed must not be the legacy constant")
	}
	// probabilities stay in [0,1].
	if a[1].ProbBeatControl < 0 || a[1].ProbBeatControl > 1 {
		t.Fatal("prob out of range")
	}
}

func TestPeekingWarningAndOverride(t *testing.T) {
	in := baseInputs(10)
	in.Cfg.PlannedSamplePerArm = 400
	addUsers(&in, "c", "control", 200, 1000)
	addUsers(&in, "b", "b", 200, 1000)
	// Strong effect so the winner gate is the only thing in the way.
	for i := 0; i < 200; i++ {
		if i < 20 {
			in.BinaryConversions = append(in.BinaryConversions, userVariant{User: fmt.Sprintf("c%d", i), Variant: "control"})
		}
		if i < 90 {
			in.BinaryConversions = append(in.BinaryConversions, userVariant{User: fmt.Sprintf("b%d", i), Variant: "b"})
		}
	}
	res := computeResults(in)
	if res.PeekingWarning == "" || !strings.Contains(res.PeekingWarning, "200 of 400") {
		t.Fatalf("expected peeking warning, got %q", res.PeekingWarning)
	}
	if res.Winner != "" || res.Significant || res.Analysis.HorizonMet {
		t.Fatalf("winner must be refused before the planned sample: %+v", res.Analysis)
	}
	// Per-request override.
	in.Opts.AllowEarly = true
	over := computeResults(in)
	if over.Winner != "b" || !over.Analysis.HorizonOverridden || over.PeekingWarning != "" {
		t.Fatalf("override: winner=%q overridden=%v warn=%q", over.Winner, over.Analysis.HorizonOverridden, over.PeekingWarning)
	}
	// Config override has the same effect.
	in.Opts.AllowEarly = false
	in.Cfg.AllowEarlyWinner = true
	if computeResults(in).Winner != "b" {
		t.Fatal("allow_early_winner must waive the planned gate")
	}
	// The min_sample floor is never waived.
	in.Exp.MinSample = 500
	if r := computeResults(in); r.Winner != "" || r.PeekingWarning == "" {
		t.Fatalf("min_sample floor must hold under override: %+v", r.Analysis)
	}
}

// continuousInputs builds a mean-metric experiment with per-user purchases.
func continuousInputs(n int, effect float64) resultInputs {
	in := baseInputs(1)
	in.Exp.Variants = `[{"key":"control"},{"key":"b"},{"key":"c"}]`
	in.Cfg.MetricKind = KindMean
	for _, arm := range []struct {
		name string
		mult float64
	}{{"control", 1}, {"b", effect}, {"c", 1}} {
		addUsers(&in, arm.name, arm.name, n, 1000)
		for i := 0; i < n; i++ {
			// deterministic spread, 60% buyers
			if i%5 < 3 {
				in.Events[PrimaryMetricKey] = append(in.Events[PrimaryMetricKey],
					metricEvent{User: fmt.Sprintf("%s%d", arm.name, i), TS: 2000, Value: arm.mult * (10 + float64(i%7))})
			}
		}
	}
	return in
}

func TestContinuousPrimaryMatchesWelch(t *testing.T) {
	in := continuousInputs(100, 1.5)
	res := computeResults(in)
	if res.MetricKind != KindMean || res.Analysis.Test != "welch-t" {
		t.Fatalf("kind/test: %s %s", res.MetricKind, res.Analysis.Test)
	}
	// Reference: per-user arrays recomputed independently.
	arr := func(name string, mult float64) []float64 {
		v := make([]float64, 100)
		for i := range v {
			if i%5 < 3 {
				v[i] = mult * (10 + float64(i%7))
			}
		}
		return v
	}
	want := welchTest(summarize(arr("control", 1)), summarize(arr("b", 1.5)), 0.05)
	var got *PairwiseResult
	for i := range res.Analysis.Pairwise {
		if res.Analysis.Pairwise[i].Variant == "b" {
			got = &res.Analysis.Pairwise[i]
		}
	}
	if got == nil || relErr(got.PValue, want.PValue) > 1e-9 || math.Abs(got.LiftAbsolute-want.Diff) > 1e-9 || relErr(got.DF, want.DF) > 1e-9 {
		t.Fatalf("pairwise %+v want %+v", got, want)
	}
	if res.Winner != "b" || !res.Significant {
		t.Fatalf("expected winner b: winner=%q sig=%v rule=%s", res.Winner, res.Significant, res.Analysis.WinnerRule)
	}
	// Holm across 2 arms: adjusted p = min(1, 2*p) for the smallest.
	if got.HolmAdjustedP < got.PValue-1e-15 || got.HolmAdjustedP > 1 {
		t.Fatalf("holm: %+v", got)
	}
	for _, v := range res.Variants {
		if v.Mean == nil || v.StdDev == nil {
			t.Fatalf("continuous variants carry mean/std_dev: %+v", v)
		}
		if v.ProbBeatControl != 0 {
			t.Fatal("no Bayesian value for continuous metrics")
		}
	}
}

func TestContinuousMinSampleGate(t *testing.T) {
	// 20 per arm is below the continuous floor of 30 even with min_sample 1.
	res := computeResults(continuousInputs(20, 5))
	if res.Analysis.HorizonMet || res.Winner != "" || res.Significant {
		t.Fatalf("n=20 must not declare: %+v", res.Analysis)
	}
	if res.Analysis.MinSamplePerArm != minContinuousN {
		t.Fatalf("required n = %d", res.Analysis.MinSamplePerArm)
	}
}

func TestContinuousWindowAndCount(t *testing.T) {
	in := baseInputs(1)
	in.Cfg.MetricKind = KindCount
	addUsers(&in, "c", "control", 40, 1000)
	addUsers(&in, "b", "b", 40, 1000)
	in.Events[PrimaryMetricKey] = []metricEvent{
		{User: "c0", TS: 1500, Value: 999},               // counts as 1 event (value ignored)
		{User: "c0", TS: 1600, Value: 1},                 // second event
		{User: "c1", TS: 999, Value: 1},                  // before exposure: ignored
		{User: "c2", TS: 1000 + 72*hourMs + 1, Value: 1}, // after window: ignored
		{User: "c3", TS: 1000 + 72*hourMs, Value: 1},     // boundary inclusive
		{User: "ghost", TS: 1500, Value: 1},              // never exposed: ignored
	}
	res := computeResults(in)
	ctrl := res.Variants[0]
	if ctrl.Variant != "control" || ctrl.Conversions != 2 {
		t.Fatalf("responders: %+v", ctrl)
	}
	if want := 3.0 / 40; math.Abs(*ctrl.Mean-want) > 1e-12 {
		t.Fatalf("mean = %v want %v", *ctrl.Mean, want)
	}
}

func TestWinsorizeOptionRobustToOutlier(t *testing.T) {
	in := continuousInputs(100, 1.0)
	// One absurd outlier in control swamps the mean.
	in.Events[PrimaryMetricKey] = append(in.Events[PrimaryMetricKey], metricEvent{User: "control0", TS: 2000, Value: 1e6})
	raw := computeResults(in)
	in.Cfg.WinsorizePct = 1
	win := computeResults(in)
	if *raw.Variants[0].Mean < 1000 {
		t.Fatalf("test setup: outlier should dominate raw mean, got %v", *raw.Variants[0].Mean)
	}
	if *win.Variants[0].Mean > 100 {
		t.Fatalf("winsorized mean should be tame, got %v", *win.Variants[0].Mean)
	}
	if win.Settings == nil || win.Settings.WinsorizePct != 1 {
		t.Fatal("settings echoed")
	}
}

func TestSecondaryNeverGatesWinner(t *testing.T) {
	in := baseInputs(1)
	in.Cfg.Secondary = []SecondaryGoal{{Key: "signup", Kind: KindBinary}, {Key: "rev", Name: "Revenue", Kind: KindMean}}
	addUsers(&in, "c", "control", 100, 1000)
	addUsers(&in, "b", "b", 100, 1000)
	// Primary: no difference. Secondary signup: huge difference.
	for i := 0; i < 100; i++ {
		if i < 10 {
			in.BinaryConversions = append(in.BinaryConversions,
				userVariant{User: fmt.Sprintf("c%d", i), Variant: "control"}, userVariant{User: fmt.Sprintf("b%d", i), Variant: "b"})
		}
		if i < 5 {
			in.Events["signup"] = append(in.Events["signup"], metricEvent{User: fmt.Sprintf("c%d", i), TS: 1500})
		}
		if i < 80 {
			in.Events["signup"] = append(in.Events["signup"], metricEvent{User: fmt.Sprintf("b%d", i), TS: 1500})
		}
	}
	res := computeResults(in)
	if res.Winner != "" {
		t.Fatalf("secondary must not produce a winner: %q", res.Winner)
	}
	if len(res.Secondary) != 2 || res.Secondary[0].Role != "secondary" || res.Secondary[0].GatesWinner {
		t.Fatalf("secondary labelling: %+v", res.Secondary)
	}
	sig := res.Secondary[0]
	if len(sig.Analysis.Pairwise) != 1 || !sig.Analysis.Pairwise[0].Significant {
		t.Fatalf("secondary signup should still be evaluated: %+v", sig.Analysis)
	}
	if !strings.Contains(sig.Analysis.WinnerRule, "does not gate the winner") || res.MultipleComparisonNote == "" {
		t.Fatal("labelling / multiple-comparison note missing")
	}
	// Secondary metric uses the same clean cohort (exposures equal primary).
	if sig.Variants[0].Exposures != res.Variants[0].Exposures {
		t.Fatal("secondary must share the cohort")
	}
}

func TestConfigNormalize(t *testing.T) {
	ok, err := ExperimentConfig{}.Normalize()
	if err != nil || ok.MetricKind != KindBinary {
		t.Fatalf("default: %+v %v", ok, err)
	}
	bad := []ExperimentConfig{
		{MetricKind: "median"},
		{MetricKind: KindMean, WinsorizePct: 50},
		{MetricKind: KindMean, WinsorizePct: -1},
		{MetricKind: KindBinary, WinsorizePct: 5},
		{PlannedSamplePerArm: -1},
		{Secondary: []SecondaryGoal{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}}},
		{Secondary: []SecondaryGoal{{Key: "Bad Key"}}},
		{Secondary: []SecondaryGoal{{Key: "primary"}}},
		{Secondary: []SecondaryGoal{{Key: "a"}, {Key: "a"}}},
		{Secondary: []SecondaryGoal{{Key: "a", Kind: "x"}}},
		{Secondary: []SecondaryGoal{{Key: "a'; DROP TABLE x;--"}}},
	}
	for i, c := range bad {
		if _, err := c.Normalize(); err == nil {
			t.Errorf("case %d should be rejected: %+v", i, c)
		}
	}
	if c, err := (ExperimentConfig{Secondary: []SecondaryGoal{{Key: "a"}}}).Normalize(); err != nil || c.Secondary[0].Kind != KindBinary {
		t.Fatalf("secondary default kind: %+v %v", c, err)
	}
}

// Property: every p-value and probability in the output is in [0,1] across
// random data, and no output field is NaN/Inf (JSON-encodable).
func TestResultsPropertyRanges(t *testing.T) {
	for seed := 0; seed < 60; seed++ {
		n := 30 + seed*7
		in := continuousInputs(n, 1+float64(seed%5)*0.2)
		if seed%3 == 0 {
			in.Cfg.MetricKind = KindCount
		}
		in.Cfg.Secondary = []SecondaryGoal{{Key: "s1", Kind: KindBinary}}
		for i := 0; i < n; i += 1 + seed%4 {
			in.Events["s1"] = append(in.Events["s1"], metricEvent{User: fmt.Sprintf("b%d", i), TS: 1500})
		}
		res := computeResults(in)
		if _, err := json.Marshal(res); err != nil {
			t.Fatalf("seed %d: not JSON-encodable: %v", seed, err)
		}
		check := func(an AnalysisResult) {
			if an.PValue < 0 || an.PValue > 1 {
				t.Fatalf("seed %d: p=%v", seed, an.PValue)
			}
			for _, pw := range an.Pairwise {
				if pw.PValue < 0 || pw.PValue > 1 || pw.HolmAdjustedP < pw.PValue-1e-15 || pw.HolmAdjustedP > 1 {
					t.Fatalf("seed %d: pairwise %+v", seed, pw)
				}
			}
		}
		check(res.Analysis)
		for _, s := range res.Secondary {
			check(s.Analysis)
		}
	}
}
