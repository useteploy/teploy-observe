package metrics

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

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// otlpMaxDecompressedBytes caps the DECOMPRESSED export body. The production
// chain (cmd/observe otlpChain) separately caps the raw bytes via
// neutron.BodyLimit; this one bounds gzip expansion.
const otlpMaxDecompressedBytes = 10 * 1024 * 1024

// OTLPHandler accepts OTLP metrics over HTTP. 1:1 mirror of
// tracing.OTLPHandler — same JSON-vs-protobuf split, same site_id context
// convention, same 10MB body cap, same official export-response encoding.
type OTLPHandler struct {
	svc metricIngester
}

// metricIngester is the transport-boundary seam the handler tests drive with a
// recording stub. *Service satisfies it; the service's own DTOs and storage
// stay untouched.
type metricIngester interface {
	Ingest(ctx context.Context, siteID string, req ExportMetricsRequest) (IngestResponse, error)
}

func NewOTLPHandler(svc *Service) *OTLPHandler {
	return &OTLPHandler{svc: svc}
}

// ServeHTTP handles OTLP HTTP metrics export requests.
// Endpoint: POST /v1/metrics
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

	// Parse the media type so parameters (`application/json; charset=...`)
	// are tolerated; anything outside the two OTLP wire formats is a 415,
	// not a silent fallthrough to the JSON decoder.
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
	// OTLP HTTP exporters gzip the body and set Content-Encoding: gzip.
	// Decompress before parsing or json.Unmarshal chokes on the gzip magic
	// byte and 400s every export. Mirrors the tracing OTLP handler.
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

	var req ExportMetricsRequest
	if isProto {
		decoded, derr := decodeProtoMetrics(body)
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
		// O12 cardinality refusal is PERMANENT for this batch — the
		// exporter must split it, retrying cannot help, so it gets a 413
		// with the remedy in the body. Everything else keeps the F13
		// posture below: OTLP exporters treat 500 as non-retryable and
		// drop the batch; 503 + Retry-After tells them to re-send
		// instead of silently losing every point.
		var tooBig *ErrTooManyPoints
		if errors.As(err, &tooBig) {
			http.Error(w, tooBig.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Retry-After", "5")
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}

	// Full success is the zero-value ExportMetricsServiceResponse — metrics
	// ingest is all-or-nothing, so there is no partial-success shape here.
	writeExportResponse(w, isProto, &metricspb.ExportMetricsServiceResponse{})
}

// writeExportResponse encodes an ExportMetricsServiceResponse in the
// request's wire format. The JSON encoder renders int64 fields as strings —
// that IS the official protobuf-JSON mapping, not a bug to fix.
func writeExportResponse(w http.ResponseWriter, isProto bool, resp *metricspb.ExportMetricsServiceResponse) {
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
