package metrics

import (
	"math"
	"testing"
)

func TestAttrsToMap_AllScalarKinds(t *testing.T) {
	in := []KeyValue{
		{Key: "k_str", Value: AnyValue{StringValue: "hello"}},
		{Key: "k_int", Value: AnyValue{IntValue: "42"}},
		{Key: "k_bool", Value: AnyValue{BoolValue: true}},
		{Key: "k_dbl", Value: AnyValue{DoubleValue: 3.14}},
	}
	got := AttrsToMap(in)
	if got["k_str"] != "hello" || got["k_int"] != "42" || got["k_bool"] != "true" {
		t.Errorf("attrs map = %v", got)
	}
	if got["k_dbl"] == "" {
		t.Errorf("expected double encoded, got empty")
	}
}

func TestMarshalAttrs_DeterministicOrdering(t *testing.T) {
	a := MarshalAttrs(map[string]string{"b": "2", "a": "1", "c": "3"})
	b := MarshalAttrs(map[string]string{"c": "3", "a": "1", "b": "2"})
	if a != b {
		t.Errorf("expected stable encoding, got %q vs %q", a, b)
	}
}

func TestMatchLabels(t *testing.T) {
	have := map[string]string{"region": "us-east-1", "tier": "prod"}
	if !MatchLabels(have, map[string]string{"region": "us-east-1"}) {
		t.Error("subset match should succeed")
	}
	if MatchLabels(have, map[string]string{"region": "eu-west-1"}) {
		t.Error("mismatched value should fail")
	}
	if !MatchLabels(have, map[string]string{}) {
		t.Error("empty filter should match anything")
	}
}

func TestMarshalUnmarshalHistogram(t *testing.T) {
	dp := HistogramDataPoint{
		Count:          "10",
		Sum:            123.4,
		BucketCounts:   []string{"1", "2", "3", "4"},
		ExplicitBounds: []float64{1, 5, 10},
	}
	raw := MarshalHistogram(dp)
	if raw == "" {
		t.Fatal("marshal returned empty")
	}
	got := UnmarshalHistogram(raw)
	if got.Count != 10 || math.Abs(got.Sum-123.4) > 1e-9 {
		t.Errorf("got %+v", got)
	}
	if len(got.Counts) != 4 || got.Counts[2] != 3 {
		t.Errorf("counts = %v", got.Counts)
	}
	if len(got.Bounds) != 3 || got.Bounds[1] != 5 {
		t.Errorf("bounds = %v", got.Bounds)
	}
}

func TestUnmarshalHistogram_Empty(t *testing.T) {
	got := UnmarshalHistogram("")
	if got.Bounds == nil || got.Counts == nil {
		t.Errorf("empty histogram should still yield non-nil slices: %+v", got)
	}
}

func TestReduce_AllAggregations(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50}
	cases := []struct {
		agg  Aggregation
		want float64
	}{
		{AggLast, 50}, // last passed in separately = 50
		{AggSum, 150},
		{AggAvg, 30},
		{AggMin, 10},
		{AggMax, 50},
	}
	for _, tc := range cases {
		t.Run(string(tc.agg), func(t *testing.T) {
			got := reduce(values, 50, tc.agg)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("reduce(%s) = %v, want %v", tc.agg, got, tc.want)
			}
		})
	}
}

func TestReduce_EmptyReturnsZero(t *testing.T) {
	if got := reduce(nil, 0, AggAvg); got != 0 {
		t.Errorf("empty avg = %v, want 0", got)
	}
}

func TestIsValidAggregation(t *testing.T) {
	for _, ok := range []string{"last", "avg", "sum", "min", "max"} {
		if !IsValidAggregation(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "rate", "p95", "histogram_quantile"} {
		if IsValidAggregation(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
}

func TestParseLabelFilters(t *testing.T) {
	q := map[string][]string{
		"site_id":      {"default"},
		"label.region": {"us-east-1"},
		"label.tier":   {"prod"},
		"name":         {"http_requests"},
	}
	got := ParseLabelFilters(q)
	if len(got) != 2 || got["region"] != "us-east-1" || got["tier"] != "prod" {
		t.Errorf("filters = %v", got)
	}
}

func TestExtractServiceName(t *testing.T) {
	attrs := []KeyValue{
		{Key: "service.name", Value: AnyValue{StringValue: "api"}},
		{Key: "host", Value: AnyValue{StringValue: "x"}},
	}
	if ExtractServiceName(attrs) != "api" {
		t.Errorf("expected api")
	}
	if ExtractServiceName(nil) != "" {
		t.Errorf("expected empty on nil")
	}
}

func TestAggregationTemporality(t *testing.T) {
	if AggregationTemporality(1) != "delta" {
		t.Error("1 -> delta")
	}
	if AggregationTemporality(2) != "cumulative" {
		t.Error("2 -> cumulative")
	}
	if AggregationTemporality(0) != "cumulative" {
		t.Error("0 -> cumulative (default)")
	}
}
