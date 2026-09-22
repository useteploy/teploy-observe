package metrics

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/nucleus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

type recordingIngester struct {
	siteID string
	req    ExportMetricsRequest
	err    error
}

func (s *recordingIngester) Ingest(ctx context.Context, siteID string, req ExportMetricsRequest) (IngestResponse, error) {
	s.siteID = siteID
	s.req = req
	if s.err != nil {
		return IngestResponse{}, s.err
	}
	n := 0
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Gauge != nil {
					n += len(m.Gauge.DataPoints)
				}
				if m.Sum != nil {
					n += len(m.Sum.DataPoints)
				}
				if m.Histogram != nil {
					n += len(m.Histogram.DataPoints)
				}
			}
		}
	}
	return IngestResponse{OK: true, Points: n}, nil
}

func buildMetricsExport(t *testing.T, points int) []byte {
	t.Helper()
	dps := make([]*otlpmetrics.NumberDataPoint, 0, points)
	for i := 0; i < points; i++ {
		region, val := "us-east-1", 42.5
		if i == 1 {
			region, val = "eu-west-1", 17.0
		}
		dps = append(dps, &otlpmetrics.NumberDataPoint{
			TimeUnixNano: 1700000000000000000,
			Attributes: []*commonpb.KeyValue{{
				Key:   "region",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: region}},
			}},
			Value: &otlpmetrics.NumberDataPoint_AsDouble{AsDouble: val},
		})
	}
	req := &metricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*otlpmetrics.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "e2e-svc"}},
			}}},
			ScopeMetrics: []*otlpmetrics.ScopeMetrics{{
				Scope:   &commonpb.InstrumentationScope{Name: "manual"},
				Metrics: []*otlpmetrics.Metric{{Name: "otlp.http.test", Data: &otlpmetrics.Metric_Gauge{Gauge: &otlpmetrics.Gauge{DataPoints: dps}}}},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func metricsExportJSON(points int) []byte {
	var b strings.Builder
	b.WriteString(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"e2e-svc"}}]},"scopeMetrics":[{"metrics":[{"name":"otlp.http.test","gauge":{"dataPoints":[`)
	for i := 0; i < points; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		region, val := "us-east-1", 42.5
		if i == 1 {
			region, val = "eu-west-1", 17.0
		}
		fmt.Fprintf(&b, `{"timeUnixNano":"1700000000000000000","asDouble":%v,"attributes":[{"key":"region","value":{"stringValue":%q}}]}`, val, region)
	}
	b.WriteString(`]}}]}]}]}`)
	return []byte(b.String())
}

func postMetrics(t *testing.T, h http.Handler, contentType string, body []byte, siteID string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/metrics", contentType, body, siteID, false)
}

func doPost(t *testing.T, h http.Handler, path, contentType string, body []byte, siteID string, gzipped bool) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if gzipped {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err != nil {
			t.Fatalf("gzip: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		rdr = bytes.NewReader(buf.Bytes())
	} else {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if siteID != "" {
		req = req.WithContext(ingest.WithSiteID(req.Context(), siteID))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func TestOTLPMetrics_BinaryFullSuccess(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPHandler{svc: stub}

	rec := postMetrics(t, h, "application/x-protobuf", buildMetricsExport(t, 2), "site-otlp")
	requireStatus(t, rec, http.StatusOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}
	var resp metricspb.ExportMetricsServiceResponse
	if err := proto.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not an official ExportMetricsServiceResponse: %v (body %q)", err, rec.Body.String())
	}
	if resp.GetPartialSuccess() != nil {
		t.Errorf("full success must not carry partialSuccess, got %+v", resp.GetPartialSuccess())
	}

	if stub.siteID != "site-otlp" {
		t.Errorf("stub site = %q, want site-otlp", stub.siteID)
	}
	pts := stub.req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints
	if len(pts) != 2 {
		t.Fatalf("stub decoded %d data points, want 2", len(pts))
	}
	if pts[0].AsDouble != 42.5 || pts[1].AsDouble != 17.0 {
		t.Errorf("decoded values = %v/%v, want 42.5/17.0", pts[0].AsDouble, pts[1].AsDouble)
	}
}

func TestOTLPMetrics_JSONFullSuccess(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPHandler{svc: stub}

	rec := postMetrics(t, h, "application/json", metricsExportJSON(2), "site-otlp")
	requireStatus(t, rec, http.StatusOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var resp metricspb.ExportMetricsServiceResponse
	if err := protojson.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not protobuf-JSON: %v (body %q)", err, rec.Body.String())
	}
	if resp.GetPartialSuccess() != nil {
		t.Errorf("full success must not carry partialSuccess, got %+v", resp.GetPartialSuccess())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("full-success JSON body = %q, want {} (official empty-message encoding)", got)
	}
	if n := len(stub.req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints); n != 2 {
		t.Errorf("stub decoded %d points from JSON, want 2", n)
	}
}

func TestOTLPMetrics_ContentTypeParametersTolerated(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	rec := postMetrics(t, h, "application/x-protobuf; charset=utf-8", buildMetricsExport(t, 1), "site-otlp")
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("proto-with-params: content type = %q, want application/x-protobuf", ct)
	}

	rec = postMetrics(t, h, "application/json; charset=utf-8", metricsExportJSON(1), "site-otlp")
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("json-with-params: content type = %q, want application/json", ct)
	}
}

func TestOTLPMetrics_GzipEncoded(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPHandler{svc: stub}
	rec := doPost(t, h, "/v1/metrics", "application/x-protobuf", buildMetricsExport(t, 1), "site-otlp", true)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}
	if n := len(stub.req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints); n != 1 {
		t.Errorf("stub decoded %d points, want 1", n)
	}
}

func TestOTLPMetrics_StorageFailureIsRetryable(t *testing.T) {
	stub := &recordingIngester{err: errors.New("nucleus down")}
	h := &OTLPHandler{svc: stub}

	rec := postMetrics(t, h, "application/x-protobuf", buildMetricsExport(t, 1), "site-otlp")
	requireStatus(t, rec, http.StatusServiceUnavailable)
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q, want 5", ra)
	}
}

func TestOTLPMetrics_UnknownContentType(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	rec := postMetrics(t, h, "text/plain", metricsExportJSON(1), "site-otlp")
	requireStatus(t, rec, http.StatusUnsupportedMediaType)
	if !strings.Contains(rec.Body.String(), "text/plain") {
		t.Errorf("415 body must name the received type, got %q", rec.Body.String())
	}

	rec = postMetrics(t, h, "", metricsExportJSON(1), "site-otlp")
	requireStatus(t, rec, http.StatusUnsupportedMediaType)
}

func TestOTLPMetrics_MalformedPayloads(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	rec := postMetrics(t, h, "application/x-protobuf", []byte("this is not protobuf at all, not even close"), "site-otlp")
	requireStatus(t, rec, http.StatusBadRequest)

	rec = postMetrics(t, h, "application/json", []byte("{bad json"), "site-otlp")
	requireStatus(t, rec, http.StatusBadRequest)

	rec = doPost(t, h, "/v1/metrics", "application/x-protobuf", []byte("definitely not gzip"), "site-otlp", true)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestOTLPMetrics_OversizedDecompressedBody(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	// A tiny gzip that expands past the decompressed cap.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(make([]byte, 10*1024*1024+1)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req = req.WithContext(ingest.WithSiteID(req.Context(), "site-otlp"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

// The production chain wraps these handlers in neutron.BodyLimit
// (cmd/observe/metrics_handlers.go). An over-cap RAW body must surface as
// 413, not the old generic 400.
func TestOTLPMetrics_BodyLimitChainRawOverflow(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}
	chained := neutron.BodyLimit(1024)(h)

	rec := postMetrics(t, chained, "application/json", []byte(strings.Repeat("a", 4096)), "site-otlp")
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

func TestOTLPMetrics_MethodAndSiteGuards(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusMethodNotAllowed)

	rec = postMetrics(t, h, "application/json", metricsExportJSON(1), "")
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestOTLPMetrics_RoundTripStoresPoints drives the REAL service + storage and
// verifies stored points through the existing query path. Requires a live
// Nucleus (nucleustest.DSN skips otherwise).
func TestOTLPMetrics_RoundTripStoresPoints(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}

	site := fmt.Sprintf("otlp_rt_%d", time.Now().UnixNano())
	svc := NewService(db)
	h := NewOTLPHandler(svc)

	rec := postMetrics(t, h, "application/x-protobuf", buildMetricsExport(t, 2), site)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}

	// buildMetricsExport stamps points at 1700000000000000000 ns; query that
	// window (ms).
	aroundMs := int64(1700000000000)
	for _, tc := range []struct {
		region string
		want   float64
	}{
		{"us-east-1", 42.5},
		{"eu-west-1", 17.0},
	} {
		pts, err := svc.QuerySeries(ctx, site, "otlp.http.test", map[string]string{"region": tc.region},
			aroundMs-60_000, aroundMs+60_000, QueryOptions{Agg: "last"})
		if err != nil {
			t.Fatalf("QuerySeries(%s): %v", tc.region, err)
		}
		if len(pts) != 1 || len(pts[0].Points) == 0 {
			t.Fatalf("region %s: got %d series, want 1 with points", tc.region, len(pts))
		}
		if got := pts[0].Points[0].Value; got != tc.want {
			t.Errorf("region %s: stored value = %v, want %v", tc.region, got, tc.want)
		}
	}
}
