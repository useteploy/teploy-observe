package experiments

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// Goldens generated 2026-10-03 with scipy 1.17.1 / numpy 2.4.6
// (scipy.stats.t, scipy.stats.ttest_ind(equal_var=False), numpy.percentile).

var tTwoSidedGolden = []struct{ t, df, p float64 }{
	{0.5, 3, 0.651447964848151},
	{2.0, 10, 0.07338803477074037},
	{2.5, 7.3, 0.03965023466560043},
	{-1.96, 1000, 0.050273184955748736},
	{5.0, 2.5, 0.02345118997086185},
	{0.1, 50, 0.92074421365297},
	{12, 30, 5.580185415199261e-13},
}

func TestStudentTTwoSidedPGolden(t *testing.T) {
	for _, g := range tTwoSidedGolden {
		if e := relErr(studentTTwoSidedP(g.t, g.df), g.p); e > 1e-9 {
			t.Errorf("p(t=%v, df=%v) = %v want %v (rel %g)", g.t, g.df, studentTTwoSidedP(g.t, g.df), g.p, e)
		}
	}
}

func TestStudentTQuantileGolden(t *testing.T) {
	for _, g := range []struct{ tail, df, q float64 }{
		{0.025, 5, 2.5705818356363155},
		{0.025, 12.5, 2.1691859427125406},
		{0.025, 100, 1.983971518523552},
		{0.005, 20, 2.8453397097861077},
		{0.1, 3.3, 1.5983669585600868},
	} {
		if e := relErr(studentTQuantileUpper(g.tail, g.df), g.q); e > 1e-9 {
			t.Errorf("q(%v, df=%v) = %v want %v", g.tail, g.df, studentTQuantileUpper(g.tail, g.df), g.q)
		}
	}
}

var (
	welchA1 = []float64{12.1, 9.8, 11.4, 10.2, 13.5, 8.9, 10.7, 11.9, 12.8, 9.5}
	welchA2 = []float64{14.2, 15.1, 12.9, 16.3, 13.8, 15.7, 14.9, 13.1, 16.8, 15.2, 14.4, 13.6}
	// Revenue-like: mostly zeros with a heavy right tail.
	welchB1 = []float64{0, 0, 0, 5.5, 0, 12.0, 0, 0, 3.2, 0, 0, 0, 40.0, 0, 0, 1.1, 0, 0, 7.7, 0}
	welchB2 = []float64{0, 2.2, 0, 9.9, 0, 0, 15.5, 0, 0, 4.4, 0, 0, 0, 60.0, 0, 3.3, 0, 0, 0, 8.8, 0, 0}
)

func TestWelchGolden(t *testing.T) {
	cases := []struct {
		name                      string
		a, b                      []float64
		diff, tt, df, p, ciL, ciH float64
	}{
		{"A", welchA1, welchA2, 3.586666666666666, 6.040422007471008, 17.359434660535243, 1.2110889125450551e-05, 2.335878672714065, 4.837454660619267},
		{"B", welchB1, welchB2, 1.2568181818181814, 0.36400584042590584, 37.78032762998611, 0.7178829444952389, -5.734227067120866, 8.247863430757228},
	}
	for _, c := range cases {
		r := welchTest(summarize(c.a), summarize(c.b), 0.05)
		chk := func(n string, got, want float64) {
			if e := relErr(got, want); e > 1e-9 {
				t.Errorf("%s %s = %v want %v (rel %g)", c.name, n, got, want, e)
			}
		}
		chk("diff", r.Diff, c.diff)
		chk("t", r.T, c.tt)
		chk("df", r.DF, c.df)
		chk("p", r.PValue, c.p)
		chk("ci_low", r.CILow, c.ciL)
		chk("ci_high", r.CIHigh, c.ciH)
	}
}

