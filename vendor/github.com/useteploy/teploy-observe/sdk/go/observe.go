// Package observe is the Go SDK for Observe — self-hosted analytics, errors,
// logs, and traces.
//
// Basic usage:
//
//	client, err := observe.New(observe.Options{
//	    Endpoint: "https://observe.example.com",
//	    APIKey:   os.Getenv("OBSERVE_API_KEY"),
//	    SiteID:   "default",
//	})
//	if err != nil { panic(err) }
//	defer client.Close()
//
//	if err := doWork(); err != nil {
//	    client.CaptureException(err, observe.WithRelease("v1.4.2"))
//	}
//	client.Info("request served", observe.F("user_id", userID), observe.F("duration_ms", elapsed))
package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// Options configures a Client.
type Options struct {
	// Endpoint is the Observe base URL, e.g. "https://observe.example.com".
	Endpoint string

	// APIKey identifies this application to Observe. Generate one in /settings.
	APIKey string

	// SiteID is the site identifier to scope events to. Defaults to "default".
	SiteID string

	// Release is an optional release tag applied to all events.
	Release string

	// Environment is an optional tag ("production", "staging", etc.).
	Environment string

	// ServiceName is the default service_name for logs. Defaults to the binary name.
	ServiceName string

	// HTTPClient lets callers supply a custom client (timeouts, proxies). Optional.
	HTTPClient *http.Client

	// LogBatchSize is the number of log entries buffered before a flush.
	// Default: 50.
	LogBatchSize int

	// LogFlushInterval is the cadence for flushing buffered logs.
	// Default: 2 seconds.
	LogFlushInterval time.Duration
}

// Client submits events, errors, logs, and traces to Observe.
type Client struct {
	opts   Options
	http   *http.Client
	mu     sync.Mutex
	logs   []LogEntry
	spans  []pendingSpan
	closed chan struct{}
	done   chan struct{}
}

// Field represents a single key/value attribute on a log entry.
type Field struct {
	Key   string
	Value any
}

// F is a convenience constructor for structured log fields.
func F(k string, v any) Field { return Field{Key: k, Value: v} }

// LogEntry is the wire format for a log line.
type LogEntry struct {
	SiteID      string         `json:"site_id"`
	Level       string         `json:"level"`
	Message     string         `json:"message"`
	ServiceName string         `json:"service_name,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
	SpanID      string         `json:"span_id,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

// ErrorPayload is the wire format for error events.
type ErrorPayload struct {
	SiteID      string       `json:"site_id"`
	ErrorType   string       `json:"error_type"`
	ErrorValue  string       `json:"error_value"`
	StackTrace  []StackFrame `json:"stack_trace,omitempty"`
	ReleaseTag  string       `json:"release_tag,omitempty"`
	Environment string       `json:"environment,omitempty"`
	Level       string       `json:"level,omitempty"`
}

// StackFrame is a single frame in an error's stack trace.
type StackFrame struct {
	Function string `json:"function"`
	Filename string `json:"filename"`
	Lineno   int    `json:"lineno"`
	InApp    bool   `json:"in_app"`
}

// ExceptionOption customizes a CaptureException call.
type ExceptionOption func(*ErrorPayload)

// WithRelease sets the release tag on a single error submission.
func WithRelease(r string) ExceptionOption { return func(p *ErrorPayload) { p.ReleaseTag = r } }

// WithLevel overrides the level (default: "error").
func WithLevel(l string) ExceptionOption { return func(p *ErrorPayload) { p.Level = l } }

// New constructs a Client and starts its background flush goroutine.
// Caller must call Close() to flush pending logs and stop the goroutine.
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("observe: Endpoint is required")
	}
	if opts.SiteID == "" {
		opts.SiteID = "default"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.LogBatchSize <= 0 {
		opts.LogBatchSize = 50
	}
	if opts.LogFlushInterval <= 0 {
		opts.LogFlushInterval = 2 * time.Second
	}
	c := &Client{
		opts:   opts,
		http:   opts.HTTPClient,
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	go c.loop()
	return c, nil
}

// Close flushes any buffered logs and stops the background goroutine.
// Safe to call multiple times.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
		close(c.closed)
	}
	<-c.done
	if err := c.flushSpans(context.Background()); err != nil {
		_ = c.flushLogs(context.Background())
		return err
	}
	return c.flushLogs(context.Background())
}

