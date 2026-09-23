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
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
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

	// MaxSendAttempts bounds the retry budget per queued chunk before it
	// is dropped and counted as a loss (O11). The first retry after a
	// retryable failure (429/5xx/network) fires immediately; further
	// consecutive failures back off exponentially from RetryBackoff.
	// Default: 6.
	MaxSendAttempts int

	// RetryBackoff is the base delay for the exponential send backoff;
	// doubles per consecutive failure up to 30 s. Default: 1 s.
	RetryBackoff time.Duration

	// OnError, when set, receives transport/admission failures (dropped
	// oversized entries, failed background flushes). Never called from
	// inside the client's locks; must not call back into the Client.
	OnError func(error)
}

// Client submits events, errors, logs, traces, and metrics to Observe.
type Client struct {
	opts Options
	http *http.Client
	mu   sync.Mutex
	// logs holds entries serialized AT ADMISSION (AUD-036, round 2): the
	// old code stored the caller's LogEntry with its live map attributes,
	// so mutation after log() changed the eventual body and one
	// unmarshalable attribute poisoned the whole detached batch.
	logs    []json.RawMessage
	logsN   int
	spans   []pendingSpan
	metrics *metricsBuf
	closed  chan struct{}
	done    chan struct{}
	// flushWake coalesces size-triggered log flushes onto the owned worker
	// (audit F35: they used to run in untracked goroutines Close never
	// waited for).
	flushWake chan struct{}
	closing   bool
	// pendingLogBytes bounds total queued log bytes (AUD-038, round 2) —
	// the count cap alone let a slow endpoint grow memory without limit.
	pendingLogBytes int64
	// spansN/pendingSpanBytes bound the queued span batches (TO-038).
	spansN           int
	pendingSpanBytes int64
	// pendingMetrics holds frozen metric envelopes whose POST failed
	// (TO-039) — retried before any new snapshot is taken.
	pendingMetrics []*frozenMetrics
	// metricFlushMu serializes metric exports (TO-039).
	metricFlushMu sync.Mutex
	// logFlushMu serializes public Flush and worker flushes so one owner
	// sends at a time and a failed chunk leaves its queue prefix intact
	// (AUD-038).
	logFlushMu sync.Mutex
	// closeOnce + closeErr make Close safe under concurrent callers (audit
	// F35: the select/default + close pair let two closers both take the
	// default path and the second close panicked).
	closeOnce sync.Once
	closeErr  error
	// O11 retry/loss state. Per-signal attempt counters and backoff gates
	// (a retryable failure holds that signal's flush until notBefore);
	// statsMtx guards the Stats snapshot counters.
	statsMtx         sync.Mutex
	stats            Stats
	logAttempts      int
	spanAttempts     int
	metricAttempts   int
	logNotBefore     time.Time
	spanNotBefore    time.Time
	metricNotBefore  time.Time
	maxSendAttempts  int
	retryBackoffBase time.Duration
}

// Stats is the O11 diagnostics snapshot: delivery and loss counters for
// everything this Client admitted. Dropped is keyed by reason:
//
//	logs_queue_full, logs_entry_oversize, logs_retry_exhausted,
//	logs_non_retryable, logs_server_rejected, logs_shutdown_unflushed,
//	spans_queue_full, spans_non_retryable, spans_retry_exhausted,
//	spans_shutdown_unflushed, metrics_non_retryable,
//	metrics_retry_exhausted, metric_series_budget, gauge_point_budget.
//
// Accounting: Delivered + sum(Dropped) + Queued + shutdown_unflushed
// (a point-in-time count taken when Shutdown gave up with work queued)
// covers every admitted record. Queues are in-memory; kill -9 loses the
// queue and these counters with the process.
type Stats struct {
	DeliveredLogs         int64          `json:"delivered_logs"`
	DeliveredSpans        int64          `json:"delivered_spans"`
	DeliveredMetricPoints int64          `json:"delivered_metric_points"`
	Retries               int64          `json:"retries"`
	Dropped               map[string]int64 `json:"dropped"`
	QueuedLogs            int            `json:"queued_logs"`
	QueuedSpans           int            `json:"queued_spans"`
}

