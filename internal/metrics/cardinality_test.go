package metrics

import (
	"errors"
	"strings"
	"testing"
)

func kv(k, v string) KeyValue {
	return KeyValue{Key: k, Value: AnyValue{StringValue: v}}
}

func TestTruncateAttrs_CountAndValueCaps(t *testing.T) {
	g := newSeriesGuard(CardinalityLimits{MaxAttrsPerPoint: 3, MaxAttrValueBytes: 10, MaxPointsPerRequest: 100, MaxSeriesPerSite: 100})

	// Over-count: 5 attrs, cap 3 — lexicographically first kept.
	m := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	out := g.truncateAttrs("s", m)
	if len(out) != 3 || out["a"] != "1" || out["b"] != "2" || out["c"] != "3" {
		t.Fatalf("count truncation kept wrong set: %v", out)
	}
	st := g.Stats()
	if st.AttrsTruncated != 1 {
		t.Fatalf("attrs_truncated = %d, want 1", st.AttrsTruncated)
	}

	// Over-long value, UTF-8-safe cut (the cut lands mid-rune without the
	// boundary walk: "é" is 2 bytes, "é✓" is 5).
	g2 := newSeriesGuard(CardinalityLimits{MaxAttrsPerPoint: 3, MaxAttrValueBytes: 4, MaxPointsPerRequest: 100, MaxSeriesPerSite: 100})
	v := g2.truncateAttrs("s", map[string]string{"k": strings.Repeat("é", 6)}) // 12 bytes
	got := v["k"]
	if len(got) != 4 || len([]rune(got)) != 2 {
		t.Fatalf("value truncation = %q (%d bytes, %d runes), want 4 bytes / 2 runes", got, len(got), len([]rune(got)))
	}
	if g2.Stats().ValuesTruncated != 1 {
		t.Fatalf("values_truncated = %d, want 1", g2.Stats().ValuesTruncated)
	}

	// Within both caps: untouched, no counters.
	g3 := newSeriesGuard(CardinalityLimits{MaxAttrsPerPoint: 3, MaxAttrValueBytes: 10, MaxPointsPerRequest: 100, MaxSeriesPerSite: 100})
	same := map[string]string{"x": "ok"}
	if out := g3.truncateAttrs("s", same); len(out) != 1 || out["x"] != "ok" {
		t.Fatalf("in-limit map must pass through: %v", out)
	}
	if s := g3.Stats(); s.AttrsTruncated != 0 || s.ValuesTruncated != 0 {
		t.Fatalf("in-limit map must not count: %+v", s)
	}
}

func TestSeriesAdmission_KnownSeriesKeepFlowingPastCap(t *testing.T) {
	g := newSeriesGuard(CardinalityLimits{MaxAttrsPerPoint: 8, MaxAttrValueBytes: 32, MaxPointsPerRequest: 1000, MaxSeriesPerSite: 3})
	k1 := seriesKey("m", "svc", `{"a":"1"}`)
	k2 := seriesKey("m", "svc", `{"a":"2"}`)
	k3 := seriesKey("m", "svc", `{"a":"3"}`)
	if !g.admit("site", k1) || !g.admit("site", k2) || !g.admit("site", k3) {
		t.Fatal("first three series must be admitted")
	}
	if g.admit("site", seriesKey("m", "svc", `{"a":"4"}`)) {
		t.Fatal("fourth distinct series past the cap must be dropped")
	}
	if !g.admit("site", k1) {
		t.Fatal("known series must keep flowing at the cap")
	}
	// Caps are per site: another site gets its own budget.
	if !g.admit("other", seriesKey("m", "svc", `{"a":"9"}`)) {
		t.Fatal("other site must be unaffected")
	}
	st := g.Stats()
	if st.SeriesDropped != 1 || st.SitesAtSeriesCap != 1 || st.SeriesLimit != 3 {
		t.Fatalf("stats = %+v, want 1 dropped / 1 site at cap / limit 3", st)
	}
}

func TestCheckBatch_RefusesLabeledPastCeiling(t *testing.T) {
	g := newSeriesGuard(CardinalityLimits{MaxAttrsPerPoint: 8, MaxAttrValueBytes: 32, MaxPointsPerRequest: 10, MaxSeriesPerSite: 5})
	if err := g.checkBatch(10); err != nil {
		t.Fatalf("at-ceiling batch must pass: %v", err)
	}
	err := g.checkBatch(11)
	var e *ErrTooManyPoints
	if !errors.As(err, &e) || e.Points != 11 || e.Limit != 10 {
		t.Fatalf("over-ceiling batch must refuse with ErrTooManyPoints, got %v", err)
	}
	if !strings.Contains(err.Error(), "split") {
		t.Fatalf("refusal must carry the remedy: %v", err)
	}
	if g.Stats().PointsRefusedBatch != 1 {
		t.Fatalf("points_refused_batch = %d, want 1", g.Stats().PointsRefusedBatch)
	}
}

func TestLoadCardinalityLimitsFromEnv(t *testing.T) {
	env := map[string]string{
		"OBSERVE_METRICS_MAX_ATTRS_PER_POINT":    "5",
		"OBSERVE_METRICS_MAX_ATTR_VALUE_BYTES":   "9",
		"OBSERVE_METRICS_MAX_POINTS_PER_REQUEST": "77",
		"OBSERVE_METRICS_MAX_SERIES_PER_SITE":    "99",
	}
	c := LoadCardinalityLimitsFromEnv(func(k string) string { return env[k] }, nil)
	if c.MaxAttrsPerPoint != 5 || c.MaxAttrValueBytes != 9 || c.MaxPointsPerRequest != 77 || c.MaxSeriesPerSite != 99 {
		t.Fatalf("limits = %+v", c)
	}
	env["OBSERVE_METRICS_MAX_SERIES_PER_SITE"] = "bogus"
	d := DefaultCardinalityLimits()
	c = LoadCardinalityLimitsFromEnv(func(k string) string { return env[k] }, nil)
	if c.MaxSeriesPerSite != d.MaxSeriesPerSite {
		t.Fatalf("bad knob must keep the declared default, got %+v", c)
	}
	// Zero-valued limits fall back to the declared defaults (a
	// hand-built CardinalityLimits{} cannot disable the guard).
	g := newSeriesGuard(CardinalityLimits{})
	if g.limits.MaxSeriesPerSite != d.MaxSeriesPerSite || g.limits.MaxAttrsPerPoint != d.MaxAttrsPerPoint {
		t.Fatalf("zero limits must fall back to defaults: %+v", g.limits)
	}
}

func TestSeriesKey_StableAcrossCallOrder(t *testing.T) {
	a := seriesKey("m", "svc", `{"a":"1","b":"2"}`)
	b := seriesKey("m", "svc", `{"a":"1","b":"2"}`)
	if a != b {
		t.Fatalf("identical series must hash identically: %d vs %d", a, b)
	}
	if a == seriesKey("m", "svc2", `{"a":"1","b":"2"}`) {
		t.Fatal("different service must be a different series")
	}
}
