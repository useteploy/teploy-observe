package metrics

// O12 ingest-side cardinality guard: bounded label maps and a bounded
// per-site series registry so a misbehaving exporter cannot grow ingest
// memory without bound. The guard TRUNCATES over-limit attributes with
// counters, REFUSES over-limit request sizes with a labeled error, and
// DROPS data points that would introduce a series past the per-site cap
// (counted, never a silent loss). Nothing here can OOM: every structure
// is sized by a declared limit.

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// CardinalityLimits are the declared O12 defaults for the metrics ingest
// path. All are env-tunable at startup (LoadCardinalityLimitsFromEnv).
type CardinalityLimits struct {
	// MaxAttrsPerPoint caps the attribute (label) map carried by ONE
	// data point. Over-limit maps are truncated (lexicographically first
	// keys kept) and counted — truncation, not refusal, because OTLP
	// exporters legitimately re-send full label sets and a hard refusal
	// would drop the whole point over one extra label.
	MaxAttrsPerPoint int
	// MaxAttrValueBytes caps one attribute value (UTF-8-safe truncation,
	// counted). Keys are capped at twice this.
	MaxAttrValueBytes int
	// MaxPointsPerRequest refuses the whole export (labeled 413) once
	// one request carries more data points than this — a request-level
	// bound on the rows slice built before any INSERT.
	MaxPointsPerRequest int
	// MaxSeriesPerSite caps the distinct (metric, service, label-set)
	// series remembered per site. A data point whose series is unknown
	// AND the cap is full is DROPPED and counted; known series keep
	// flowing. In-memory by design (restart re-learns from traffic);
	// the honest label rides Stats().
	MaxSeriesPerSite int
}

// DefaultCardinalityLimits declares the shipped ceilings. 32 labels per
// point matches common OTel SDK resource limits; 20k series per site is
// roughly 200 well-instrumented services x 100 label combinations, far
// above the small-install target and far below anything that can OOM a
// 512 MiB engine through per-series rows.
func DefaultCardinalityLimits() CardinalityLimits {
	return CardinalityLimits{
		MaxAttrsPerPoint:    32,
		MaxAttrValueBytes:   200,
		MaxPointsPerRequest: 20000,
		MaxSeriesPerSite:    20000,
	}
}

// LoadCardinalityLimitsFromEnv reads the O12 cardinality knobs over
// defaults:
//
//	OBSERVE_METRICS_MAX_ATTRS_PER_POINT
//	OBSERVE_METRICS_MAX_ATTR_VALUE_BYTES
//	OBSERVE_METRICS_MAX_POINTS_PER_REQUEST
//	OBSERVE_METRICS_MAX_SERIES_PER_SITE
//
// Unparsable or non-positive values keep the default and log a warning.
func LoadCardinalityLimitsFromEnv(getenv func(string) string, logger *slog.Logger) CardinalityLimits {
	c := DefaultCardinalityLimits()
	if getenv == nil {
		return c
	}
	knob := func(name string, dst *int) {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			if logger != nil {
				logger.Warn("metrics cardinality knob is not a positive integer — using the default", "env", name, "raw", raw)
			}
			return
		}
		*dst = n
	}
	knob("OBSERVE_METRICS_MAX_ATTRS_PER_POINT", &c.MaxAttrsPerPoint)
	knob("OBSERVE_METRICS_MAX_ATTR_VALUE_BYTES", &c.MaxAttrValueBytes)
	knob("OBSERVE_METRICS_MAX_POINTS_PER_REQUEST", &c.MaxPointsPerRequest)
	knob("OBSERVE_METRICS_MAX_SERIES_PER_SITE", &c.MaxSeriesPerSite)
	return c
}

// ErrTooManyPoints refuses an export whose data-point count exceeds
// MaxPointsPerRequest. The HTTP layer maps it to 413 (permanent — the
// exporter must split the batch), distinct from the 503 retry path.
type ErrTooManyPoints struct {
	Points, Limit int
}

func (e *ErrTooManyPoints) Error() string {
	return fmt.Sprintf("metrics: export carries %d data points, above the declared maximum of %d — split the export", e.Points, e.Limit)
}

// CardinalityStats is the /healthz view of the guard.
type CardinalityStats struct {
	AttrsTruncated     int64 `json:"attrs_truncated_points"`
	ValuesTruncated    int64 `json:"values_truncated"`
	PointsRefusedBatch int64 `json:"points_refused_batch"`
	SeriesDropped      int64 `json:"series_dropped_points"`
	SitesAtSeriesCap   int   `json:"sites_at_series_cap"`
	SeriesLimit        int   `json:"series_limit_per_site"`
}