// Stats returns a deep copy of the current diagnostics counters (O11).
func (c *Client) Stats() Stats {
	c.statsMtx.Lock()
	defer c.statsMtx.Unlock()
	out := c.stats
	out.Dropped = make(map[string]int64, len(c.stats.Dropped))
	for k, v := range c.stats.Dropped {
		out.Dropped[k] = v
	}
	c.mu.Lock()
	out.QueuedLogs = c.logsN
	out.QueuedSpans = c.spansN
	c.mu.Unlock()
	return out
}

// countLoss increments a visible loss counter AND reports through OnError
// (O11: a loss is never silent even when no hook is configured, because
// Stats() always exposes it).
func (c *Client) countLoss(reason string, n int64, detail string) {
	c.statsMtx.Lock()
	if c.stats.Dropped == nil {
		c.stats.Dropped = map[string]int64{}
	}
	c.stats.Dropped[reason] += n
	c.statsMtx.Unlock()
	msg := fmt.Sprintf("observe: lost %d record(s) (%s)", n, reason)
	if detail != "" {
		msg += " — " + detail
	}
	c.reportError(errors.New(msg))
}

// countDelivered books successful deliveries per signal.
func (c *Client) countDelivered(fn func(*Stats)) {
	c.statsMtx.Lock()
	fn(&c.stats)
	c.statsMtx.Unlock()
}

// httpError carries the HTTP status of a refused send so callers can
// classify retryable (429/5xx) from permanent (other 4xx) failures.
type httpError struct {
	status int
	url    string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("observe: %s returned %d", e.url, e.status)
}

// retryableSendErr classifies a send failure: HTTP 429 and 5xx (and any
// non-HTTP transport error — network, timeout, deadline) are retryable;
// every other 4xx is permanent for that payload (O11).
func retryableSendErr(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status == 429 || he.status >= 500
	}
	return true
}

// backoffFor returns the exponential send backoff after attempt n
// (1-based), capped at 30 s.
func (c *Client) backoffFor(attempt int) time.Duration {
	d := c.retryBackoffBase
	for i := 1; i < attempt && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// backoffGated reports whether a signal's flush is currently held off by
// its retry backoff. The FIRST retry is always allowed immediately (a
// transient blip recovers with no delay, and signal-path Flush callers
// get a real attempt); the gate engages from the second consecutive
// failure onward (O11).
func backoffGated(attempts int, notBefore time.Time) bool {
	return attempts >= 2 && time.Now().Before(notBefore)
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
	TraceID     string       `json:"trace_id,omitempty"`
	SpanID      string       `json:"span_id,omitempty"`
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

// WithSpan attaches a span's trace context to the error so the trace detail
// view correlates it exactly instead of by timestamp overlap. Typical use:
//
//	client.CaptureException(err, observe.WithSpan(observe.SpanFromContext(ctx)))
//
// A nil span is a no-op, so callers don't need to guard the lookup.
func WithSpan(s *Span) ExceptionOption {
	return func(p *ErrorPayload) {
		if s == nil {
			return
		}
		p.TraceID = s.TraceID()
		p.SpanID = s.SpanID()
	}
}

// New constructs a Client and starts its background flush goroutine.
// Caller must call Close() to flush pending logs and stop the goroutine.
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("observe: Endpoint is required")
	}
	if opts.SiteID == "" {
		opts.SiteID = "default"
	}
	base := opts.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Second}
	}
	// TO-037: telemetry carries the X-API-Key credential, and Go's default
	// redirect follower forwards custom headers across origins — a
	// redirecting (or compromised) endpoint would receive the key. The
	// client is COPIED (never mutate a caller-owned client) and refuses
	// redirects; postRaw accepts only 2xx.
	owned := *base
	owned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if opts.LogBatchSize <= 0 {
		opts.LogBatchSize = 50
	}
	if opts.LogFlushInterval <= 0 {
		opts.LogFlushInterval = 2 * time.Second
	}
	if opts.MaxSendAttempts <= 0 {
		opts.MaxSendAttempts = 6
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = time.Second
	}
	c := &Client{
		opts:             opts,
		http:             &owned,
		closed:           make(chan struct{}),
		done:             make(chan struct{}),
		flushWake:        make(chan struct{}, 1),
		stats:            Stats{Dropped: map[string]int64{}},
		maxSendAttempts:  opts.MaxSendAttempts,
		retryBackoffBase: opts.RetryBackoff,
	}
	go c.loop()
	return c, nil
}

