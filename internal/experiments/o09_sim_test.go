package experiments

// O09 fixture plan (DELEGATED_DECISIONS 2026-09-23, section 4): simulation
// fixtures pinning the full gate pipeline - no-effect false-winner rate,
// per-arm horizon regression, SRM detection/false-alarm, and measured power
// against the design calculation. Deterministic PCG seeds; a failing seed
// here means the pipeline, not the dice.

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func simArms(r *rand.Rand, ns []int64, rates []float64) []VariantResult {
	arms := make([]VariantResult, len(ns))
	for i := range ns {
		conv := int64(0)
		for j := int64(0); j < ns[i]; j++ {
			if r.Float64() < rates[i] {
				conv++
			}
		}
		rate := 0.0
		if ns[i] > 0 {
			rate = float64(conv) / float64(ns[i])
		}
		lo, hi := wilsonInterval(conv, ns[i], zAlphaTwoSided005)
		arms[i] = VariantResult{
			Variant: fmt.Sprintf("arm%d", i), Exposures: ns[i], Conversions: conv,
			ConversionRate: rate, WilsonLow: lo, WilsonHigh: hi,
		}
	}
	return arms
}

func uniformWeights(k int) []float64 {
	w := make([]float64, k)
	for i := range w {
		w[i] = 1
	}
	return w
}

// TestO09NoEffectSimulation: k arms, equal rates, 10k/arm. The full gate
// (horizon + omnibus + Holm pairwise + winner-must-be-Holm-significant)
// must declare a winner in at most ~5% of simulations. The Holm pairwise
// requirement is what holds the family rate at alpha.
func TestO09NoEffectSimulation(t *testing.T) {
	const sims = 1000
	r := rand.New(rand.NewPCG(9, 1))
	ns := []int64{10_000, 10_000}
	rates := []float64{0.10, 0.10}
	winners := 0
	for i := 0; i < sims; i++ {
		arms := simArms(r, ns, rates)
		res := analyze(arms, uniformWeights(2), 100)
		if winnerFrom(res, arms) != "" {
			winners++
		}
	}
	if got := float64(winners) / sims; got > 0.06 {
		t.Fatalf("no-effect winner rate %v over %d sims (want <= ~0.05)", got, sims)
	} else {
		t.Logf("no-effect winner rate: %d/%d = %.3f", winners, sims, got)
	}
}

// TestO09PerArmHorizonRegression: a 9,900/100 split with a large raw-rate
// gap declared winners under the old TOTAL-exposure gate (10,000 total).
// The per-arm gate must refuse: min arm 80 < 100.
func TestO09PerArmHorizonRegression(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 2))
	ns := []int64{9_900, 80}
	// Force the gap without waiting for the dice: 990-exposure-equivalent
	// rates on the big arm, 90% on the small one.
	res := analyze(simArms(r, ns, []float64{0.09, 0.90}), uniformWeights(2), 100)
	if res.HorizonMet {
		t.Fatalf("horizon met with min arm %d < 100 - the per-arm gate regressed to the total gate", res.MinArmExposures)
	}
	if w := winnerFrom(res, nil); w != "" {
		t.Fatalf("winner %q declared under an unmet horizon", w)
	}
	if res.SRM.Detected {
		t.Logf("note: SRM also fires on this 99:1 split, as it should")
	}
}

// TestO09SRMDetectionAndFalseAlarm: a 55/45 allocation skew is detected in
// >= 99% of large-n simulations at alpha=0.001; a clean uniform allocation
// is flagged in <= 0.5% of them.
func TestO09SRMDetectionAndFalseAlarm(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 3))
	detected, sims := 0, 200
	for i := 0; i < sims; i++ {
		// 55/45 hash skew, 10k assignments
		arms := make([]int64, 2)
		for j := 0; j < 10_000; j++ {
			if r.Float64() < 0.55 {
				arms[0]++
			} else {
				arms[1]++
			}
		}
		vr := []VariantResult{{Exposures: arms[0]}, {Exposures: arms[1]}}
		if analyze(vr, uniformWeights(2), 100).SRM.Detected {
			detected++
		}
	}
	if got := float64(detected) / float64(sims); got < 0.99 {
		t.Fatalf("55/45 SRM detection %.3f over %d sims (want >= 0.99)", got, sims)
	}

	falseAlarms, cleanSims := 0, 1000
	for i := 0; i < cleanSims; i++ {
		arms := make([]int64, 2)
		for j := 0; j < 10_000; j++ {
			if r.Float64() < 0.5 {
				arms[0]++
			} else {
				arms[1]++
			}
		}
		vr := []VariantResult{{Exposures: arms[0]}, {Exposures: arms[1]}}
		if analyze(vr, uniformWeights(2), 100).SRM.Detected {
			falseAlarms++
		}
	}
	if got := float64(falseAlarms) / float64(cleanSims); got > 0.005 {
		t.Fatalf("clean-allocation SRM false alarm %.4f over %d sims (want <= 0.005)", got, cleanSims)
	}
}

