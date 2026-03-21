package ingest

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const (
	keyClientIP ctxKey = iota
	keyUserAgent
)

// RequestInfoMiddleware extracts client IP and User-Agent from the request
// and stores them in the context for downstream handlers.
func RequestInfoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := extractClientIP(r)
		ua := r.Header.Get("User-Agent")
		ctx := context.WithValue(r.Context(), keyClientIP, ip)
		ctx = context.WithValue(ctx, keyUserAgent, ua)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx > 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// ClientIPFromContext returns the client IP stored by RequestInfoMiddleware.
func ClientIPFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyClientIP).(string); ok {
		return v
	}
	return ""
}

// UserAgentFromContext returns the User-Agent stored by RequestInfoMiddleware.
func UserAgentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyUserAgent).(string); ok {
		return v
	}
	return ""
}