// Close flushes any buffered telemetry and stops the background goroutine,
// with a 10 s shutdown budget. Safe to call multiple times and from
// multiple goroutines: every caller waits for the same single finalization
// and receives the same result (audit F35 — the old select/default +
// close raced a double close panic, and untracked flush goroutines could
// outlive Close's return).
//
// For a caller-controlled shutdown deadline use Shutdown (O11). On an
// expired deadline Shutdown reports still-queued records as counted
// losses (stats key "<signal>_shutdown_unflushed") instead of swallowing
// them.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Shutdown(ctx)
}

// Shutdown is Close with the caller's deadline (O11 shutdown semantics:
// flush(timeout)). The context bounds the final drain; when it expires
// with work still queued, the leftovers are counted per signal as
// "<signal>_shutdown_unflushed" losses and reported through OnError, and
// the context error is returned joined with any flush failure.
func (c *Client) Shutdown(ctx context.Context) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true // new admissions rejected from here on
		c.mu.Unlock()
		close(c.closed)
		<-c.done // includes ALL owned flush work, not only the ticker
		c.closeErr = errors.Join(
			c.flushSpans(ctx),
			c.FlushMetrics(ctx),
			c.flushLogs(ctx),
		)
		if ctx.Err() != nil {
			c.closeErr = errors.Join(c.closeErr, ctx.Err())
		}
		// O11: honest shutdown accounting. Anything still queued when the
		// deadline hit is lost with the process — count and report it
		// rather than returning a clean-looking result.
		c.mu.Lock()
		leftLogs, leftSpans := c.logsN, c.spansN
		c.mu.Unlock()
		if leftLogs > 0 {
			c.countLoss("logs_shutdown_unflushed", int64(leftLogs), "shutdown deadline reached with logs queued")
		}
		if leftSpans > 0 {
			c.countLoss("spans_shutdown_unflushed", int64(leftSpans), "shutdown deadline reached with spans queued")
		}
		if s := c.lossSummary(); s != "" {
			c.reportError(errors.New(s))
		}
	})
	return c.closeErr
}

// lossSummary renders a one-line shutdown summary of all counted losses,
// or "" when nothing was lost.
func (c *Client) lossSummary() string {
	c.statsMtx.Lock()
	defer c.statsMtx.Unlock()
	if len(c.stats.Dropped) == 0 {
		return ""
	}
	keys := make([]string, 0, len(c.stats.Dropped))
	total := int64(0)
	for k, v := range c.stats.Dropped {
		keys = append(keys, fmt.Sprintf("%s=%d", k, v))
		total += v
	}
	sort.Strings(keys)
	return fmt.Sprintf("observe: shutdown loss summary — %d record(s) lost: %s", total, strings.Join(keys, ", "))
}

func (c *Client) loop() {
	defer close(c.done)
	t := time.NewTicker(c.opts.LogFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-c.flushWake:
			// One bounded attempt per wakeup; a failing flush is retried
			// by the ticker rather than a tight loop. AUD-038 (round 2):
			// failures are REPORTED through the hook, not ignored — a
			// silently dropped chunk used to look identical to success at
			// Close time. TO-038: span/metric export failures are
			// reported the same way.
			if err := c.flushLogs(context.Background()); err != nil {
				c.reportError(err)
			}
			if err := c.flushSpans(context.Background()); err != nil {
				c.reportError(err)
			}
			if err := c.FlushMetrics(context.Background()); err != nil {
				c.reportError(err)
			}
		case <-t.C:
			if err := c.flushLogs(context.Background()); err != nil {
				c.reportError(err)
			}
			if err := c.flushSpans(context.Background()); err != nil {
				c.reportError(err)
			}
			if err := c.FlushMetrics(context.Background()); err != nil {
				c.reportError(err)
			}
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
	c.admitLog(LogEntry{
		SiteID:      c.opts.SiteID,
		Level:       level,
		Message:     msg,
		ServiceName: c.opts.ServiceName,
		Attributes:  attrs,
	})
}

// admitLog serializes one entry NOW, owns the bytes, and queues it under
// the byte budget (AUD-036/AUD-038, round 2). json.Marshal used to run
// after the batch was detached, so a caller-mutated map changed the body
// (or raced it), one unsupported value failed the entire batch, and the
// queue had no bound at all.
func (c *Client) admitLog(entry LogEntry) {
	raw, err := json.Marshal(entry)
	if err != nil || len(raw) > maxLogEntryBytes {
		if err == nil {
			err = fmt.Errorf("log entry exceeds %d bytes", maxLogEntryBytes)
		}
		c.countLoss("logs_entry_oversize", 1, err.Error())
		return
	}
	c.mu.Lock()
	if c.closing {
		// Post-close admission used to enqueue work with no guaranteed
		// consumer (audit F35) — refuse instead.
		c.mu.Unlock()
		return
	}
	// AUD-038: bounded admission — a stalled endpoint must not turn the
	// queue into unbounded memory. Overflow drops the NEW entry (O11
	// documented drop-newest policy) and says so.
	if c.pendingLogBytes+int64(len(raw)) > maxPendingLogBytes {
		c.mu.Unlock()
		c.countLoss("logs_queue_full", 1, "queue byte limit reached — newest entry dropped")
		return
	}
	c.logs = append(c.logs, raw)
	c.logsN++
	c.pendingLogBytes += int64(len(raw))
	full := c.logsN >= c.opts.LogBatchSize
	c.mu.Unlock()
	if full {
		// Wake the owned worker; never spawn an untracked goroutine.
		select {
		case c.flushWake <- struct{}{}:
		default:
		}
	}
}

// reportError routes a failure to the configured hook without panicking on
// a nil hook or a hook that itself fails.
func (c *Client) reportError(err error) {
	if c.opts.OnError == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		c.opts.OnError(err)
	}()
}

