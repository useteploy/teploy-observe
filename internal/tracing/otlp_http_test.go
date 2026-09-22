package tracing

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

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// recordingIngester is the transport-boundary stub: it records the decoded
// request the handler produced (the ingest-effect check available without
// storage) and returns a canned result or error.
type recordingIngester struct {
	siteID string
	req    ExportTraceRequest
	err    error
}

func (s *recordingIngester) Ingest(ctx context.Context, siteID string, req ExportTraceRequest) (IngestResponse, error) {
	s.siteID = siteID
	s.req = req
	if s.err != nil {
		return IngestResponse{}, s.err
	}
	n := 0
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			n += len(ss.Spans)
		}
	}
	return IngestResponse{OK: true, Spans: n}, nil
}

var otlpTraceID = []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}

func buildTraceExport(t *testing.T, names ...string) []byte {
	t.Helper()
	spans := make([]*otlptrace.Span, 0, len(names))
	for i, name := range names {
		spans = append(spans, &otlptrace.Span{
			TraceId:           otlpTraceID,
			SpanId:            []byte{byte(0x11 + i), 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
			Name:              name,
			Kind:              otlptrace.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: 1700000000000000000,
			EndTimeUnixNano:   1700000000500000000,
		})
	}
	req := &tracepb.ExportTraceServiceRequest{
		ResourceSpans: []*otlptrace.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
			}}},
			ScopeSpans: []*otlptrace.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "manual"},
				Spans: spans,
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// traceExportJSON is the OTLP/JSON equivalent (ids hex — why this path is
// hand-decoded, see types.go).
func traceExportJSON(names ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[`)
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"%02x12131415161718","name":%q,"kind":2,"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000500000000"}`, 0x11+i, name)
	}
	b.WriteString(`]}]}]}`)
	return []byte(b.String())
}

func postTraces(t *testing.T, h http.Handler, contentType string, body []byte, siteID string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/traces", contentType, body, siteID, false)
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
		// The real context helper the API-key middleware uses — not a fake.
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

func TestOTLPTraces_BinaryFullSuccess(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPHandler{svc: stub}

	rec := postTraces(t, h, "application/x-protobuf", buildTraceExport(t, "GET /cart", "POST /pay"), "site-otlp")
	requireStatus(t, rec, http.StatusOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}
	var resp tracepb.ExportTraceServiceResponse
	if err := proto.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not an official ExportTraceServiceResponse: %v (body %q)", err, rec.Body.String())
	}
	if resp.GetPartialSuccess() != nil {
		t.Errorf("full success must not carry partialSuccess, got %+v", resp.GetPartialSuccess())
	}

	// Ingest effect: the stub received the decoded request.
	if stub.siteID != "site-otlp" {
		t.Errorf("stub site = %q, want site-otlp", stub.siteID)
	}
	spans := stub.req.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("stub decoded %d spans, want 2", len(spans))
	}
	if spans[0].Name != "GET /cart" || spans[0].TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("decoded span[0] = %q/%q", spans[0].Name, spans[0].TraceID)
	}
}

func TestOTLPTraces_JSONFullSuccess(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPHandler{svc: stub}

	rec := postTraces(t, h, "application/json", traceExportJSON("GET /cart", "POST /pay"), "site-otlp")
	requireStatus(t, rec, http.StatusOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var resp tracepb.ExportTraceServiceResponse
	if err := protojson.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not protobuf-JSON: %v (body %q)", err, rec.Body.String())
	}
	if resp.GetPartialSuccess() != nil {
		t.Errorf("full success must not carry partialSuccess, got %+v", resp.GetPartialSuccess())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("full-success JSON body = %q, want {} (official empty-message encoding)", got)
	}
	if len(stub.req.ResourceSpans[0].ScopeSpans[0].Spans) != 2 {
		t.Errorf("stub decoded %d spans from JSON, want 2", len(stub.req.ResourceSpans[0].ScopeSpans[0].Spans))
	}
}

