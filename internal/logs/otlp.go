package logs

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// OTLP logs ingest at /v1/logs — the third OTLP signal, alongside the traces
// and metrics endpoints. Without it an OTel SDK or Collector configured to send
// all three got a 405 for logs and silently dropped them.
//
// Records land in the same store as /api/v1/logs, so pipelines, search and the
// UI work on OTLP logs with no further wiring.

// otlpLogsJSON mirrors the OTLP/JSON encoding. Hand-written rather than
// decoded via protojson because OTLP/JSON deviates from the standard protobuf
// JSON mapping: trace_id and span_id are hex strings there, not base64.
type otlpLogsJSON struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []otlpKV `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				ObservedTimeUnixNano string   `json:"observedTimeUnixNano"`
				TimeUnixNano         string   `json:"timeUnixNano"`
				SeverityNumber       int      `json:"severityNumber"`
				SeverityText         string   `json:"severityText"`
				Body                 otlpAny  `json:"body"`
				Attributes           []otlpKV `json:"attributes"`
				TraceID              string   `json:"traceId"`
				SpanID               string   `json:"spanId"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type otlpKV struct {
	Key   string  `json:"key"`
	Value otlpAny `json:"value"`
}

type otlpAny struct {
	StringValue string  `json:"stringValue,omitempty"`
	IntValue    string  `json:"intValue,omitempty"`
	BoolValue   bool    `json:"boolValue,omitempty"`
	DoubleValue float64 `json:"doubleValue,omitempty"`

	// kind records which field the producer set, so false, 0, 0.0 and "" are
	// kept instead of being indistinguishable from "not set".
	kind byte
}

const (
	otlpUnset byte = iota
	otlpString
	otlpInt
	otlpBool
	otlpDouble
)

