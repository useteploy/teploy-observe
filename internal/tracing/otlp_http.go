package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OTLPHandler returns an http.HandlerFunc that accepts OTLP traces via HTTP.
// Supports both JSON and protobuf content types.
// This serves as the /v1/traces endpoint compatible with OTLP HTTP exporters.
// For gRPC exporters, use an OTLP-to-HTTP gateway or configure the exporter
// to use HTTP transport (supported by all major OTLP exporters).
type OTLPHandler struct {
	svc *IngestService
}

func NewOTLPHandler(svc *IngestService) *OTLPHandler {
	return &OTLPHandler{svc: svc}
}

// ServeHTTP handles OTLP HTTP trace export requests.
// Endpoint: POST /v1/traces
// Content-Type: application/json (OTLP JSON) or application/x-protobuf (not yet supported)
func (h *OTLPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	contentType := r.Header.Get("Content-Type")
	siteID := r.Header.Get("X-Observe-Site")
	if siteID == "" {
		siteID = r.URL.Query().Get("site_id")
	}
	if siteID == "" {
		siteID = "default"
	}

	switch {
	case contentType == "application/json" || contentType == "":
		h.handleJSON(w, r, siteID)
	case contentType == "application/x-protobuf":
		// Protobuf support would require importing the OTLP proto definitions.
		// For now, return a helpful error directing users to JSON transport.
		http.Error(w, `{"error":"protobuf not supported, use JSON transport: set OTEL_EXPORTER_OTLP_PROTOCOL=http/json"}`, http.StatusUnsupportedMediaType)
	default:
		h.handleJSON(w, r, siteID)
	}
}

func (h *OTLPHandler) handleJSON(w http.ResponseWriter, r *http.Request, siteID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // 10MB max
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req ExportTraceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	result, err := h.svc.Ingest(context.Background(), siteID, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
