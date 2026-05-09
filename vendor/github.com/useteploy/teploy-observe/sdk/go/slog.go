package observe

import (
	"context"
	"log/slog"
)

// SlogHandler implements slog.Handler so existing slog.Logger callers
// in any application can mirror their logs to Observe with no code changes.
//
// Use NewSlogHandler to wrap an existing handler — records flow to BOTH the
// original handler (typically stderr) and Observe.
type SlogHandler struct {
	client     *Client
	wrapped    slog.Handler
	level      slog.Level
	groupAttrs []slog.Attr
	groupName  string
}

// NewSlogHandler returns a slog.Handler that mirrors records to Observe at
// or above level. If wrapped is non-nil, records ALSO flow through it
// (use this to keep stderr output unchanged while adding Observe ingest).
func (c *Client) NewSlogHandler(level slog.Level, wrapped slog.Handler) *SlogHandler {
	return &SlogHandler{client: c, wrapped: wrapped, level: level}
}

// Enabled reports whether the handler handles records at the given level.
func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.wrapped != nil && h.wrapped.Enabled(ctx, level) {
		return true
	}
	return level >= h.level
}

// Handle writes the record. Errors from Observe ingest are swallowed — the
// wrapped handler still gets called so logs aren't silently lost.
func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	// Always forward to wrapped first so stderr output is unblocked even if
	// Observe is slow/down.
	var wrapErr error
	if h.wrapped != nil {
		wrapErr = h.wrapped.Handle(ctx, r)
	}

	if r.Level >= h.level {
		attrs := make(map[string]any)
		// Include any group-scoped attrs added via WithAttrs.
		for _, a := range h.groupAttrs {
			addAttr(attrs, a)
		}
		r.Attrs(func(a slog.Attr) bool {
			addAttr(attrs, a)
			return true
		})
		entry := LogEntry{
			SiteID:      h.client.opts.SiteID,
			Level:       slogLevelString(r.Level),
			Message:     r.Message,
			ServiceName: h.client.opts.ServiceName,
			Attributes:  attrs,
		}
		// Capture trace context if a span is active on ctx.
		if span := SpanFromContext(ctx); span != nil {
			entry.TraceID = span.TraceID()
			entry.SpanID = span.SpanID()
		}
		h.client.mu.Lock()
		h.client.logs = append(h.client.logs, entry)
		full := len(h.client.logs) >= h.client.opts.LogBatchSize
		h.client.mu.Unlock()
		if full {
			go func() { _ = h.client.flushLogs(context.Background()) }()
		}
	}
	return wrapErr
}

// WithAttrs returns a handler whose records will have the given attrs added.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.groupAttrs = append([]slog.Attr(nil), h.groupAttrs...)
	clone.groupAttrs = append(clone.groupAttrs, attrs...)
	if h.wrapped != nil {
		clone.wrapped = h.wrapped.WithAttrs(attrs)
	}
	return &clone
}

// WithGroup returns a handler that prefixes attrs with name. We flatten the
// group name into the attribute key so it round-trips through Observe's
// JSON-blob attribute storage.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	clone := *h
	if h.groupName != "" {
		clone.groupName = h.groupName + "." + name
	} else {
		clone.groupName = name
	}
	if h.wrapped != nil {
		clone.wrapped = h.wrapped.WithGroup(name)
	}
	return &clone
}

func addAttr(m map[string]any, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	m[a.Key] = a.Value.Any()
}

func slogLevelString(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	case l <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}