func (c *Client) loop() {
	defer close(c.done)
	t := time.NewTicker(c.opts.LogFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			_ = c.flushLogs(context.Background())
			_ = c.flushSpans(context.Background())
		}
	}
}

// CaptureException submits an error event with a stack trace snapshot.
func (c *Client) CaptureException(err error, opts ...ExceptionOption) error {
	if err == nil {
		return nil
	}
	payload := ErrorPayload{
		SiteID:      c.opts.SiteID,
		ErrorType:   errorType(err),
		ErrorValue:  err.Error(),
		ReleaseTag:  c.opts.Release,
		Environment: c.opts.Environment,
		Level:       "error",
		StackTrace:  captureStack(2),
	}
	for _, opt := range opts {
		opt(&payload)
	}
	return c.post(context.Background(), "/api/v1/errors", payload)
}

// Debug records a debug-level log line.
func (c *Client) Debug(msg string, fields ...Field) { c.log("debug", msg, fields) }

// Info records an info-level log line.
func (c *Client) Info(msg string, fields ...Field) { c.log("info", msg, fields) }

// Warn records a warn-level log line.
func (c *Client) Warn(msg string, fields ...Field) { c.log("warn", msg, fields) }

// Error records an error-level log line (does not count as an exception).
func (c *Client) Error(msg string, fields ...Field) { c.log("error", msg, fields) }

// Fatal records a fatal-level log line.
func (c *Client) Fatal(msg string, fields ...Field) { c.log("fatal", msg, fields) }

func (c *Client) log(level, msg string, fields []Field) {
	attrs := make(map[string]any, len(fields))
	for _, f := range fields {
		attrs[f.Key] = f.Value
	}
	entry := LogEntry{
		SiteID:      c.opts.SiteID,
		Level:       level,
		Message:     msg,
		ServiceName: c.opts.ServiceName,
		Attributes:  attrs,
	}
	c.mu.Lock()
	c.logs = append(c.logs, entry)
	full := len(c.logs) >= c.opts.LogBatchSize
	c.mu.Unlock()
	if full {
		go func() { _ = c.flushLogs(context.Background()) }()
	}
}

// Flush immediately sends any buffered logs.
func (c *Client) Flush(ctx context.Context) error { return c.flushLogs(ctx) }

func (c *Client) flushLogs(ctx context.Context) error {
	c.mu.Lock()
	if len(c.logs) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := c.logs
	c.logs = nil
	c.mu.Unlock()

	// The ingest endpoint accepts a single log per request today. Send sequentially.
	var firstErr error
	for _, entry := range batch {
		if err := c.post(ctx, "/api/v1/logs", entry); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	url := c.opts.Endpoint
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += path
	return c.postRaw(ctx, url, body, nil)
}

// postRaw is the underlying HTTP call, used by post() and the OTLP trace path.
func (c *Client) postRaw(ctx context.Context, url string, body any, extraHeaders map[string]string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("observe: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("observe: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.APIKey != "" {
		req.Header.Set("X-API-Key", c.opts.APIKey)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("observe: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("observe: %s returned %d", url, resp.StatusCode)
	}
	return nil
}

func errorType(err error) string {
	return fmt.Sprintf("%T", err)
}

func captureStack(skip int) []StackFrame {
	const maxFrames = 32
	pcs := make([]uintptr, maxFrames)
	n := runtime.Callers(skip+1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]StackFrame, 0, n)
	for {
		f, more := frames.Next()
		out = append(out, StackFrame{
			Function: f.Function,
			Filename: f.File,
			Lineno:   f.Line,
			InApp:    isInApp(f.File),
		})
		if !more {
			break
		}
	}
	return out
}

func isInApp(file string) bool {
	// Standard library and GOROOT paths are not "in app".
	// Consumers usually want to highlight their own code.
	for _, prefix := range []string{"runtime/", "/usr/local/go/", "golang.org/"} {
		if len(file) >= len(prefix) && file[:len(prefix)] == prefix {
			return false
		}
	}
	return true
}