// Flush immediately sends any buffered logs.
func (c *Client) Flush(ctx context.Context) error { return c.flushLogs(ctx) }

// serverLogBatchCap mirrors the server's /logs/batch limit.
const serverLogBatchCap = 200

// maxLogEntryBytes caps one serialized log entry at admission.
const maxLogEntryBytes = 64 << 10

// maxPendingLogBytes bounds total queued log bytes (AUD-038).
const maxPendingLogBytes = 8 << 20

// maxLogRequestBytes packs request bodies by encoded bytes (AUD-026,
// round 2): 200 near-64-KiB entries exceeded the route's 2 MiB cap and the
// whole batch was rejected. 1 MiB leaves envelope headroom.
const maxLogRequestBytes = 1 << 20

// flushLogs sends buffered entries through the batch endpoint as bounded
// chunks (audit F35: one HTTP request per log amplified shutdown latency).
//
// AUD-038 (round 2): the queue prefix stays intact until its request
// SUCCEEDS — the old code detached everything up front and dropped failed
// chunks on the floor, so a 503 lost the batch while a later Close saw an
// empty queue and reported success. Serialized by logFlushMu so public
// Flush and the worker cannot double-send.
//
// O11 retry/loss contract: retryable failures (429/5xx/network, see
// retryableSendErr) consume the bounded attempt budget with exponential
// backoff (logNotBefore gate); when the budget is exhausted the chunk is
// dropped and counted (logs_retry_exhausted). A NON-retryable 4xx can
// never succeed as-shaped and is dropped immediately with a counted loss
// (logs_non_retryable) — the previous behavior blocked the queue head
// forever while admission drops silently piled up behind it. A 200 whose
// body reports per-entry rejections counts them as logs_server_rejected
// (the server already skipped those rows; resending would only duplicate
// the accepted neighbors).
func (c *Client) flushLogs(ctx context.Context) error {
	c.logFlushMu.Lock()
	defer c.logFlushMu.Unlock()

	c.mu.Lock()
	remaining := c.logsN // fixed watermark: continuous producers cannot extend this flush forever
	c.mu.Unlock()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if backoffGated(c.logAttempts, c.logNotBefore) {
			return nil // mid-backoff; the ticker or a later Flush retries
		}
		c.mu.Lock()
		n, size := 0, len(`{"logs":[]}`)
		for n < remaining && n < c.logsN && n < serverLogBatchCap {
			extra := len(c.logs[n])
			if n > 0 {
				extra++
			}
			if size+extra > maxLogRequestBytes && n > 0 {
				break
			}
			size += extra
			n++
		}
		if n == 0 {
			// Belt-and-braces: admission caps entries at 64 KiB, so a
			// chunk that cannot fit even alone is unreachable — if it
			// ever happens, drop the head entry loudly instead of
			// erroring forever on a queue that can never drain.
			oversize := c.logs[0]
			c.logs = c.logs[1:]
			c.logsN--
			c.pendingLogBytes -= int64(len(oversize))
			if c.pendingLogBytes < 0 {
				c.pendingLogBytes = 0
			}
			remaining--
			c.mu.Unlock()
			c.countLoss("logs_entry_oversize", 1, "entry exceeds the request byte budget")
			continue
		}
		batch := c.logs[:n]
		c.mu.Unlock()
		var ack struct {
			Accepted int `json:"accepted"`
			Rejected int `json:"rejected"`
		}
		err := c.postJSON(ctx, "/api/v1/logs/batch", logBatchWire{Logs: batch}, &ack)
		if err != nil {
			if !retryableSendErr(err) {
				c.dropLogPrefix(n, "logs_non_retryable", err.Error())
				remaining -= n
				c.logAttempts = 0
				continue
			}
			c.logAttempts++
			if c.logAttempts >= c.maxSendAttempts {
				c.dropLogPrefix(n, "logs_retry_exhausted",
					fmt.Sprintf("%d attempts, last error: %v", c.maxSendAttempts, err))
				remaining -= n
				c.logAttempts = 0
				continue
			}
			c.countDelivered(func(s *Stats) { s.Retries++ })
			c.logNotBefore = time.Now().Add(c.backoffFor(c.logAttempts))
			return err // the same queue prefix stays queued for the next attempt
		}
		if ack.Accepted == 0 && ack.Rejected == 0 {
			// Older servers answered 200 without a body — count the chunk.
			ack.Accepted = n
		}
		if ack.Rejected > 0 {
			c.countLoss("logs_server_rejected", int64(ack.Rejected),
				fmt.Sprintf("server accepted %d of %d", ack.Accepted, n))
		}
		delivered := int64(ack.Accepted)
		c.countDelivered(func(s *Stats) { s.DeliveredLogs += delivered })
		c.mu.Lock()
		var sent int64
		for _, raw := range c.logs[:n] {
			sent += int64(len(raw))
		}
		c.pendingLogBytes -= sent
		if c.pendingLogBytes < 0 {
			c.pendingLogBytes = 0
		}
		c.logs = c.logs[n:]
		c.logsN -= n
		remaining -= n
		c.mu.Unlock()
		c.logAttempts = 0
	}
	return nil
}

