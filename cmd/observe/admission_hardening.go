package main

import (
	"bytes"
	"io"
	"mime"
	"net/http"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

// Buffer before the typed framework binder so chunked overflow has the same
// 413 response as known-length overflow and multipart never spills to disk.
func boundedRequestBody(limit int64) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
			if err != nil {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			next.ServeHTTP(w, r)
		})
	}
}

func loginBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			writeJSONError(w, http.StatusUnsupportedMediaType, "login requires application/json")
			return
		}
		boundedRequestBody(8<<10)(next).ServeHTTP(w, r)
	})
}

func queueError(q *ingest.DiskQueue) error {
	if q == nil {
		return nil
	}
	return q.LastError()
}

// Readiness aggregates independent signal failures; an event queue failure
// must never hide the error queue or make the process ready.
func readinessFailures(eventsMemory, errorsMemory bool, eventWorker, errorWorker, eventWAL, errorWAL error, errorFlush bool) []string {
	var failures []string
	if eventsMemory {
		failures = append(failures, "memory-only")
	}
	if errorsMemory {
		failures = append(failures, "errors-memory-only")
	}
	if eventWorker != nil {
		failures = append(failures, "flush-worker-failed")
	}
	if errorWorker != nil {
		failures = append(failures, "error-worker-failed")
	}
	if eventWAL != nil {
		failures = append(failures, "wal-degraded")
	}
	if errorWAL != nil {
		failures = append(failures, "error-wal-degraded")
	}
	if errorFlush {
		failures = append(failures, "error-flush-failing")
	}
	return failures
}
