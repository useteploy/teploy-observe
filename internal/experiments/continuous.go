package experiments

// Continuous-metric statistics (count and mean/revenue goals): Welch's
// unequal-variance t-test with Welch-Satterthwaite degrees of freedom, the
// mean-difference confidence interval, and percentile winsorizing. Pure
// functions, pinned by scipy goldens in continuous_test.go.

import (
	"math"
	"sort"
)

// minContinuousN floors the per-arm sample for continuous metrics: a t-test
// on a handful of heavy-tailed revenue values is not trustworthy even when
// the experiment's min_sample is set lower.
const minContinuousN = 30

// ---------------------------------------------------------------------------
// Regularized incomplete beta I_x(a, b) and the Student t distribution.

// betaContinuedFraction is the Lentz evaluation of the incomplete-beta
// continued fraction (Numerical Recipes betacf).
func betaContinuedFraction(a, b, x float64) float64 {
	const (
		maxIter = 500
		eps     = 1e-15
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// regIncBeta is the regularized incomplete beta function I_x(a, b).
func regIncBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lbeta := lnGamma(a+b) - lnGamma(a) - lnGamma(b)
	front := math.Exp(lbeta + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(a, b, x) / a
	}
	return 1 - front*betaContinuedFraction(b, a, 1-x)/b
}

// studentTTwoSidedP is P(|T| >= |t|) for Student's t with df degrees of
// freedom (df may be fractional, as Welch-Satterthwaite produces).
func studentTTwoSidedP(t, df float64) float64 {
	if df <= 0 || math.IsNaN(t) {
		return 1
	}
	if math.IsInf(t, 0) {
		return 0
	}
	if t == 0 {
		return 1
	}
	x := df / (df + t*t)
	p := regIncBeta(df/2, 0.5, x)
	return clamp01(p)
}

// studentTQuantileUpper returns the t such that P(T > t) = tail for
// 0 < tail < 0.5 (bisection on the monotone two-sided p; 200 halvings reach
// double precision).
func studentTQuantileUpper(tail, df float64) float64 {
	if tail <= 0 || tail >= 0.5 || df <= 0 {
		return math.NaN()
	}
	target := 2 * tail
	lo, hi := 0.0, 1.0
	for studentTTwoSidedP(hi, df) > target && hi < 1e12 {
		hi *= 2
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if studentTTwoSidedP(mid, df) > target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// ---------------------------------------------------------------------------
// Per-arm summary and Welch's test.

// contSummary is the sufficient statistic of one arm's per-user values.
type contSummary struct {
	N        int64
	Mean     float64
	Variance float64 // unbiased sample variance (n-1); 0 when N < 2
}

func summarize(values []float64) contSummary {
	n := len(values)
	if n == 0 {
		return contSummary{}
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)
	s := contSummary{N: int64(n), Mean: mean}
	if n > 1 {
		var ss float64
		for _, v := range values {
			d := v - mean
			ss += d * d
		}
		s.Variance = ss / float64(n-1)
	}
	return s
}

func (s contSummary) stdDev() float64 { return math.Sqrt(s.Variance) }

// WelchResult is one two-sample Welch comparison (b minus a).
type WelchResult struct {
	Diff   float64
	T      float64
	DF     float64
	PValue float64
	CILow  float64
	CIHigh float64
}

// welchTest compares arm b against arm a (diff = mean_b - mean_a) with the
// two-sided p-value and the 1-alpha confidence interval of the difference,
// using Welch-Satterthwaite degrees of freedom. Fewer than two users in an
// arm gives no inference (p = 1, unbounded-width CI is reported as the point
// difference with p = 1 so callers never claim significance).
func welchTest(a, b contSummary, alpha float64) WelchResult {
	diff := b.Mean - a.Mean
	res := WelchResult{Diff: diff, PValue: 1, CILow: diff, CIHigh: diff}
	if a.N < 2 || b.N < 2 {
		return res
	}
	va := a.Variance / float64(a.N)
	vb := b.Variance / float64(b.N)
	se2 := va + vb
	if se2 <= 0 {
		// Both arms are constant: the difference is exact. Identical
		// constants are no evidence; different constants are certain.
		res.DF = float64(a.N + b.N - 2)
		if diff != 0 {
			res.PValue = 0
		}
		return res
	}
	se := math.Sqrt(se2)
	res.T = diff / se
	res.DF = se2 * se2 / (va*va/float64(a.N-1) + vb*vb/float64(b.N-1))
	res.PValue = studentTTwoSidedP(res.T, res.DF)
	q := studentTQuantileUpper(alpha/2, res.DF)
	res.CILow = diff - q*se
	res.CIHigh = diff + q*se
	return res
}

// ---------------------------------------------------------------------------
// Winsorizing.

// percentileLinear is the linear-interpolation percentile (numpy.percentile
// default) of an ascending-sorted slice, q in [0, 100].
func percentileLinear(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q / 100 * float64(n-1)
	lo := int(math.Floor(pos))
	if lo < 0 {
		lo = 0
	}
	if lo >= n-1 {
		return sorted[n-1]
	}
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}

// winsorizeBounds returns the [pct, 100-pct] percentile clip bounds of the
// pooled values. pct must be in (0, 50); otherwise there is nothing to clip.
func winsorizeBounds(pooled []float64, pct float64) (lo, hi float64, ok bool) {
	if !(pct > 0 && pct < 50) || len(pooled) == 0 {
		return 0, 0, false
	}
	sorted := append([]float64(nil), pooled...)
	sort.Float64s(sorted)
	return percentileLinear(sorted, pct), percentileLinear(sorted, 100-pct), true
}

// winsorizeArms clips every arm's values to the pooled [pct, 100-pct]
// percentile bounds, in place. Bounds are pooled across arms so the clip is
// identical for every arm (a per-arm clip would bias the comparison).
func winsorizeArms(arms [][]float64, pct float64) {
	var pooled []float64
	for _, a := range arms {
		pooled = append(pooled, a...)
	}
	lo, hi, ok := winsorizeBounds(pooled, pct)
	if !ok {
		return
	}
	for _, a := range arms {
		for i, v := range a {
			if v < lo {
				a[i] = lo
			} else if v > hi {
				a[i] = hi
			}
		}
	}
}
