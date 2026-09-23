package logs

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

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/neutron-build/neutron/go/nucleus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// recordingIngester is the transport-boundary stub. It records every chunk
// ingestExport hands it so tests can verify the decoded LogInputs and the
// maxLogBatchSize chunking through the real handler.
type recordingIngester struct {
	calls   int
	inputs  []LogInput
	result  LogBatchResult
	err     error
	lastReq []LogInput
}

func (s *recordingIngester) IngestLogs(ctx context.Context, inputs []LogInput) (LogBatchResult, error) {
	s.calls++
	s.inputs = append(s.inputs, inputs...)
	s.lastReq = inputs
	if s.err != nil {
		return LogBatchResult{}, s.err
	}
	return s.result, nil
}

func buildLogsExport(t *testing.T, messages ...string) []byte {
	t.Helper()
	records := make([]*otlplogs.LogRecord, 0, len(messages))
	for i, msg := range messages {
		records = append(records, &otlplogs.LogRecord{
			SeverityNumber: otlplogs.SeverityNumber_SEVERITY_NUMBER_ERROR,
			Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: msg}},
			TraceId:        []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, byte(0x10 + i)},
			SpanId:         []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, byte(0x18 + i)},
		})
	}
	req := &logspb.ExportLogsServiceRequest{
		ResourceLogs: []*otlplogs.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "worker"}},
			}}},
			ScopeLogs: []*otlplogs.ScopeLogs{{LogRecords: records}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func logsExportJSON(messages ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"worker"}}]},"scopeLogs":[{"logRecords":[`)
	for i, msg := range messages {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"severityNumber":17,"body":{"stringValue":%q},"traceId":"0102030405060708090a0b0c0d0e0f%02x","spanId":"11121314151617%02x"}`, msg, 0x10+i, 0x18+i)
	}
	b.WriteString(`]}]}]}`)
	return []byte(b.String())
}

func postLogs(t *testing.T, h http.Handler, contentType string, body []byte, siteID string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/logs", contentType, body, siteID, false)
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

func TestOTLPLogs_BinaryFullSuccess(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPLogsHandler{svc: stub}

	rec := postLogs(t, h, "application/x-protobuf", buildLogsExport(t, "job failed", "job retried"), "site-otlp")
	requireStatus(t, rec, http.StatusOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}
	var resp logspb.ExportLogsServiceResponse
	if err := proto.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not an official ExportLogsServiceResponse: %v (body %q)", err, rec.Body.String())
	}
	if resp.GetPartialSuccess() != nil {
		t.Errorf("full success must not carry partialSuccess, got %+v", resp.GetPartialSuccess())
	}

	if stub.calls != 1 || len(stub.inputs) != 2 {
		t.Fatalf("stub received %d calls / %d records, want 1/2", stub.calls, len(stub.inputs))
	}
	if stub.inputs[0].Message != "job failed" || stub.inputs[0].ServiceName != "worker" {
		t.Errorf("decoded record[0] = %+v", stub.inputs[0])
	}
	if stub.inputs[0].SiteID != "site-otlp" {
		t.Errorf("decoded site = %q, want site-otlp (context site, not body)", stub.inputs[0].SiteID)
	}
	if stub.inputs[0].TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("decoded traceId = %q, want hex form", stub.inputs[0].TraceID)
	}
}

func TestOTLPLogs_JSONFullSuccess(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPLogsHandler{svc: stub}

	rec := postLogs(t, h, "application/json", logsExportJSON("job failed"), "site-otlp")
	requireStatus(t, rec, http.StatusOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var resp logspb.ExportLogsServiceResponse
	if err := protojson.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not protobuf-JSON: %v (body %q)", err, rec.Body.String())
	}
	if resp.GetPartialSuccess() != nil {
		t.Errorf("full success must not carry partialSuccess, got %+v", resp.GetPartialSuccess())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("full-success JSON body = %q, want {} (official empty-message encoding)", got)
	}
	if len(stub.inputs) != 1 || stub.inputs[0].Message != "job failed" {
		t.Errorf("stub decoded %+v from JSON", stub.inputs)
	}
}

func TestOTLPLogs_PartialRejection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"binary", "application/x-protobuf", buildLogsExport(t, "a", "b", "c")},
		{"json", "application/json", logsExportJSON("a", "b", "c")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordingIngester{result: LogBatchResult{Accepted: 1, Rejected: 2}}
			h := &OTLPLogsHandler{svc: stub}

			rec := postLogs(t, h, tc.contentType, tc.body, "site-otlp")
			requireStatus(t, rec, http.StatusOK)

			var resp logspb.ExportLogsServiceResponse
			if tc.name == "binary" {
				if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
					t.Errorf("content type = %q, want application/x-protobuf", ct)
				}
				if err := proto.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("response is not an official ExportLogsServiceResponse: %v", err)
				}
			} else {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("content type = %q, want application/json", ct)
				}
				if err := protojson.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("response is not protobuf-JSON: %v (body %q)", err, rec.Body.String())
				}
				// protojson renders int64 as a JSON string — the official shape.
				if !strings.Contains(rec.Body.String(), `"rejectedLogRecords":"2"`) {
					t.Errorf("JSON body %q must carry rejectedLogRecords as the official string-encoded int64", rec.Body.String())
				}
			}
			ps := resp.GetPartialSuccess()
			if ps == nil {
				t.Fatal("partial success response missing partialSuccess")
			}
			if ps.GetRejectedLogRecords() != 2 {
				t.Errorf("rejectedLogRecords = %d, want 2", ps.GetRejectedLogRecords())
			}
			if ps.GetErrorMessage() != "" {
				t.Errorf("clean reject must carry an empty errorMessage, got %q", ps.GetErrorMessage())
			}
		})
	}
}

