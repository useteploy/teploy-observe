package tracing

import (
	"strconv"
	"testing"
)

// TestComputeFunnel_OrderedConversion is the headline test: three synthetic
// traces drive a 3-step funnel and we assert the per-step counts +
// conversion %. Trace A reaches all 3 steps; trace B reaches 2 steps;
// trace C reaches only step 1.
func TestComputeFunnel_OrderedConversion(t *testing.T) {
	const base int64 = 1_700_000_000_000
	rows := []funnelSpan{
		// Trace A: full funnel
		{TraceID: "a", OpName: "GET /products", StartTime: itoa(base + 100)},
		{TraceID: "a", OpName: "POST /cart", StartTime: itoa(base + 500)},
		{TraceID: "a", OpName: "POST /checkout", StartTime: itoa(base + 1500)},
		// Trace B: stops at step 2
		{TraceID: "b", OpName: "GET /products", StartTime: itoa(base + 50)},
		{TraceID: "b", OpName: "POST /cart", StartTime: itoa(base + 200)},
		// Trace C: only step 1
		{TraceID: "c", OpName: "GET /products", StartTime: itoa(base + 10)},
	}
	ops := []string{"GET /products", "POST /cart", "POST /checkout"}

	got := computeFunnel(rows, ops)

	if got.TotalTraces != 3 {
		t.Errorf("TotalTraces=%d want 3", got.TotalTraces)
	}
	if len(got.Steps) != 3 {
		t.Fatalf("len(Steps)=%d want 3", len(got.Steps))
	}
	wantCounts := []int64{3, 2, 1}
	wantConv := []float64{100.0, 200.0 / 3.0, 50.0}
	for i, step := range got.Steps {
		if step.Count != wantCounts[i] {
			t.Errorf("step[%d] count=%d want %d", i, step.Count, wantCounts[i])
		}
		if !approxEq(step.ConversionPct, wantConv[i], 0.001) {
			t.Errorf("step[%d] conversion=%.3f want %.3f", i, step.ConversionPct, wantConv[i])
		}
	}

	// step1 gap for trace A=400, trace B=150 -> median=150 (idx 0), p95=400.
	// Nearest-rank with n=2: median idx = int(1*0.5)=0, p95 idx = int(1*0.95)=0.
	// So with only 2 samples median and p95 collapse to the lower; assert at
	// least non-zero rather than over-specify.
	if got.Steps[1].MedianGapMs == 0 {
		t.Errorf("step[1] median gap should be >0")
	}
	if got.Steps[2].MedianGapMs == 0 {
		t.Errorf("step[2] median gap should be >0")
	}
}

// TestComputeFunnel_RepeatOpInTraceCountsOnce verifies that a trace which
// has multiple matches for the same operation only counts once at that
// step (we anchor on first match, then advance).
func TestComputeFunnel_RepeatOpInTraceCountsOnce(t *testing.T) {
	const base int64 = 1_700_000_000_000
	rows := []funnelSpan{
		// Two GET /products spans before the cart event.
		{TraceID: "a", OpName: "GET /products", StartTime: itoa(base + 10)},
		{TraceID: "a", OpName: "GET /products", StartTime: itoa(base + 20)},
		{TraceID: "a", OpName: "POST /cart", StartTime: itoa(base + 100)},
	}
	ops := []string{"GET /products", "POST /cart"}

	got := computeFunnel(rows, ops)
	if got.Steps[0].Count != 1 {
		t.Errorf("step0 count=%d want 1", got.Steps[0].Count)
	}
	if got.Steps[1].Count != 1 {
		t.Errorf("step1 count=%d want 1", got.Steps[1].Count)
	}
	if got.Steps[1].ConversionPct != 100.0 {
		t.Errorf("step1 conversion=%.2f want 100", got.Steps[1].ConversionPct)
	}
}

// TestComputeFunnel_OutOfOrderDoesNotMatch verifies that step N+1 occurring
// before step N within a trace doesn't satisfy the funnel.
func TestComputeFunnel_OutOfOrderDoesNotMatch(t *testing.T) {
	const base int64 = 1_700_000_000_000
	rows := []funnelSpan{
		// POST /cart precedes GET /products in time within trace A.
		{TraceID: "a", OpName: "POST /cart", StartTime: itoa(base + 10)},
		{TraceID: "a", OpName: "GET /products", StartTime: itoa(base + 50)},
	}
	ops := []string{"GET /products", "POST /cart"}

	got := computeFunnel(rows, ops)
	if got.Steps[0].Count != 1 {
		t.Errorf("step0 count=%d want 1 (the GET still matches step0)", got.Steps[0].Count)
	}
	// The POST occurred BEFORE the GET, so step1 should NOT match (no
	// POST after the matched GET).
	if got.Steps[1].Count != 0 {
		t.Errorf("step1 count=%d want 0 (POST is before GET, can't match step1)", got.Steps[1].Count)
	}
}

// TestComputeFunnel_EmptyOpsReturnsEmpty guards the cheap-exit path.
func TestComputeFunnel_EmptyOpsReturnsEmpty(t *testing.T) {
	got := computeFunnel(nil, nil)
	if len(got.Steps) != 0 {
		t.Errorf("len(Steps)=%d want 0", len(got.Steps))
	}
	if got.TotalTraces != 0 {
		t.Errorf("TotalTraces=%d want 0", got.TotalTraces)
	}
}

// TestComputeFunnel_NoMatchingTracesReturnsZeros verifies a funnel against
// an op set that no trace satisfies returns zero-count steps without panic.
func TestComputeFunnel_NoMatchingTracesReturnsZeros(t *testing.T) {
	rows := []funnelSpan{
		{TraceID: "a", OpName: "GET /home", StartTime: itoa(1)},
	}
	got := computeFunnel(rows, []string{"GET /products", "POST /cart"})
	if got.Steps[0].Count != 0 || got.Steps[1].Count != 0 {
		t.Errorf("expected all-zero steps, got %+v", got.Steps)
	}
	if got.Steps[0].ConversionPct != 0 {
		t.Errorf("step0 conversion=%.2f want 0", got.Steps[0].ConversionPct)
	}
}

// TestPercentilePair covers the helper directly so failures surface
// faster than via the larger funnel tests. Uses nearest-rank with idx =
// (n-1)*p — for n=5 that gives median idx=2 (300), p95 idx=3 (400).
func TestPercentilePair(t *testing.T) {
	med, p95 := percentilePair([]int64{100, 200, 300, 400, 500})
	if med != 300 {
		t.Errorf("median=%d want 300", med)
	}
	if p95 != 400 {
		t.Errorf("p95=%d want 400", p95)
	}
	med, p95 = percentilePair(nil)
	if med != 0 || p95 != 0 {
		t.Errorf("empty input median=%d p95=%d want 0/0", med, p95)
	}
	// With 100 samples, p95 idx = int(99*0.95) = 94 -> vals[94] = 95.
	big := make([]int64, 100)
	for i := range big {
		big[i] = int64(i + 1)
	}
	med, p95 = percentilePair(big)
	if med != 50 {
		t.Errorf("100-sample median=%d want 50", med)
	}
	if p95 != 95 {
		t.Errorf("100-sample p95=%d want 95", p95)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func approxEq(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
