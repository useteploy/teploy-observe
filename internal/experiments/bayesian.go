package experiments

import (
	"math"
	"math/rand"
)

// computeBayesianProbabilities fills in ProbBeatControl for each variant using
// a Beta(1+conversions, 1+nonconversions) prior and 4000 Monte Carlo samples.
// The first variant is treated as the control.
func computeBayesianProbabilities(variants []VariantResult) {
	if len(variants) < 2 {
		return
	}
	const samples = 4000
	rng := rand.New(rand.NewSource(42))

	controlSamples := drawBeta(rng, float64(1+variants[0].Conversions), float64(1+variants[0].Exposures-variants[0].Conversions), samples)

	for i := 1; i < len(variants); i++ {
		v := variants[i]
		varSamples := drawBeta(rng, float64(1+v.Conversions), float64(1+v.Exposures-v.Conversions), samples)
		wins := 0
		for j := 0; j < samples; j++ {
			if varSamples[j] > controlSamples[j] {
				wins++
			}
		}
		variants[i].ProbBeatControl = float64(wins) / float64(samples)
	}
	// Control by convention: probability it "beats itself" is undefined.
	variants[0].ProbBeatControl = 0
}

// drawBeta returns n samples from Beta(alpha, beta) via the ratio of two Gammas.
func drawBeta(rng *rand.Rand, alpha, beta float64, n int) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		x := sampleGamma(rng, alpha)
		y := sampleGamma(rng, beta)
		if x+y == 0 {
			out[i] = 0.5
			continue
		}
		out[i] = x / (x + y)
	}
	return out
}

// sampleGamma draws from Gamma(shape, 1) using Marsaglia & Tsang (shape >= 1),
// with the Johnk shift for shape < 1.
func sampleGamma(rng *rand.Rand, shape float64) float64 {
	if shape < 1 {
		u := rng.Float64()
		return sampleGamma(rng, shape+1) * math.Pow(u, 1.0/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		var x, v float64
		for {
			x = rng.NormFloat64()
			v = 1 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
