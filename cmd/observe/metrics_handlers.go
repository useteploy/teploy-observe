package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/metrics"
)

// RegisterMetricsRoutes wires the W3.A Phase-1 metrics API onto the root
// router. Three concerns are bundled here:
//
//   1. OTLP HTTP ingest at POST /v1/metrics — no JWT (parity with /v1/traces).
//   2. Metric-name listing at GET /api/v1/metrics/list — JWT-only read.
//   3. Aggregated query at GET /api/v1/metrics/query — JWT-only read.
//
// Kept in its own file so the merge-conflict surface against parallel
// agents stays a single line addition in main.go (mirrors the
// RegisterBoardsRoutes / RegisterAttributionRoutes convention).
func RegisterMetricsRoutes(r *neutron.Router, jwtMW neutron.Middleware, svc *metrics.Service) {
	otlpHandler := metrics.NewOTLPHandler(svc)
	r.Handle("POST /v1/metrics", otlpHandler)

	r.Handle("GET /api/v1/metrics/list", jwtMW(metricsListHandler(svc)))
	r.Handle("GET /api/v1/metrics/query", jwtMW(metricsQueryHandler(svc)))
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

// metricsQueryHandler exposes /api/v1/metrics/query.
// Parameters:
//   site_id      (required)
//   name         (required) metric name
//   from         (ms epoch, optional — defaults to now-1h)
//   to           (ms epoch, optional — defaults to now)
//   agg          last|avg|sum|min|max (optional — defaults to last)
//   label.<key>  AND-joined exact-match label filter
func metricsQueryHandler(svc *metrics.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		siteID := q.Get("site_id")
		name := q.Get("name")
		if siteID == "" || name == "" {
			http.Error(w, `{"error":"site_id and name required"}`, http.StatusBadRequest)
			return
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
		labels := metrics.ParseLabelFilters(q)

		points, err := svc.Query(r.Context(), siteID, name, labels, fromMs, toMs, agg)
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
