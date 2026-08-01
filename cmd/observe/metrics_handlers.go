package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/metrics"
)

// RegisterMetricsRoutes wires the metrics API onto the root router. Three
// concerns are bundled here:
//
//  1. OTLP HTTP ingest at POST /v1/metrics — API-key authenticated (parity
//     with /v1/traces); site_id comes from the validated key, not the body.
//  2. Metric-name listing at GET /api/v1/metrics/list — JWT-only read.
//  3. Aggregated query at GET /api/v1/metrics/query — JWT-only read.
//     Phase-2 callers can also request the per-label-set series shape via
//     GET /api/v1/metrics/series — see metricsSeriesHandler below.
//
// Kept in its own file so the merge-conflict surface against parallel
// agents stays a single line addition in main.go (mirrors the
// RegisterBoardsRoutes / RegisterAttributionRoutes convention).
// otlpChain is the shared ingest protection chain (API-key auth + rate limit +
// body cap) main.go applies to every /v1/<signal> route.
func RegisterMetricsRoutes(r *neutron.Router, jwtMW, otlpChain neutron.Middleware, svc *metrics.Service) {
	otlpHandler := metrics.NewOTLPHandler(svc)
	r.Handle("POST /v1/metrics", otlpChain(otlpHandler))

	r.Handle("GET /api/v1/metrics/list", jwtMW(metricsListHandler(svc)))
	r.Handle("GET /api/v1/metrics/query", jwtMW(metricsQueryHandler(svc)))
	r.Handle("GET /api/v1/metrics/series", jwtMW(metricsSeriesHandler(svc)))
}

func metricsListHandler(svc *metrics.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.URL.Query().Get("site_id")
		if siteID == "" {
			http.Error(w, `{"error":"site_id required"}`, http.StatusBadRequest)
			return
		}
		list, err := svc.ListMetrics(r.Context(), siteID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []metrics.MetricInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
}

// metricsQueryHandler exposes /api/v1/metrics/query — the Phase-1 single-
// series shape, preserved for backwards compatibility with the SDK and
// any embedded /metrics route logic that doesn't know about group-by.
//
// Parameters:
//
//	site_id      (required)
//	name         (required) metric name
//	from         (ms epoch, optional — defaults to now-1h)
//	to           (ms epoch, optional — defaults to now)
//	agg          last|avg|sum|min|max|rate|p50|p95|p99 (default: last)
//	step         15s|30s|60s|5m|1h|1d (default: 60s)
//	label.<key>  AND-joined exact-match label filter
func metricsQueryHandler(svc *metrics.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := parseQueryRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		points, err := svc.Query(r.Context(), req.siteID, req.name, req.labels, req.fromMs, req.toMs, req.agg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if points == nil {
			points = []metrics.Point{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(points)
	}
}

// metricsSeriesHandler exposes /api/v1/metrics/series — the Phase-2 fan-out
// shape. Same query parameters as /query plus:
//
//	group_by  comma-separated list of label keys to fan series out by
//
// Returns a JSON array of {labels, points} entries. With no group_by it
// returns one entry whose labels is {} so callers can unify rendering.
func metricsSeriesHandler(svc *metrics.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := parseQueryRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		series, err := svc.QuerySeries(r.Context(), req.siteID, req.name, req.labels, req.fromMs, req.toMs, metrics.QueryOptions{
			Agg:     req.agg,
			StepMs:  req.stepMs,
			GroupBy: req.groupBy,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if series == nil {
			series = []metrics.Series{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(series)
	}
}

// queryRequest is the parsed shape shared by the /query and /series
// handlers — they only differ on the group_by knob and the response shape.
type queryRequest struct {
	siteID  string
	name    string
	fromMs  int64
	toMs    int64
	agg     string
	stepMs  int64
	labels  map[string]string
	groupBy []string
}

func parseQueryRequest(r *http.Request) (*queryRequest, error) {
	q := r.URL.Query()
	siteID := q.Get("site_id")
	name := q.Get("name")
	if siteID == "" || name == "" {
		return nil, &httpErr{msg: `{"error":"site_id and name required"}`}
	}
	fromMs, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	toMs, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	now := time.Now().UTC().UnixMilli()
	if toMs == 0 {
		toMs = now
	}
	if fromMs == 0 {
		fromMs = toMs - 60*60*1000 // default: last hour
	}
	agg := q.Get("agg")
	if agg == "" {
		agg = "last"
	}
	if !metrics.IsValidAggregation(agg) {
		return nil, &httpErr{msg: `{"error":"unsupported agg"}`}
	}
	stepMs, err := metrics.ParseStep(q.Get("step"))
	if err != nil {
		return nil, &httpErr{msg: `{"error":"` + err.Error() + `"}`}
	}
	return &queryRequest{
		siteID:  siteID,
		name:    name,
		fromMs:  fromMs,
		toMs:    toMs,
		agg:     agg,
		stepMs:  stepMs,
		labels:  metrics.ParseLabelFilters(q),
		groupBy: metrics.ParseGroupBy(q.Get("group_by")),
	}, nil
}

// httpErr lets parseQueryRequest return both a status-formatted message and
// satisfy the error interface without dragging fmt.Errorf into every call
// site (tiny readability win).
type httpErr struct{ msg string }

func (e *httpErr) Error() string { return e.msg }
