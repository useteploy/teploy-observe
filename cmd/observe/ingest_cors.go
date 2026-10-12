package main

import (
	"net/http"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

// ingestCORSOptions is the browser-ingest CORS posture: any origin, no
// credentials. The public ingest key is write-only and travels in X-API-Key,
// never in a cookie, so a wildcard here discloses nothing a reader could use.
var ingestCORSOptions = neutron.CORSOptions{
	AllowOrigins: []string{"*"},
	AllowMethods: []string{"POST", "OPTIONS"},
	AllowHeaders: []string{"Content-Type", "X-API-Key"},
	MaxAge:       86400,
}

// ingestPreflight answers OPTIONS /api/v1/{path...}. Go 1.22 method routing
// rejects OPTIONS before group middleware runs, so the CORS middleware never
// sees a browser preflight; this handler returns the same headers and 204.
func ingestPreflight(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, If-None-Match")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

// ingestMiddleware is the /api/v1 ingest group's chain. CORS is outermost so
// every response the chain can produce - 401/403/503 from key auth, 429 from
// the limiter, 413 from the body cap, the handler's own 4xx/5xx - already
// carries Access-Control-Allow-Origin; a browser cannot read a status whose
// response lacks it. Auth runs before the limiter so the limiter can key on
// site_id; the body cap rejects oversized payloads before they are decoded;
// RetryAfterOnUnavailable (O01 ADR §5.2) stamps Retry-After on durable-mode
// 503s. Deadline sits directly inside CORS so a Nucleus stall anywhere below
// it (key lookup, handler write) answers a CORS-readable 503 before the
// listener's WriteTimeout kills the connection.
func ingestMiddleware(apiKeyMW neutron.Middleware, rl *ingest.RateLimiter, deadline time.Duration) []neutron.Middleware {
	return []neutron.Middleware{
		neutron.CORS(ingestCORSOptions),
		ingest.Deadline(deadline, 5*time.Second),
		apiKeyMW,
		rl.Middleware,
		boundedRequestBody(2 << 20),
		ingest.RetryAfterOnUnavailable(5 * time.Second),
	}
}