// A >200-record export is chunked by ingestExport (maxLogBatchSize), never
// rejected wholesale.
func TestOTLPLogs_LargeExportChunked(t *testing.T) {
	msgs := make([]string, 201)
	for i := range msgs {
		msgs[i] = fmt.Sprintf("line %d", i)
	}
	stub := &recordingIngester{result: LogBatchResult{Accepted: 200}}
	h := &OTLPLogsHandler{svc: stub}

	rec := postLogs(t, h, "application/x-protobuf", buildLogsExport(t, msgs...), "site-otlp")
	requireStatus(t, rec, http.StatusOK)
	if stub.calls != 2 {
		t.Errorf("stub calls = %d, want 2 (chunked at maxLogBatchSize)", stub.calls)
	}
	if len(stub.inputs) != 201 {
		t.Errorf("stub received %d records, want 201", len(stub.inputs))
	}
}

func TestOTLPLogs_GzipEncoded(t *testing.T) {
	stub := &recordingIngester{}
	h := &OTLPLogsHandler{svc: stub}
	rec := doPost(t, h, "/v1/logs", "application/x-protobuf", buildLogsExport(t, "gz"), "site-otlp", true)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}
	if len(stub.inputs) != 1 || stub.inputs[0].Message != "gz" {
		t.Errorf("stub decoded %+v", stub.inputs)
	}
}

func TestOTLPLogs_StorageFailureIsRetryable(t *testing.T) {
	stub := &recordingIngester{err: errors.New("nucleus down")}
	h := &OTLPLogsHandler{svc: stub}

	rec := postLogs(t, h, "application/x-protobuf", buildLogsExport(t, "a"), "site-otlp")
	requireStatus(t, rec, http.StatusServiceUnavailable)
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q, want 5", ra)
	}
}

func TestOTLPLogs_UnknownContentType(t *testing.T) {
	h := &OTLPLogsHandler{svc: &recordingIngester{}}

	rec := postLogs(t, h, "text/plain", logsExportJSON("a"), "site-otlp")
	requireStatus(t, rec, http.StatusUnsupportedMediaType)
	if !strings.Contains(rec.Body.String(), "text/plain") {
		t.Errorf("415 body must name the received type, got %q", rec.Body.String())
	}

	rec = postLogs(t, h, "", logsExportJSON("a"), "site-otlp")
	requireStatus(t, rec, http.StatusUnsupportedMediaType)
}

func TestOTLPLogs_MalformedPayloads(t *testing.T) {
	h := &OTLPLogsHandler{svc: &recordingIngester{}}

	rec := postLogs(t, h, "application/x-protobuf", []byte("this is not protobuf at all, not even close"), "site-otlp")
	requireStatus(t, rec, http.StatusBadRequest)

	rec = postLogs(t, h, "application/json", []byte("{bad json"), "site-otlp")
	requireStatus(t, rec, http.StatusBadRequest)

	rec = doPost(t, h, "/v1/logs", "application/x-protobuf", []byte("definitely not gzip"), "site-otlp", true)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestOTLPLogs_OversizedDecompressedBody(t *testing.T) {
	h := &OTLPLogsHandler{svc: &recordingIngester{}}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(make([]byte, 10*1024*1024+1)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req = req.WithContext(ingest.WithSiteID(req.Context(), "site-otlp"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

// The production chain wraps these handlers in neutron.BodyLimit
// (cmd/observe/main.go otlpChain). An over-cap RAW body must surface as 413.
func TestOTLPLogs_BodyLimitChainRawOverflow(t *testing.T) {
	h := &OTLPLogsHandler{svc: &recordingIngester{}}
	chained := neutron.BodyLimit(1024)(h)

	rec := postLogs(t, chained, "application/json", []byte(strings.Repeat("a", 4096)), "site-otlp")
	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
}

func TestOTLPLogs_MethodAndSiteGuards(t *testing.T) {
	h := &OTLPLogsHandler{svc: &recordingIngester{}}

	req := httptest.NewRequest(http.MethodGet, "/v1/logs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusMethodNotAllowed)

	rec = postLogs(t, h, "application/json", logsExportJSON("a"), "")
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestOTLPLogs_RoundTripStoresRecords drives the REAL service + storage and
// verifies stored records through the existing search path. Requires a live
// Nucleus (nucleustest.DSN skips otherwise).
func TestOTLPLogs_RoundTripStoresRecords(t *testing.T) {
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
	svc := NewLogService(db)
	h := NewOTLPLogsHandler(svc)

	rec := postLogs(t, h, "application/x-protobuf", buildLogsExport(t, "roundtrip failed", "roundtrip ok"), site)
	requireStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}

	logs, err := svc.SearchLogs(ctx, site, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("stored %d records, want 2", len(logs))
	}
}
