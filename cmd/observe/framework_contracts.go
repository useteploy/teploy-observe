package main

import (
	"context"
	"encoding/json"
	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"net/http"
	"strings"
)

// Pin-preserving application workaround for the framework's interface-output
// reflection. These routes return JSON, and their schema is intentionally open.
func observeOpenAPI(spec *neutron.OpenAPISpec) *neutron.OpenAPISpec {
	raw, _ := json.Marshal(spec)
	var fixed neutron.OpenAPISpec
	_ = json.Unmarshal(raw, &fixed)
	for _, route := range []struct{ path, method, status string }{
		{"/api/v1/logs/search", "get", "200"},
		{"/api/v1/dashboards/{dashboard_id}/panels/{panel_id}/execute", "post", "201"},
	} {
		if operation := fixed.Paths[route.path][route.method]; operation != nil {
			delete(operation.Responses, "204")
			operation.Responses[route.status] = neutron.OpenAPIResponse{Description: "JSON result", Content: map[string]neutron.OpenAPIMediaType{"application/json": {Schema: &neutron.OpenAPISchema{}}}}
		}
	}
	return &fixed
}

// The pin drops AppError extensions. Restore only the stable query refusal
// code already authenticated by its type URL, without buffering normal/SSE data.
func queryRefusalMetadata(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(&refusalWriter{ResponseWriter: w}, r) })
}

type refusalWriter struct {
	http.ResponseWriter
	status int
}

func (w *refusalWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *refusalWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *refusalWriter) Write(body []byte) (int, error) {
	original := len(body)
	if (w.status == 429 || w.status == 504) && strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		var problem map[string]any
		if json.Unmarshal(body, &problem) == nil {
			typ, _ := problem["type"].(string)
			prefix := "https://neutron.dev/errors/"
			if strings.HasPrefix(typ, prefix) {
				code := strings.TrimPrefix(typ, prefix)
				switch code {
				case queryguard.CodeConcurrencyGlobal, queryguard.CodeConcurrencySite, queryguard.CodeBudgetRows, queryguard.CodeBudgetTime:
					extensions, _ := problem["extensions"].(map[string]any)
					if extensions == nil {
						extensions = map[string]any{}
					}
					extensions["refusal_code"] = code
					problem["extensions"] = extensions
					if encoded, err := json.Marshal(problem); err == nil {
						body = append(encoded, '\n')
					}
				}
			}
		}
	}
	n, err := w.ResponseWriter.Write(body)
	if err == nil {
		return original, nil
	}
	return n, err
}

// Keep the started prefix aligned with registration order in the pinned runner.
// Stop-only resources are already acquired before app startup.
func observeLifecycleHook(h neutron.LifecycleHook) neutron.LifecycleHook {
	if h.OnStart == nil {
		h.OnStart = func(context.Context) error { return nil }
	}
	return h
}
