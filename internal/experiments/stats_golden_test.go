package experiments

import (
	"math"
	"testing"
)

// Golden tables generated 2026-09-23 by an INDEPENDENT implementation
// (scipy 1.17 / statsmodels, via the O09 fixture generator) so the in-repo
// pure functions are pinned against outside truth, not against themselves.
// The pre-declared tolerance is 1e-9 relative (the Acklam inverse-normal
// inside sample-size math is refined to ~1e-12; gamma/CF code converges far
// tighter than 1e-9 on these ranges).

func relErr(got, want float64) float64 {
	if want == 0 {
		if got == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(got-want) / math.Abs(want)
}

var chiSquareGolden = []struct {
	df int
	x  float64
	p  float64
}{
	{1, 0.5, 0.47950012218695337},
	{1, 3.84, 0.050043521248705085},
	{1, 3.841458820694124, 0.04999999999999994},
	{1, 10.0, 0.0015654022580025482},
	{1, 6.634896601021213, 0.010000000000000014},
	{2, 1.0, 0.6065306597126334},
	{2, 5.991464547107979, 0.05000000000000008},
	{2, 13.815510557964274, 0.0010000000000000005},
	{3, 7.814727903251179, 0.04999999999999999},
	{3, 16.26623618660646, 0.0010000000045507912},
	{3, 2.5, 0.4752910833430205},
	{4, 9.487729036781154, 0.05000000000000006},
	{4, 18.46684364462193, 0.0009999924697185443},
	{4, 0.3, 0.9898141728888165},
	{5, 0.1, 0.9998376833880774},
	{5, 20.51500598534382, 0.0009999998556398139},
	{5, 11.070497693516351, 0.05000000000000007},
	{9, 27.877164873008, 0.0009999999993269718},
	{20, 45.314746322323124, 0.0010000000927092405},
	{40, 66.76588440992523, 0.005000089470752023},
}

func TestChiSquareSFGolden(t *testing.T) {
	for _, g := range chiSquareGolden {
		if e := relErr(chiSquareSF(g.df, g.x), g.p); e > 1e-9 {
			t.Errorf("chiSquareSF(df=%d, x=%v) = %v, want %v (rel err %g)", g.df, g.x, chiSquareSF(g.df, g.x), g.p, e)
		}
	}
}

var fisherGolden = []struct {
	a, b, c, d int64
	p          float64
}{
	{8, 2, 1, 5, 0.034965034965034975},
	{10, 10, 10, 10, 1.0},
	{3, 1, 0, 4, 0.14285714285714288},
	{12, 4, 5, 11, 0.03195238826540324},
	{0, 5, 5, 0, 0.007936507936507938},
	{7, 7, 7, 7, 1.0},
	{20, 5, 8, 17, 0.0014415471651244192},
	{1, 9, 8, 2, 0.005477494641581329},
	{4, 4, 4, 4, 1.0},
	{2, 18, 9, 11, 0.03095030514385353},
}

func TestFisherExactGolden(t *testing.T) {
	for _, g := range fisherGolden {
		if e := relErr(fisherExact2x2TwoSided(g.a, g.b, g.c, g.d), g.p); e > 1e-9 {
			t.Errorf("fisherExact2x2TwoSided(%d,%d,%d,%d) = %v, want %v (rel err %g)",
				g.a, g.b, g.c, g.d, fisherExact2x2TwoSided(g.a, g.b, g.c, g.d), g.p, e)
		}
	}
}

var wilsonGolden = []struct {
	k, n      int64
	low, high float64
}{
	{5, 100, 0.021543679154367966, 0.11175046923191914},
	{0, 10, 0.0, 0.27753279986288926},
	{10, 10, 0.7224672001371106, 1.0},
	{1, 2, 0.09453120573423068, 0.9054687942657693},
	{42, 500, 0.06274413680383037, 0.11159931446018112},
	{777, 3000, 0.24363879421564508, 0.2749776108699621},
	{3, 7, 0.15821985525146964, 0.7495416354723428},
}

func TestWilsonGolden(t *testing.T) {
	for _, g := range wilsonGolden {
		lo, hi := wilsonInterval(g.k, g.n, zAlphaTwoSided005)
		if e := relErr(lo, g.low); e > 1e-9 {
			t.Errorf("wilson(%d,%d) low = %v, want %v", g.k, g.n, lo, g.low)
		}
		if e := relErr(hi, g.high); e > 1e-9 {
			t.Errorf("wilson(%d,%d) high = %v, want %v", g.k, g.n, hi, g.high)
		}
	}
}

var newcombeGolden = []struct {
	k1, n1, k2, n2 int64
	low, high      float64
}{
	{40, 200, 60, 200, 0.015063442559663034, 0.18316248444472888},
	{0, 50, 5, 50, 0.008975448080590018, 0.21360231437479657},
	{10, 10, 9, 10, -0.4041500267952386, 0.1894283527495889},
	{25, 100, 25, 100, -0.11922538133726894, 0.11922538133726894},
	{100, 1000, 140, 1000, 0.01151533907141367, 0.06856374914754898},
}

func TestNewcombeGolden(t *testing.T) {
	for _, g := range newcombeGolden {
		lo, hi := newcombeDifferenceCI(g.k1, g.n1, g.k2, g.n2, zAlphaTwoSided005)
		if e := relErr(lo, g.low); e > 1e-9 {
			t.Errorf("newcombe(%d,%d,%d,%d) low = %v, want %v", g.k1, g.n1, g.k2, g.n2, lo, g.low)
		}
		if e := relErr(hi, g.high); e > 1e-9 {
			t.Errorf("newcombe(%d,%d,%d,%d) high = %v, want %v", g.k1, g.n1, g.k2, g.n2, hi, g.high)
		}
	}
}

var holmGolden = []struct {
	in  []float64
	out []float64
}{
	{[]float64{0.03, 0.04, 0.1}, []float64{0.09, 0.09, 0.1}},
	{[]float64{0.001, 0.2, 0.3, 0.011}, []float64{0.004, 0.4, 0.4, 0.033}},
	{[]float64{0.5, 0.5}, []float64{1.0, 1.0}},
	{[]float64{0.01, 0.01, 0.01}, []float64{0.03, 0.03, 0.03}},
	{[]float64{0.9, 0.001}, []float64{0.9, 0.002}},
}

func TestHolmGolden(t *testing.T) {
	for _, g := range holmGolden {
		adj := holmAdjusted(g.in)
		for i := range adj {
			if e := relErr(adj[i], g.out[i]); e > 1e-9 {
				t.Errorf("holm(%v)[%d] = %v, want %v", g.in, i, adj[i], g.out[i])
			}
		}
	}
}

var srmGolden = []struct {
	counts  []int64
	weights []float64
	stat, p float64
}{
	{[]int64{5500, 4500}, []float64{0.5, 0.5}, 100.0, 1.5239706048320995e-23},
	{[]int64{5000, 5000}, []float64{0.5, 0.5}, 0.0, 1.0},
	{[]int64{5050, 4950}, []float64{0.5, 0.5}, 1.0, 0.31731050786291115},
	{[]int64{3000, 3000, 3000, 3000}, []float64{0.25, 0.25, 0.25, 0.25}, 0.0, 1.0},
	{[]int64{3400, 3300, 3300}, []float64{0.3333333333333333, 0.3333333333333333, 0.3333333333333333}, 2.0, 0.36787944117144245},
	{[]int64{6000, 4000}, []float64{0.5, 0.5}, 400.0, 5.507248237212379e-89},
	{[]int64{5300, 4700}, []float64{0.5, 0.5}, 36.0, 1.973175290075393e-09},
	{[]int64{5200, 4800}, []float64{0.5, 0.5}, 16.0, 6.33424836662399e-05},
}

func TestSRMGolden(t *testing.T) {
	for _, g := range srmGolden {
		w := append([]float64(nil), g.weights...)
		stat, p := srmGoodnessOfFit(g.counts, w)
		if e := relErr(stat, g.stat); e > 1e-9 {
			t.Errorf("srm(%v) stat = %v, want %v", g.counts, stat, g.stat)
		}
		if g.p < 1e-300 {
			// sub-normal p: pin order of magnitude instead of relative error
			if p > 1e-100 && g.p < 1e-100 {
				t.Errorf("srm(%v) p = %v, want %v", g.counts, p, g.p)
			}
			continue
		}
		if e := relErr(p, g.p); e > 1e-9 {
			t.Errorf("srm(%v) p = %v, want %v (rel err %g)", g.counts, p, g.p, e)
		}
	}
}

var mdeGolden = []struct {
	p1, delta float64
	n         int64
}{
	{0.1, 0.02, 3841},
	{0.05, 0.01, 8158},
	{0.2, 0.05, 1094},
	{0.5, 0.03, 4356},
	{0.02, 0.01, 3826},
}

func TestMDEGolden(t *testing.T) {
	for _, g := range mdeGolden {
		if n := sampleSizePerArm(g.p1, g.delta, 0.05, 0.80); n != g.n {
			t.Errorf("sampleSizePerArm(%v, %v) = %d, want %d", g.p1, g.delta, n, g.n)
		}
	}
}
