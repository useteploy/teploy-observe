package ingest

import (
	"net/http"
	"strconv"
	"time"
)

// RetryAfterOnUnavailable sets Retry-After on any 503 leaving the wrapped
// handler (O01 ADR §5.2/§5.4 — the OTLP handlers' convention, applied to the
// typed events routes whose handlers cannot set headers directly). Durable
// refusals are retryable by construction; the header tells well-behaved
// producers how long to back off. Other statuses pass untouched.
func RetryAfterOnUnavailable(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&retryAfterWriter{ResponseWriter: w, after: d}, r)
		})
	}
}

// retryAfterWriter intercepts WriteHeader to add the header before the
// status line is committed. Unwrap keeps http.NewResponseController reach
// the real connection through it (same requirement as R16's wrappers).
type retryAfterWriter struct {
	http.ResponseWriter
	after time.Duration
	wrote bool
}

func (w *retryAfterWriter) WriteHeader(code int) {
	if !w.wrote && code == http.StatusServiceUnavailable {
		secs := int64(w.after / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *retryAfterWriter) Write(p []byte) (int, error) {
	w.wrote = true // an implicit 200 must not let a later WriteHeader add the header
	return w.ResponseWriter.Write(p)
}

func (w *retryAfterWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