func (v *otlpAny) UnmarshalJSON(b []byte) error {
	*v = otlpAny{}
	var w struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		BoolValue   *bool    `json:"boolValue"`
		DoubleValue *float64 `json:"doubleValue"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	switch {
	case w.StringValue != nil:
		*v = otlpAny{StringValue: *w.StringValue, kind: otlpString}
	case w.IntValue != nil:
		*v = otlpAny{IntValue: *w.IntValue, kind: otlpInt}
	case w.BoolValue != nil:
		*v = otlpAny{BoolValue: *w.BoolValue, kind: otlpBool}
	case w.DoubleValue != nil:
		*v = otlpAny{DoubleValue: *w.DoubleValue, kind: otlpDouble}
	}
	return nil
}

func (v otlpAny) text() string {
	k := v.kind
	if k == otlpUnset {
		switch {
		case v.StringValue != "":
			k = otlpString
		case v.IntValue != "":
			k = otlpInt
		case v.DoubleValue != 0:
			k = otlpDouble
		case v.BoolValue:
			k = otlpBool
		}
	}
	switch k {
	case otlpString:
		return v.StringValue
	case otlpInt:
		return v.IntValue
	case otlpDouble:
		return fmt.Sprintf("%v", v.DoubleValue)
	case otlpBool:
		return strconv.FormatBool(v.BoolValue)
	}
	return ""
}

// severityToLevel maps an OTLP severity number onto the level vocabulary the
// rest of Observe uses. SeverityText wins when the producer set it, since it is
// the producer's own name for the level.
func severityToLevel(number int, text string) string {
	if text != "" {
		return strings.ToLower(text)
	}
	switch {
	case number >= 21:
		return "fatal"
	case number >= 17:
		return "error"
	case number >= 13:
		return "warn"
	case number >= 9:
		return "info"
	case number >= 5:
		return "debug"
	case number >= 1:
		return "trace"
	default:
		return "info"
	}
}

// OTLPLogsHandler serves POST /v1/logs in both wire formats.
type OTLPLogsHandler struct {
	svc logIngester
}

// logIngester is the transport-boundary seam the handler tests drive with a
// recording stub. *LogService satisfies it; the service's own DTOs and
// storage stay untouched.
type logIngester interface {
	IngestLogs(ctx context.Context, inputs []LogInput) (LogBatchResult, error)
}

func NewOTLPLogsHandler(svc *LogService) *OTLPLogsHandler {
	return &OTLPLogsHandler{svc: svc}
}

// otlpMaxDecompressedBytes caps the DECOMPRESSED export body. The production
// chain (cmd/observe otlpChain) separately caps the raw bytes via
// neutron.BodyLimit; this one bounds gzip expansion.
const otlpMaxDecompressedBytes = 10 * 1024 * 1024

func (h *OTLPLogsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// site_id comes from the validated API key, never from the client.
	siteID := ingest.SiteIDFromContext(r.Context())
	if siteID == "" {
		http.Error(w, `{"error":"missing site_id"}`, http.StatusBadRequest)
		return
	}

	// Parse the media type so parameters (`application/json; charset=...`)
	// are tolerated; anything outside the two OTLP wire formats is a 415,
	// not a silent fallthrough to the JSON decoder.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, fmt.Sprintf("unsupported content type %q", r.Header.Get("Content-Type")), http.StatusUnsupportedMediaType)
		return
	}
	isProto := false
	switch mediaType {
	case "application/x-protobuf", "application/protobuf":
		isProto = true
	case "application/json":
	default:
		http.Error(w, fmt.Sprintf("unsupported content type %q", mediaType), http.StatusUnsupportedMediaType)
		return
	}

	var src io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, gerr := gzip.NewReader(r.Body)
		if gerr != nil {
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		src = gz
	}

	// Read one byte past the cap so an oversized (e.g. gzip-expanded) body is
	// detected instead of silently truncated into a misleading 400.
	body, err := io.ReadAll(io.LimitReader(src, otlpMaxDecompressedBytes+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > otlpMaxDecompressedBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var inputs []LogInput
	if isProto {
		inputs, err = protoLogInputs(body, siteID)
	} else {
		inputs, err = jsonLogInputs(body, siteID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result, ierr := ingestExport(r.Context(), h.svc, inputs)
	if ierr != nil {
		// Audit F13: a storage failure is retryable. OTLP exporters treat
		// 500 as non-retryable and drop the batch; 503 + Retry-After tells
		// them to re-send instead of silently losing every record.
		if errors.Is(ierr, ErrBatchTooLarge) {
			http.Error(w, ierr.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Retry-After", "5")
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}

	resp := &logspb.ExportLogsServiceResponse{}
	if result.Rejected > 0 {
		// A clean reject carries the count with an empty errorMessage; the
		// per-record reasons are not in the service's result DTO and are not
		// invented here.
		resp.PartialSuccess = &logspb.ExportLogsPartialSuccess{
			RejectedLogRecords: int64(result.Rejected),
		}
	}
	writeExportResponse(w, isProto, resp)
}

// writeExportResponse encodes an ExportLogsServiceResponse in the request's
// wire format: proto bytes for protobuf requests, protobuf-JSON for JSON
// requests. The JSON encoder renders int64 fields as strings — that IS the
// official protobuf-JSON mapping, not a bug to fix.
func writeExportResponse(w http.ResponseWriter, isProto bool, resp *logspb.ExportLogsServiceResponse) {
	if isProto {
		body, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, "encode response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(body)
		return
	}
	body, err := protojson.Marshal(resp)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// ingestExport feeds one OTLP export through IngestLogs in maxLogBatchSize
// chunks.
//
// maxLogBatchSize caps a single /logs/batch API request, but an OTLP export is
// not that request shape: a stock OTel SDK batch log processor exports 512
// records by default and the Collector's send_batch_size defaults to 8192, so
// handing an export straight to IngestLogs rejects the whole thing above 200.
// That surfaces as a 500, which the OTLP spec classes as non-retryable — the
// exporter drops the batch instead of resending, losing every record. The cap
// stays as-is (it also bounds the multi-row INSERT); the export is split to fit.
func ingestExport(ctx context.Context, svc logIngester, inputs []LogInput) (LogBatchResult, error) {
	total := LogBatchResult{}
	for start := 0; start < len(inputs); start += maxLogBatchSize {
		end := start + maxLogBatchSize
		if end > len(inputs) {
			end = len(inputs)
		}
		res, err := svc.IngestLogs(ctx, inputs[start:end])
		total.Accepted += res.Accepted
		total.Rejected += res.Rejected
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func jsonLogInputs(body []byte, siteID string) ([]LogInput, error) {
	var req otlpLogsJSON
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var out []LogInput
	for _, rl := range req.ResourceLogs {
		service := ""
		resAttrs := map[string]any{}
		for _, kv := range rl.Resource.Attributes {
			resAttrs[kv.Key] = kv.Value.text()
			if kv.Key == "service.name" {
				service = kv.Value.text()
			}
		}
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				attrs := map[string]any{}
				for k, v := range resAttrs {
					attrs[k] = v
				}
				for _, kv := range lr.Attributes {
					attrs[kv.Key] = kv.Value.text()
				}
				ts, err := logEventTime(lr.TimeUnixNano, lr.ObservedTimeUnixNano)
				if err != nil {
					return nil, err
				}
				out = append(out, LogInput{
					TimestampNs: ts,
					SiteID:      siteID,
					Level:       severityToLevel(lr.SeverityNumber, lr.SeverityText),
					Message:     lr.Body.text(),
					ServiceName: service,
					TraceID:     lr.TraceID,
					SpanID:      lr.SpanID,
					Attributes:  attrs,
				})
			}
		}
	}
	return out, nil
}

func protoLogInputs(body []byte, siteID string) ([]LogInput, error) {
	var pb logspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &pb); err != nil {
		return nil, fmt.Errorf("invalid protobuf: %w", err)
	}
	var out []LogInput
	for _, rl := range pb.GetResourceLogs() {
		service := ""
		resAttrs := map[string]any{}
		for _, kv := range rl.GetResource().GetAttributes() {
			resAttrs[kv.GetKey()] = anyText(kv.GetValue())
			if kv.GetKey() == "service.name" {
				service = anyText(kv.GetValue())
			}
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := map[string]any{}
				for k, v := range resAttrs {
					attrs[k] = v
				}
				for _, kv := range lr.GetAttributes() {
					attrs[kv.GetKey()] = anyText(kv.GetValue())
				}
				if lr.GetTimeUnixNano() > math.MaxInt64 || lr.GetObservedTimeUnixNano() > math.MaxInt64 {
					return nil, fmt.Errorf("invalid log event timestamp")
				}
				ts := lr.GetTimeUnixNano()
				if ts == 0 {
					ts = lr.GetObservedTimeUnixNano()
				}
				out = append(out, LogInput{
					TimestampNs: int64(ts),
					SiteID:      siteID,
					Level:       severityToLevel(int(lr.GetSeverityNumber()), lr.GetSeverityText()),
					Message:     anyText(lr.GetBody()),
					ServiceName: service,
					TraceID:     hex.EncodeToString(lr.GetTraceId()),
					SpanID:      hex.EncodeToString(lr.GetSpanId()),
					Attributes:  attrs,
				})
			}
		}
	}
	return out, nil
}

func anyText(v *commonpb.AnyValue) string {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprintf("%t", val.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", val.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%v", val.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(val.BytesValue)
	case nil:
		return ""
	default:
		return v.String()
	}
}

// Event time wins; observed time is the fallback; zero means receipt time.
func logEventTime(event, observed string) (int64, error) {
	parse := func(v string) (int64, error) {
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid log event timestamp")
		}
		return n, nil
	}
	n, err := parse(event)
	if err != nil {
		return 0, err
	}
	o, err := parse(observed)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		n = o
	}
	return n, nil
}