func TestWinsorizeGolden(t *testing.T) {
	pooled := append(append([]float64(nil), welchB1...), welchB2...)
	for _, g := range []struct {
		pct, lo, hi, diff, p, ciL, ciH float64
	}{
		{5, 0, 15.324999999999985, 0.4519318181818175, 0.7581746661195907, -2.4945443577862987, 3.3984079941499337},
		{10, 0, 9.79, 0.33099999999999974, 0.7667082820919727, -1.9085608638852396, 2.5705608638852393},
	} {
		lo, hi, ok := winsorizeBounds(pooled, g.pct)
		if !ok || math.Abs(lo-g.lo) > 1e-12 || math.Abs(hi-g.hi) > 1e-9 {
			t.Fatalf("bounds(%v) = %v,%v want %v,%v", g.pct, lo, hi, g.lo, g.hi)
		}
		arms := [][]float64{append([]float64(nil), welchB1...), append([]float64(nil), welchB2...)}
		winsorizeArms(arms, g.pct)
		r := welchTest(summarize(arms[0]), summarize(arms[1]), 0.05)
		if relErr(r.Diff, g.diff) > 1e-9 || relErr(r.PValue, g.p) > 1e-9 || relErr(r.CILow, g.ciL) > 1e-9 || relErr(r.CIHigh, g.ciH) > 1e-9 {
			t.Errorf("winsorized %v%%: %+v want diff=%v p=%v ci=[%v,%v]", g.pct, r, g.diff, g.p, g.ciL, g.ciH)
		}
	}
	// off / invalid percentiles clip nothing.
	arms := [][]float64{{1, 100}, {2, 3}}
	winsorizeArms(arms, 0)
	winsorizeArms(arms, 50)
	if arms[0][1] != 100 {
		t.Fatal("pct 0/50 must not clip")
	}
}

func TestPercentileLinearGolden(t *testing.T) {
	pooled := append(append([]float64(nil), welchB1...), welchB2...)
	for _, g := range []struct{ q, want float64 }{{0, 0}, {50, 0}, {87.5, 8.662500000000001}, {100, 60}} {
		s := append([]float64(nil), pooled...)
		sort.Float64s(s)
		if got := percentileLinear(s, g.q); math.Abs(got-g.want) > 1e-12 {
			t.Errorf("percentile(%v) = %v want %v", g.q, got, g.want)
		}
	}
}

// Property: p in [0,1], symmetric under swapping arms, monotone in the mean
// shift, CI contains the point difference and brackets 0 iff p > alpha.
func TestWelchProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	gen := func(n int, mu, sd float64) []float64 {
		v := make([]float64, n)
		for i := range v {
			v[i] = mu + sd*rng.NormFloat64()
		}
		return v
	}
	for trial := 0; trial < 200; trial++ {
		a := gen(5+rng.Intn(60), 10, 1+3*rng.Float64())
		b := gen(5+rng.Intn(60), 10+rng.NormFloat64()*2, 1+3*rng.Float64())
		sa, sb := summarize(a), summarize(b)
		r := welchTest(sa, sb, 0.05)
		if r.PValue < 0 || r.PValue > 1 || math.IsNaN(r.PValue) {
			t.Fatalf("p out of range: %+v", r)
		}
		rev := welchTest(sb, sa, 0.05)
		if math.Abs(rev.PValue-r.PValue) > 1e-12 || math.Abs(rev.Diff+r.Diff) > 1e-12 {
			t.Fatalf("not symmetric: %+v vs %+v", r, rev)
		}
		if r.CILow > r.Diff || r.CIHigh < r.Diff {
			t.Fatalf("CI does not contain diff: %+v", r)
		}
		containsZero := r.CILow <= 0 && r.CIHigh >= 0
		if containsZero != (r.PValue >= 0.05) && math.Abs(r.PValue-0.05) > 1e-6 {
			t.Fatalf("CI/p disagree: %+v", r)
		}
		// Monotone: shifting b further from a never raises p.
		prev := 2.0
		sign := 1.0
		if sb.Mean < sa.Mean {
			sign = -1
		}
		for shift := 0.0; shift <= 20; shift += 2 {
			s := sb
			s.Mean += sign * shift
			p := welchTest(sa, s, 0.05).PValue
			if p > prev+1e-12 && math.Abs(s.Mean-sa.Mean) > math.Abs(sb.Mean-sa.Mean) {
				t.Fatalf("p not monotone in shift: %v after %v", p, prev)
			}
			prev = p
		}
	}
}

func TestWelchDegenerate(t *testing.T) {
	if r := welchTest(summarize([]float64{1}), summarize([]float64{2, 3}), 0.05); r.PValue != 1 {
		t.Fatalf("n<2 must give p=1: %+v", r)
	}
	if r := welchTest(summarize([]float64{2, 2, 2}), summarize([]float64{2, 2, 2}), 0.05); r.PValue != 1 {
		t.Fatalf("identical constants: %+v", r)
	}
	if r := welchTest(summarize([]float64{2, 2, 2}), summarize([]float64{5, 5, 5}), 0.05); r.PValue != 0 || math.IsNaN(r.CILow) {
		t.Fatalf("different constants: %+v", r)
	}
}
