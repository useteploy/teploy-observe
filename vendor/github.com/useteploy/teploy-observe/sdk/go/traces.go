package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// span context key for in-process propagation.
type spanKey struct{}

// Span is an active OTLP span. End() finalizes it and queues it for export.
type Span struct {
	traceID    string
	spanID     string
	parentID   string
	name       string
	kind       int
	start      time.Time
	end        time.Time
	attributes []spanAttr
	statusCode int
	statusMsg  string
	client     *Client
	ended      bool
	mu         sync.Mutex
}

type spanAttr struct {
	key   string
	value any
}

// otlpResource is a minimal Resource representation kept inside the SDK.
type otlpResource struct {
	ServiceName string
	Environment string
}

// pendingSpan is the wire form held in the buffer until flush.
type pendingSpan struct {
	traceID    string
	spanID     string
	parentID   string
	name       string
	kind       int
	startNano  int64
	endNano    int64
	attributes []spanAttr
	statusCode int
	statusMsg  string
}

// StartSpan begins a new span. Returns a context carrying the span and the
// span itself; callers MUST call (*Span).End to record the span.
//
// If ctx already carries a span, the new span is parented to it.
func (c *Client) StartSpan(ctx context.Context, name string) (context.Context, *Span) {
	parent, _ := ctx.Value(spanKey{}).(*Span)
	traceID := ""
	parentID := ""
	if parent != nil {
		traceID = parent.traceID
		parentID = parent.spanID
	} else {
		traceID = newTraceID()
	}
	s := &Span{
		traceID:  traceID,
		spanID:   newSpanID(),
		parentID: parentID,
		name:     name,
		kind:     2, // server kind by default; callers can override
		start:    time.Now(),
		client:   c,
	}
	return context.WithValue(ctx, spanKey{}, s), s
}

// SpanFromContext returns the active span carried in ctx, or nil.
func SpanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

// SetAttribute attaches a key/value attribute to the span. The value is
// NORMALIZED AND SNAPSHOTTED here (TO-054): unsupported types are reported
// through OnError and dropped instead of silently encoding as an empty
// string, and the caller's value can no longer mutate the eventual export.
func (s *Span) SetAttribute(key string, value any) {
	if s == nil {
		return
	}
	normalized, err := normalizeSpanScalar(value)
	if err != nil {
		if s.client != nil {
			s.client.reportError(fmt.Errorf("observe: span attribute %q: %w", key, err))
		}
		return
	}
	s.mu.Lock()
	s.attributes = append(s.attributes, spanAttr{key, normalized})
	s.mu.Unlock()
}

// normalizeSpanScalar converts the supported scalar types to the OTLP
// wire types and rejects everything else with a diagnostic (TO-054). The
// old default branch returned stringValue:"" while its comment promised a
// string representation — common Go values (float32, small ints, unsigned
// ints, slices) were silently lost.
func normalizeSpanScalar(v any) (any, error) {
	switch n := v.(type) {
	case string, bool, int, int64:
		return n, nil
	case int8:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case uint:
		if uint64(n) > math.MaxInt64 {
			return nil, errors.New("integer exceeds int64")
		}
		return int64(n), nil
	case uint8:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		if n > math.MaxInt64 {
			return nil, errors.New("integer exceeds int64")
		}
		return int64(n), nil
	case float32:
		return normalizeSpanScalar(float64(n))
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("non-finite value")
		}
		return n, nil
	default:
		return nil, fmt.Errorf("unsupported attribute type %T", v)
	}
}

// SetKind overrides the span kind. 1=internal, 2=server, 3=client, 4=producer, 5=consumer.
func (s *Span) SetKind(kind int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.kind = kind
	s.mu.Unlock()
}

// RecordError marks the span as failed and attaches the error message.
func (s *Span) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.statusCode = 2
	s.statusMsg = err.Error()
	s.mu.Unlock()
}

// SetStatus explicitly sets the span status (0=unset, 1=ok, 2=error).
func (s *Span) SetStatus(code int, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.statusCode = code
	s.statusMsg = msg
	s.mu.Unlock()
}

// TraceID returns the W3C-style trace ID (32 hex chars).
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return s.traceID
}

// SpanID returns the W3C-style span ID (16 hex chars).
func (s *Span) SpanID() string {
	if s == nil {
		return ""
	}
	return s.spanID
}

// End finalizes the span and queues it for export. Idempotent.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.end = time.Now()
	ps := pendingSpan{
		traceID:    s.traceID,
		spanID:     s.spanID,
		parentID:   s.parentID,
		name:       s.name,
		kind:       s.kind,
		startNano:  s.start.UnixNano(),
		endNano:    s.end.UnixNano(),
		attributes: append([]spanAttr(nil), s.attributes...),
		statusCode: s.statusCode,
		statusMsg:  s.statusMsg,
	}
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return
	}
	c.queueSpan(ps)
}

// queueSpan admits one finalized span onto the owned flush worker
// (TO-038): post-Close admissions are refused and reported, and a full
// buffer WAKES the worker instead of spawning an untracked goroutine Close
// could return past. The serialized span bytes count against the queue
// budget so a stalled endpoint cannot grow memory without limit.
func (c *Client) queueSpan(ps pendingSpan) {
	cost := pendingSpanBytes(ps)
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		c.reportError(errors.New("observe: span rejected after close began"))
		return
	}
	if c.pendingSpanBytes+cost > maxPendingSpanBytes || c.spansN >= maxPendingSpans {
		c.mu.Unlock()
		c.reportError(errors.New("observe: span queue limit reached — span dropped"))
		return
	}
	c.spans = append(c.spans, ps)
	c.spansN++
	c.pendingSpanBytes += cost
	full := c.spansN >= c.opts.LogBatchSize
	c.mu.Unlock()
	if full {
		select {
		case c.flushWake <- struct{}{}:
		default:
		}
	}
}