// seriesGuard is the bounded per-site series registry. Memory is bounded
// by MaxSeriesPerSite entries per site; sites themselves are bounded by
// the site count the deployment actually has (same trust level as every
// other per-site map in the process).
type seriesGuard struct {
	mu     sync.Mutex
	limits CardinalityLimits
	sites  map[string]map[uint64]struct{}
	stats  CardinalityStats
}

func newSeriesGuard(limits CardinalityLimits) *seriesGuard {
	if limits.MaxSeriesPerSite <= 0 || limits.MaxAttrsPerPoint <= 0 || limits.MaxAttrValueBytes <= 0 || limits.MaxPointsPerRequest <= 0 {
		d := DefaultCardinalityLimits()
		if limits.MaxSeriesPerSite <= 0 {
			limits.MaxSeriesPerSite = d.MaxSeriesPerSite
		}
		if limits.MaxAttrsPerPoint <= 0 {
			limits.MaxAttrsPerPoint = d.MaxAttrsPerPoint
		}
		if limits.MaxAttrValueBytes <= 0 {
			limits.MaxAttrValueBytes = d.MaxAttrValueBytes
		}
		if limits.MaxPointsPerRequest <= 0 {
			limits.MaxPointsPerRequest = d.MaxPointsPerRequest
		}
	}
	return &seriesGuard{
		limits: limits,
		sites:  make(map[string]map[uint64]struct{}),
	}
}

// seriesKey hashes the (metric, service, label-set) identity. The attrs
// JSON is deterministic (MarshalAttrs sorts keys), so the hash is stable
// for the same series across requests and restarts.
func seriesKey(name, service, attrsJSON string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(service))
	h.Write([]byte{0})
	h.Write([]byte(attrsJSON))
	return h.Sum64()
}

// admit reports whether the series may be written. A known series always
// passes; an unknown one passes only while the site is under its cap.
func (g *seriesGuard) admit(siteID string, key uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	set, ok := g.sites[siteID]
	if !ok {
		set = make(map[uint64]struct{})
		g.sites[siteID] = set
	}
	if _, known := set[key]; known {
		return true
	}
	if len(set) >= g.limits.MaxSeriesPerSite {
		g.stats.SeriesDropped++
		return false
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], key)
	set[key] = struct{}{}
	return true
}

// truncateAttrs bounds one label map in place: at most limits
// .MaxAttrsPerPoint entries (lexicographically first kept — the choice is
// deterministic so identical inputs always truncate identically), each
// value capped at limits.MaxAttrValueBytes without splitting a UTF-8
// rune. Counters record both truncation kinds once per affected point.
func (g *seriesGuard) truncateAttrs(site string, m map[string]string) map[string]string {
	if len(m) <= g.limits.MaxAttrsPerPoint && !anyOverLong(m, g.limits.MaxAttrValueBytes) {
		return m
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	attrsHit := false
	valuesHit := false
	for i, k := range keys {
		if i >= g.limits.MaxAttrsPerPoint {
			attrsHit = true
			continue
		}
		v := m[k]
		if len(v) > g.limits.MaxAttrValueBytes {
			v = truncateUTF8(v, g.limits.MaxAttrValueBytes)
			valuesHit = true
		}
		out[k] = v
	}
	g.mu.Lock()
	if attrsHit {
		g.stats.AttrsTruncated++
	}
	if valuesHit {
		g.stats.ValuesTruncated++
	}
	g.mu.Unlock()
	return out
}

func anyOverLong(m map[string]string, limit int) bool {
	for _, v := range m {
		if len(v) > limit {
			return true
		}
	}
	return false
}

// truncateUTF8 shortens s to at most limit bytes without splitting a
// multi-byte character (same rule as internal/ingest's field truncation).
func truncateUTF8(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if cut <= 0 {
		return ""
	}
	return s[:cut]
}

// checkBatch refuses a request whose point count exceeds the request
// ceiling (labeled, permanent) and records the refusal.
func (g *seriesGuard) checkBatch(points int) error {
	if points > g.limits.MaxPointsPerRequest {
		g.mu.Lock()
		g.stats.PointsRefusedBatch++
		g.mu.Unlock()
		return &ErrTooManyPoints{Points: points, Limit: g.limits.MaxPointsPerRequest}
	}
	return nil
}

// Stats snapshots the counters plus the sites-at-cap gauge.
func (g *seriesGuard) Stats() CardinalityStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stats
	s.SeriesLimit = g.limits.MaxSeriesPerSite
	atCap := 0
	for _, set := range g.sites {
		if len(set) >= g.limits.MaxSeriesPerSite {
			atCap++
		}
	}
	s.SitesAtSeriesCap = atCap
	return s
}