// TestO09MeasuredPowerMatchesDesign: for a designed effect (p1=0.10,
// delta=0.02, 80% power), simulating at the recommended n/arm must declare
// the better arm winner in ~80% of runs (within ±2%).
func TestO09MeasuredPowerMatchesDesign(t *testing.T) {
	p1, delta := 0.10, 0.02
	n := sampleSizePerArm(p1, delta, 0.05, 0.80)
	if n <= 0 {
		t.Fatalf("sampleSizePerArm returned %d", n)
	}
	const sims = 4000
	r := rand.New(rand.NewPCG(9, 4))
	ns := []int64{n, n}
	winners := 0
	for i := 0; i < sims; i++ {
		arms := simArms(r, ns, []float64{p1, p1 + delta})
		res := analyze(arms, uniformWeights(2), int(n)/10)
		if w := winnerFrom(res, arms); w == "arm1" {
			winners++
		}
	}
	got := float64(winners) / sims
	want := 0.80
	if math.Abs(got-want) > 0.02 {
		t.Fatalf("measured power %.3f over %d sims at n=%d/arm, design 0.800 (|delta| %.3f > 0.02)", got, sims, n, math.Abs(got-want))
	}
	t.Logf("measured power %.3f at n=%d/arm (design 0.800)", got, n)
}

// TestO09InputSemanticsLive pins the SQL-side input semantics against the
// real engine: exposures restricted to the running interval, conversions
// restricted to the conversion window after exposure (late arrival after
// stop still counts inside the window), identities deduped.
func TestO09InputSemanticsLive(t *testing.T) {
	db, done := expTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewExperimentService(db)

	site := fmt.Sprintf("o09-sem-%d", time.Now().UnixNano())
	exp, err := svc.Create(ctx, site, "sem", "sem_flag", "pageview", "",
		`[{"key":"control","rollout_pct":50},{"key":"treatment","rollout_pct":50}]`, 4)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Start(ctx, exp.ExperimentID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Read the experiment back for its ACTUAL started_at stamp (Start
	// writes its own now; guessing it here is exactly the class of bug the
	// interval semantics exist to catch).
	cur, err := svc.Results(ctx, exp.ExperimentID, site)
	if err != nil {
		t.Fatalf("results (pre-inserts): %v", err)
	}
	started := cur.Experiment.StartedAt.UnixMilli()

	ins := func(table, user, variant string, ts int64) {
		id := fmt.Sprintf("%s-%s-%d", user, variant, ts)
		var stmt string
		if table == "exposures" {
			stmt = `INSERT INTO experiment_exposures (exposure_id, tenant_id, experiment_id, site_id, user_id, variant, converted, timestamp)
				VALUES ($1,'default',$2,$3,$4,$5,'false',$6)`
		} else {
			stmt = `INSERT INTO experiment_conversions (conversion_id, tenant_id, experiment_id, site_id, user_id, variant, timestamp)
				VALUES ($1,'default',$2,$3,$4,$5,$6)`
		}
		if _, err := db.SQL().Exec(ctx, stmt, id, exp.ExperimentID, site, user, variant, ts); err != nil {
			t.Fatalf("insert %s: %v", table, err)
		}
	}

	const hour = int64(3600 * 1000)
	// In-interval exposure stamps: after started_at, before the Results()
	// call that will evaluate them (ms-scale offsets on both sides).
	eTs := started + 1
	// Pre-start exposure: excluded.
	ins("exposures", "pre", "control", started-2*hour)
	// In-interval exposures (duplicate for in1: deduped to one).
	ins("exposures", "in1", "control", eTs)
	ins("exposures", "in1", "control", eTs+1000)
	ins("exposures", "in2", "treatment", eTs)
	// Conversion inside the window: counts.
	ins("conversions", "in1", "control", eTs+10*1000)
	// Conversion outside the 72h window: excluded.
	ins("conversions", "in2", "treatment", eTs+100*hour)
	// Conversion with no exposure at all: ignored.
	ins("conversions", "ghost", "treatment", eTs+5*1000)

	res, err := svc.Results(ctx, exp.ExperimentID, site)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	byV := map[string]VariantResult{}
	for _, v := range res.Variants {
		byV[v.Variant] = v
	}
	if c := byV["control"].Exposures; c != 1 {
		t.Errorf("control exposures = %d, want 1 (pre-start excluded, duplicate deduped)", c)
	}
	if c := byV["treatment"].Exposures; c != 1 {
		t.Errorf("treatment exposures = %d, want 1", c)
	}
	if c := byV["control"].Conversions; c != 1 {
		t.Errorf("control conversions = %d, want 1 (in-window)", c)
	}
	if c := byV["treatment"].Conversions; c != 0 {
		t.Errorf("treatment conversions = %d, want 0 (100h > 72h window)", c)
	}

	// Stop the experiment; a LATE conversion inside the window still counts.
	if err := svc.Stop(ctx, exp.ExperimentID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	ins("conversions", "in2", "treatment", time.Now().UTC().UnixMilli()) // ~within 72h of its exposure
	res2, err := svc.Results(ctx, exp.ExperimentID, site)
	if err != nil {
		t.Fatalf("results after stop: %v", err)
	}
	for _, v := range res2.Variants {
		if v.Variant == "treatment" && v.Conversions != 1 {
			t.Errorf("late in-window conversion after stop not counted: treatment conversions = %d, want 1", v.Conversions)
		}
	}
}