func TestOTLPTraces_ContentTypeParametersTolerated(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	rec := postTraces(t, h, "application/x-protobuf; charset=utf-8", buildTraceExport(t, "a"), "site-otlp")
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("proto-with-params: content type = %q, want application/x-protobuf", ct)
	}

	rec = postTraces(t, h, "application/json; charset=utf-8", traceExportJSON("a"), "site-otlp")
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("json-with-params: content type = %q, want application/json", ct)
	}
}

func TestOTLPTraces_GzipEncoded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
		wantCT      string
	}{
		{"binary", "application/x-protobuf", buildTraceExport(t, "gz"), "application/x-protobuf"},
		{"json", "application/json", traceExportJSON("gz"), "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordingIngester{}
			h := &OTLPHandler{svc: stub}
			rec := doPost(t, h, "/v1/traces", tc.contentType, tc.body, "site-otlp", true)
			requireStatus(t, rec, http.StatusOK)
			if ct := rec.Header().Get("Content-Type"); ct != tc.wantCT {
				t.Errorf("content type = %q, want %q", ct, tc.wantCT)
			}
			if n := len(stub.req.ResourceSpans[0].ScopeSpans[0].Spans); n != 1 {
				t.Errorf("stub decoded %d spans, want 1", n)
			}
		})
	}
}

func TestOTLPTraces_StorageFailureIsRetryable(t *testing.T) {
	stub := &recordingIngester{err: errors.New("nucleus down")}
	h := &OTLPHandler{svc: stub}

	rec := postTraces(t, h, "application/x-protobuf", buildTraceExport(t, "a"), "site-otlp")
	requireStatus(t, rec, http.StatusServiceUnavailable)
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q, want 5", ra)
	}
}

func TestOTLPTraces_UnknownContentType(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	rec := postTraces(t, h, "text/plain", buildTraceExport(t, "a"), "site-otlp")
	requireStatus(t, rec, http.StatusUnsupportedMediaType)
	if !strings.Contains(rec.Body.String(), "text/plain") {
		t.Errorf("415 body must name the received type, got %q", rec.Body.String())
	}

	rec = postTraces(t, h, "", traceExportJSON("a"), "site-otlp")
	requireStatus(t, rec, http.StatusUnsupportedMediaType)
}

func TestOTLPTraces_MalformedPayloads(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	cases := []struct {
		name        string
		contentType string
		body        []byte
		gzip        bool
	}{
		{"protobuf", "application/x-protobuf", []byte("this is not protobuf at all, not even close"), false},
		{"json", "application/json", []byte("{bad json"), false},
		{"gzip", "application/x-protobuf", []byte("definitely not gzip"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPost(t, h, "/v1/traces", tc.contentType, tc.body, "site-otlp", tc.gzip)
			requireStatus(t, rec, http.StatusBadRequest)
		})
	}
}

func TestOTLPTraces_OversizedDecompressedBody(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req = req.WithContext(ingest.WithSiteID(req.Context(), "site-otlp"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

// The production chain wraps these handlers in neutron.BodyLimit
// (cmd/observe/main.go otlpChain). An over-cap RAW body must surface as 413,
// not the old generic 400.
func TestOTLPTraces_BodyLimitChainRawOverflow(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}
	chained := neutron.BodyLimit(1024)(h)

	rec := postTraces(t, chained, "application/json", bytes.Repeat([]byte("a"), 4096), "site-otlp")
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

func TestOTLPTraces_MethodAndSiteGuards(t *testing.T) {
	h := &OTLPHandler{svc: &recordingIngester{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusMethodNotAllowed)

	rec = postTraces(t, h, "application/json", traceExportJSON("a"), "")
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestOTLPTraces_RoundTripStoresSpans drives the REAL service + storage and
// verifies stored counts through the existing query path. Requires a live
// Nucleus (nucleustest.DSN skips otherwise).
func TestOTLPTraces_RoundTripStoresSpans(t *testing.T) {
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
	h := NewOTLPHandler(NewIngestService(db))

	rec := postTraces(t, h, "application/x-protobuf", buildTraceExport(t, "GET /cart", "POST /pay"), site)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}

	stored, err := NewQueryService(db).GetTrace(ctx, "0102030405060708090a0b0c0d0e0f10", site)
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d spans, want 2", len(stored))
	}
}
