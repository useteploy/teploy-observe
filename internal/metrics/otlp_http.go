package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
// Content-Type: application/json (OTLP JSON) or application/x-protobuf (not supported).
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

	switch {
	case contentType == "application/json" || contentType == "":
		h.handleJSON(w, r, siteID)
	case contentType == "application/x-protobuf":
		http.Error(w, `{"error":"protobuf not supported, use JSON transport: set OTEL_EXPORTER_OTLP_PROTOCOL=http/json"}`, http.StatusUnsupportedMediaType)
	default:
		h.handleJSON(w, r, siteID)
	}
}

func (h *OTLPHandler) handleJSON(w http.ResponseWriter, r *http.Request, siteID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req ExportMetricsRequest
	if err := json.Unmarshal(body, &req); err != nil {
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
