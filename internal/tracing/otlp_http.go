package tracing

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// otlpMaxDecompressedBytes caps the DECOMPRESSED export body. The production
// chain (cmd/observe otlpChain) separately caps the raw bytes via
// neutron.BodyLimit; this one bounds gzip expansion.
const otlpMaxDecompressedBytes = 10 * 1024 * 1024

// OTLPHandler accepts OTLP traces over HTTP at /v1/traces, in both wire
// formats: application/x-protobuf (what OTLP exporters send by default) and
// application/json. Protobuf is translated into the JSON-shaped request in
// otlp_proto.go so there is only one ingest path.
//
// Responses follow OTLP/HTTP: the export response is encoded in the request's
// wire format (application/x-protobuf or protobuf-JSON) so a stock exporter
// can parse what it gets back.
//
// gRPC is not served here. Point an exporter at HTTP, or put a Collector in
// front — every major exporter supports HTTP transport.
type OTLPHandler struct {
	svc traceIngester
}

// traceIngester is the transport-boundary seam the handler tests drive with a
// recording stub. *IngestService satisfies it; the service's own DTOs and
// storage stay untouched.
type traceIngester interface {
	Ingest(ctx context.Context, siteID string, req ExportTraceRequest) (IngestResponse, error)
}

func NewOTLPHandler(svc *IngestService) *OTLPHandler {
	return &OTLPHandler{svc: svc}
}

// ServeHTTP handles OTLP HTTP trace export requests.
// Endpoint: POST /v1/traces
// Content-Type: application/x-protobuf or application/json.
func (h *OTLPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// site_id is resolved by the API-key middleware from the validated key,
	// never trusted from a client-supplied header/query param.
	siteID := ingest.SiteIDFromContext(r.Context())
	if siteID == "" {
		http.Error(w, `{"error":"missing site_id"}`, http.StatusBadRequest)
		return
	}

	// Parse the media type so parameters (`application/x-protobuf;
	// charset=...`) are tolerated; anything outside the two OTLP wire formats
	// is a 415, not a silent fallthrough to the JSON decoder.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, fmt.Sprintf("unsupported content type %q", r.Header.Get("Content-Type")), http.StatusUnsupportedMediaType)
		return
	}
	var isProto bool
	switch mediaType {
	case "application/x-protobuf", "application/protobuf":
		isProto = true
	case "application/json":
	default:
		http.Error(w, fmt.Sprintf("unsupported content type %q", mediaType), http.StatusUnsupportedMediaType)
		return
	}

	h.handle(w, r, siteID, isProto)
}

func (h *OTLPHandler) handle(w http.ResponseWriter, r *http.Request, siteID string, isProto bool) {
	// OTLP HTTP exporters (incl. @vercel/otel and the OTel SDK with
	// OTEL_EXPORTER_OTLP_COMPRESSION=gzip) gzip the body and set
	// Content-Encoding: gzip. Decompress before parsing, or json.Unmarshal
	// chokes on the gzip magic byte and 400s every export.
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
			// The chain's neutron.BodyLimit rejected the RAW body; stay
			// consistent with its 413.
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

	var req ExportTraceRequest
	if isProto {
		decoded, derr := decodeProtoTraces(body)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}
		req = decoded
	} else if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if _, err := h.svc.Ingest(r.Context(), siteID, req); err != nil {
		// Audit F13 (logs precedent): OTLP exporters treat 500 as
		// non-retryable and drop the batch; 503 + Retry-After tells them to
		// re-send instead of silently losing every span.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}

	// Full success is the zero-value ExportTraceServiceResponse — tracing
	// ingest is all-or-nothing, so there is no partial-success shape here.
	writeExportResponse(w, isProto, &tracepb.ExportTraceServiceResponse{})
}

// writeExportResponse encodes an ExportTraceServiceResponse in the request's
// wire format: proto bytes for protobuf requests, protobuf-JSON for JSON
// requests. The JSON encoder renders int64 fields as strings — that IS the
// official protobuf-JSON mapping, not a bug to fix.
func writeExportResponse(w http.ResponseWriter, isProto bool, resp *tracepb.ExportTraceServiceResponse) {
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