// dropLogPrefix removes n entries from the queue head, releasing their
// byte reservation, and books them as a counted loss (O11).
func (c *Client) dropLogPrefix(n int, reason, detail string) {
	c.mu.Lock()
	var dropped int64
	for _, raw := range c.logs[:n] {
		dropped += int64(len(raw))
	}
	c.pendingLogBytes -= dropped
	if c.pendingLogBytes < 0 {
		c.pendingLogBytes = 0
	}
	c.logs = c.logs[n:]
	c.logsN -= n
	c.mu.Unlock()
	c.countLoss(reason, int64(n), detail)
}

// logBatchWire is the /logs/batch request shape. Logs holds pre-encoded
// entries (AUD-036) — the wire JSON per element is unchanged.
type logBatchWire struct {
	Logs []json.RawMessage `json:"logs"`
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	url := c.opts.Endpoint
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += path
	return c.postRaw(ctx, url, body, nil)
}

// postJSON sends body and decodes the 2xx response into out (O11: the
// logs batch endpoint reports per-entry outcomes in its response body —
// {accepted, rejected} — which the caller books as loss counters).
func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	url := c.opts.Endpoint
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += path
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("observe: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("observe: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.APIKey != "" {
		req.Header.Set("X-API-Key", c.opts.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("observe: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, url: url}
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return fmt.Errorf("observe: read ack: %w", err)
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("observe: decode ack: %w", err)
			}
		}
	}
	return nil
}

// postRaw is the underlying HTTP call, used by post() and the OTLP trace path.
// AUD-039 (round 2): every request carries its own deadline even when the
// caller supplied a custom HTTPClient with no Timeout — Close used to be
// able to wait forever on a stuck worker request that ctx.Background()
// would never cancel.
func (c *Client) postRaw(ctx context.Context, url string, body any, extraHeaders map[string]string) error {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("observe: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(raw))
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
	// TO-037: only 2xx is success. Redirects are already refused by the
	// owned client (they would forward the credential); a 3xx reaching
	// this check means the caller's transport forced one through. The
	// typed status lets flush callers classify retryable vs permanent
	// (O11, see retryableSendErr).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, url: url}
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
