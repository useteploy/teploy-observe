package ingest

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Deadline bounds a public ingest request to d. Ingest handlers write
// synchronously to Nucleus (replays, OTLP, logs, LLM, check-ins) or look keys
// up there; when Nucleus stalls, a handler that waits past the listener's
// WriteTimeout gets its connection killed, and the tunnel in front reports a
// response with no CORS headers - the browser sees a CORS failure, not a
// retryable status.
//
// The handler runs with a context that expires at d, against a buffered
// writer. If it finishes in time its response is copied out unchanged. If not,
// the client gets 503 + Retry-After immediately and every later write by the
// handler is discarded (http.ErrHandlerTimeout), so nothing reaches the wire
// after the deadline. Mount it INSIDE the CORS middleware: the 503 is written
// to the outer writer, whose headers CORS has already set.
func Deadline(d, retryAfter time.Duration) func(http.Handler) http.Handler {
	secs := int64(retryAfter / time.Second)
	if secs < 1 {
		secs = 1
	}
	retry := strconv.FormatInt(secs, 10)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			dw := &deadlineWriter{h: make(http.Header), expired: ctx.Done()}
			done := make(chan struct{})
			panicked := make(chan any, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicked <- p
					}
				}()
				next.ServeHTTP(dw, r)
				close(done)
			}()

			select {
			case p := <-panicked:
				panic(p)
			case <-done:
				dw.mu.Lock()
				defer dw.mu.Unlock()
				dst := w.Header()
				for k, v := range dw.h {
					dst[k] = v
				}
				if dw.code == 0 {
					dw.code = http.StatusOK
				}
				w.WriteHeader(dw.code)
				_, _ = w.Write(dw.buf.Bytes())
			case <-ctx.Done():
				dw.mu.Lock()
				defer dw.mu.Unlock()
				dw.timedOut = true
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", retry)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"ok":false,"error":"ingest store unavailable, retry later"}`))
			}
		})
	}
}

// deadlineWriter buffers the handler's response until Deadline decides
// whether it ships. It deliberately does not expose Flush/Hijack or Unwrap:
// a deadline-bounded ingest response is small and must not reach the
// connection directly.
type deadlineWriter struct {
	mu       sync.Mutex
	h        http.Header
	buf      bytes.Buffer
	code     int
	timedOut bool
	// expired is the request context's Done channel: a write can race the
	// middleware's timeout critical section (the context is already dead
	// while timedOut has not been set yet), and such a write must report
	// ErrHandlerTimeout - its bytes would only ever be buffered and
	// discarded, and the caller deserves the documented signal, matching
	// net/http's own TimeoutHandler semantics.
	expired <-chan struct{}
}

func (w *deadlineWriter) Header() http.Header { return w.h }

func (w *deadlineWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut || w.code != 0 {
		return
	}
	w.code = code
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	select {
	case <-w.expired:
		w.timedOut = true
		return 0, http.ErrHandlerTimeout
	default:
	}
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(p)
}
