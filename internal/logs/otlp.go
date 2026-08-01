package logs

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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
				TimeUnixNano   string   `json:"timeUnixNano"`
				SeverityNumber int      `json:"severityNumber"`
				SeverityText   string   `json:"severityText"`
				Body           otlpAny  `json:"body"`
				Attributes     []otlpKV `json:"attributes"`
				TraceID        string   `json:"traceId"`
				SpanID         string   `json:"spanId"`
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
}

func (v otlpAny) text() string {
	switch {
	case v.StringValue != "":
		return v.StringValue
	case v.IntValue != "":
		return v.IntValue
	case v.DoubleValue != 0:
		return fmt.Sprintf("%v", v.DoubleValue)
	case v.BoolValue:
		return "true"
	default:
		return ""
	}
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
	svc *LogService
}

func NewOTLPLogsHandler(svc *LogService) *OTLPLogsHandler {
	return &OTLPLogsHandler{svc: svc}
}

func (h *OTLPLogsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// site_id comes from the validated API key, never from the client.
	siteID := ingest.SiteIDFromContext(r.Context())
	if siteID == "" {
		http.Error(w, `{"error":"missing site_id"}`, http.StatusBadRequest)
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
	body, err := io.ReadAll(io.LimitReader(src, 10*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	ct := r.Header.Get("Content-Type")
	var inputs []LogInput
	if strings.HasPrefix(ct, "application/x-protobuf") || strings.HasPrefix(ct, "application/protobuf") {
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
		http.Error(w, ierr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// OTLP's success response is an empty ExportLogsServiceResponse; anything
	// JSON-shaped satisfies the exporters, which only check the status code.
	json.NewEncoder(w).Encode(map[string]any{"partialSuccess": map[string]any{
		"rejectedLogRecords": result.Rejected,
	}})
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
func ingestExport(ctx context.Context, svc *LogService, inputs []LogInput) (LogBatchResult, error) {
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
				out = append(out, LogInput{
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
				out = append(out, LogInput{
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
