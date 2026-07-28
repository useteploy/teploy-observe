package metrics

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// OTLPHandler accepts OTLP metrics over HTTP. 1:1 mirror of
// tracing.OTLPHandler — same JSON-vs-protobuf split, same site_id
// header / query convention, same 10MB body cap.
type OTLPHandler struct {
	svc *Service
}

func NewOTLPHandler(svc *Service) *OTLPHandler {
	return &OTLPHandler{svc: svc}
}

// ServeHTTP handles OTLP HTTP metrics export requests.
// Endpoint: POST /v1/metrics
// Content-Type: application/x-protobuf or application/json.
func (h *OTLPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	contentType := r.Header.Get("Content-Type")
	// site_id is resolved by the API-key middleware from the validated key,
	// never trusted from a client-supplied header/query param.
	siteID := ingest.SiteIDFromContext(r.Context())
	if siteID == "" {
		http.Error(w, `{"error":"missing site_id"}`, http.StatusBadRequest)
		return
	}

	// Content-Type may carry parameters (`application/x-protobuf; charset=...`).
	switch {
	case strings.HasPrefix(contentType, "application/x-protobuf"),
		strings.HasPrefix(contentType, "application/protobuf"):
		h.handle(w, r, siteID, true)
	default:
		h.handle(w, r, siteID, false)
	}
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

	body, err := io.ReadAll(io.LimitReader(src, 10*1024*1024)) // 10MB max (decompressed)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
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

	result, err := h.svc.Ingest(r.Context(), siteID, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