// maxPendingSpanBytes bounds total queued span bytes (TO-038).
const maxPendingSpanBytes = 8 << 20

// maxPendingSpans bounds the queued span count.
const maxPendingSpans = 10000

// pendingSpanBytes is the serialized-size estimate for one queued span.
func pendingSpanBytes(ps pendingSpan) int64 {
	n := 256
	for _, a := range ps.attributes {
		n += len(a.key) + 64
	}
	return int64(n)
}

// spanFlushMu serializes span flushes (TO-038): the public Close path, the
// owned worker, and any final flush take turns; a failed export leaves its
// batch queued for the next attempt instead of being dropped.

func (c *Client) flushSpans(ctx context.Context) error {
	c.logFlushMu.Lock()
	defer c.logFlushMu.Unlock()

	c.mu.Lock()
	if len(c.spans) == 0 {
		c.mu.Unlock()
		return nil
	}
	batch := c.spans
	var batchBytes int64
	for _, ps := range batch {
		batchBytes += pendingSpanBytes(ps)
	}
	c.spans = nil
	c.spansN = 0
	c.pendingSpanBytes -= batchBytes
	if c.pendingSpanBytes < 0 {
		c.pendingSpanBytes = 0
	}
	c.mu.Unlock()

	// Build a single OTLP ExportTraceRequest grouping all spans under one
	// resource (we only have one ServiceName per Client). Use the standard
	// OTLP endpoint /v1/traces so SDK consumers can also point this Client
	// at a non-Observe OTLP collector if needed.
	otlp := buildOTLPRequest(c.opts.ServiceName, c.opts.Environment, batch)
	if err := c.postOTLP(ctx, otlp); err != nil {
		// TO-038: a failed export requeues the WHOLE detached batch —
		// spans are admitted with at-least-once delivery semantics, and
		// a transient endpoint failure must not silently discard them.
		c.mu.Lock()
		c.spans = append(batch, c.spans...)
		c.spansN += len(batch)
		c.pendingSpanBytes += batchBytes
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Client) postOTLP(ctx context.Context, body any) error {
	url := c.opts.Endpoint
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += "/v1/traces"
	return c.postRaw(ctx, url, body, map[string]string{"X-Observe-Site": c.opts.SiteID})
}

// buildOTLPRequest constructs the OTLP JSON wire format the Observe ingest
// endpoint accepts. Kept as anonymous maps so the SDK doesn't depend on the
// server's tracing package.
func buildOTLPRequest(serviceName, environment string, spans []pendingSpan) map[string]any {
	resourceAttrs := []map[string]any{}
	if serviceName != "" {
		resourceAttrs = append(resourceAttrs, map[string]any{
			"key":   "service.name",
			"value": map[string]any{"stringValue": serviceName},
		})
	}
	if environment != "" {
		resourceAttrs = append(resourceAttrs, map[string]any{
			"key":   "deployment.environment",
			"value": map[string]any{"stringValue": environment},
		})
	}

	otlpSpans := make([]map[string]any, 0, len(spans))
	for _, s := range spans {
		attrs := make([]map[string]any, 0, len(s.attributes))
		for _, a := range s.attributes {
			attrs = append(attrs, attrToOTLP(a))
		}
		otlpSpans = append(otlpSpans, map[string]any{
			"traceId":           s.traceID,
			"spanId":            s.spanID,
			"parentSpanId":      s.parentID,
			"name":              s.name,
			"kind":              s.kind,
			"startTimeUnixNano": strconv.FormatInt(s.startNano, 10),
			"endTimeUnixNano":   strconv.FormatInt(s.endNano, 10),
			"attributes":        attrs,
			"status":            map[string]any{"code": s.statusCode, "message": s.statusMsg},
		})
	}

	return map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{"attributes": resourceAttrs},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{"name": "github.com/useteploy/teploy-observe/sdk/go", "version": "0.1.0"},
				"spans": otlpSpans,
			}},
		}},
	}
}

// attrToOTLP maps one ALREADY-NORMALIZED attribute (see
// normalizeSpanScalar) onto the OTLP wire form. Unsupported types never
// reach here — SetAttribute rejects them at admission with a diagnostic.
func attrToOTLP(a spanAttr) map[string]any {
	switch v := a.value.(type) {
	case string:
		return map[string]any{"key": a.key, "value": map[string]any{"stringValue": v}}
	case int:
		return map[string]any{"key": a.key, "value": map[string]any{"intValue": strconv.Itoa(v)}}
	case int64:
		return map[string]any{"key": a.key, "value": map[string]any{"intValue": strconv.FormatInt(v, 10)}}
	case bool:
		return map[string]any{"key": a.key, "value": map[string]any{"boolValue": v}}
	case float64:
		return map[string]any{"key": a.key, "value": map[string]any{"doubleValue": v}}
	default:
		// Unreachable for spans built through SetAttribute; defensive only.
		return map[string]any{"key": a.key, "value": map[string]any{"stringValue": ""}}
	}
}

func newTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
