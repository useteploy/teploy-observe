package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// AttrsToMap converts OTLP KeyValue pairs into the string map persisted
// to the `attributes` column. Mirrors tracing.AttrsToMap so labels look
// the same across signals (and the Explorer can join on them by key).
func AttrsToMap(attrs []KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if v, ok := kv.Value.text(); ok {
			m[kv.Key] = v
		}
	}
	return m
}

// MarshalAttrs returns a deterministic JSON encoding of the label map.
// Determinism (sorted keys) means equal label sets serialize byte-identical,
// which makes the `attributes` column safe to LIKE-search for the simple
// label filter the query API supports.
func MarshalAttrs(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([][2]string, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, [2]string{k, m[k]})
	}
	// Encode as a JSON object by hand-building an ordered map equivalent.
	// json.Marshal of map[string]string sorts keys, so use that.
	out := make(map[string]string, len(m))
	for _, kv := range ordered {
		out[kv[0]] = kv[1]
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// UnmarshalAttrs decodes a JSON-encoded label map back into a string map.
// Returns an empty map for empty / malformed input — the column is best
// effort and shouldn't poison query responses.
func UnmarshalAttrs(raw string) map[string]string {
	if raw == "" || raw == "{}" {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

// MatchLabels returns true when every (k, v) in want is present in have
// with exactly the same value. Used by the query path to filter points
// without pushing the predicate into SQL (Nucleus has no JSONB extract).
func MatchLabels(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// HistogramShape is the persisted form of a histogram data point.
// Stored as JSON in the `histogram` column so the query layer can fan
// it back out without a second table.
type HistogramShape struct {
	Bounds []float64 `json:"bounds"`
	Counts []int64   `json:"counts"`
	Sum    float64   `json:"sum"`
	Count  int64     `json:"count"`
}

// MarshalHistogram converts an OTLP HistogramDataPoint into the persisted
// JSON shape. Counts are coerced from string (OTLP wire format uses
// strings for int64 to survive JSON's lossy number type) to int64; bad
// inputs degrade to zero rather than failing the whole insert.
func MarshalHistogram(dp HistogramDataPoint) string {
	counts := make([]int64, 0, len(dp.BucketCounts))
	for _, c := range dp.BucketCounts {
		v, _ := parseInt64(string(c))
		counts = append(counts, v)
	}
	total, _ := parseInt64(string(dp.Count))
	bounds := dp.ExplicitBounds
	if bounds == nil {
		bounds = []float64{}
	}
	shape := HistogramShape{
		Bounds: bounds,
		Counts: counts,
		Sum:    dp.Sum,
		Count:  total,
	}
	raw, err := json.Marshal(shape)
	if err != nil {
		return ""
	}
	return string(raw)
}

// UnmarshalHistogram is the read-side counterpart to MarshalHistogram.
// Returns a HistogramShape with empty (non-nil) Bounds/Counts for empty
// or malformed input so query handlers can still emit a row instead of
// crashing — and so the JSON encoder ships [] rather than null.
func UnmarshalHistogram(raw string) HistogramShape {
	out := HistogramShape{Bounds: []float64{}, Counts: []int64{}}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return HistogramShape{Bounds: []float64{}, Counts: []int64{}}
	}
	if out.Bounds == nil {
		out.Bounds = []float64{}
	}
	if out.Counts == nil {
		out.Counts = []int64{}
	}
	if !validHistogramShape(out) {
		return HistogramShape{Bounds: []float64{}, Counts: []int64{}}
	}
	return out
}

func parseInt64(s string) (int64, error) {
	var n int64
	for i, r := range s {
		if i == 0 && r == '-' {
			continue
		}
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not an int: %q", s)
		}
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// Explicit histograms have one overflow bucket beyond their finite bounds.
// This reducer supports nonnegative finite bounds only. Signed finite
// bounds cannot justify a zero first-bucket lower edge; refuse before storage
// acknowledgement, including legacy decoding, rather than inventing a median.
func validHistogramShape(h HistogramShape) bool {
	if len(h.Bounds) == 0 || len(h.Counts) != len(h.Bounds)+1 || h.Count < 0 || h.Sum < 0 || math.IsNaN(h.Sum) || math.IsInf(h.Sum, 0) {
		return false
	}
	for i, b := range h.Bounds {
		if b < 0 || math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= h.Bounds[i-1]) {
			return false
		}
	}
	var total int64
	for _, c := range h.Counts {
		if c < 0 || c > math.MaxInt64-total {
			return false
		}
		total += c
	}
	return total == h.Count
}
func validHistogramPoint(dp HistogramDataPoint) bool {
	total, err := parseInt64(string(dp.Count))
	if err != nil {
		return false
	}
	h := HistogramShape{Bounds: dp.ExplicitBounds, Sum: dp.Sum, Count: total}
	for _, c := range dp.BucketCounts {
		n, err := parseInt64(string(c))
		if err != nil {
			return false
		}
		h.Counts = append(h.Counts, n)
	}
	return validHistogramShape(h)
}
