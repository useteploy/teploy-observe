package observe

import (
	"context"
	"fmt"
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
	groupAttrs []scopedAttr
	groupName  string

	// crumbLevel/crumbs enable breadcrumb recording (WithBreadcrumbs);
	// noMirror suppresses log shipping for breadcrumb-only handlers.
	crumbs     bool
	crumbLevel slog.Level
	noMirror   bool
}

// NewSlogBreadcrumbHandler returns a slog.Handler that records records at
// or above level as breadcrumbs (attached to later CaptureException /
// CaptureMessage calls) WITHOUT shipping them as Observe logs. If wrapped
// is non-nil, records also flow through it.
func (c *Client) NewSlogBreadcrumbHandler(level slog.Level, wrapped slog.Handler) *SlogHandler {
	return &SlogHandler{client: c, wrapped: wrapped, crumbs: true, crumbLevel: level, noMirror: true}
}

// WithBreadcrumbs returns a copy that ALSO records records at or above
// level as breadcrumbs, in addition to mirroring logs.
func (h *SlogHandler) WithBreadcrumbs(level slog.Level) *SlogHandler {
	clone := *h
	clone.crumbs = true
	clone.crumbLevel = level
	return &clone
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
	if h.crumbs && level >= h.crumbLevel {
		return true
	}
	return !h.noMirror && level >= h.level
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

	if h.crumbs && r.Level >= h.crumbLevel {
		h.recordBreadcrumb(r)
	}

	if !h.noMirror && r.Level >= h.level {
		attrs := make(map[string]any)
		// Include any group-scoped attrs added via WithAttrs.
		for _, a := range h.groupAttrs {
			crumbAttr(attrs, a.prefix, a.attr)
		}
		r.Attrs(func(a slog.Attr) bool {
			crumbAttr(attrs, groupPrefix(h.groupName), a)
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
		// AUD-036 (round 2): shared admission path — serialize at admission,
		// bounded queue, owned-worker wakeup (the old direct append raced
		// the byte accounting and spawned an untracked flush goroutine).
		h.client.admitLog(entry)
	}
	return wrapErr
}

// recordBreadcrumb records r as a breadcrumb; attrs become its data.
func (h *SlogHandler) recordBreadcrumb(r slog.Record) {
	defer func() { _ = recover() }()
	data := make(map[string]any)
	for _, a := range h.groupAttrs {
		crumbAttr(data, a.prefix, a.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		crumbAttr(data, groupPrefix(h.groupName), a)
		return true
	})
	lvl := "debug"
	switch {
	case r.Level >= slog.LevelError:
		lvl = "error"
	case r.Level >= slog.LevelWarn:
		lvl = "warning"
	case r.Level >= slog.LevelInfo:
		lvl = "info"
	}
	h.client.AddBreadcrumb(Breadcrumb{
		Type:      "log",
		Category:  "slog",
		Message:   r.Message,
		Level:     lvl,
		Data:      data,
		Timestamp: r.Time.UnixMilli(),
	})
}

// crumbAttr flattens an attr (groups become dotted keys) into data,
// rendering errors and Stringers as text so they survive JSON.
func crumbAttr(m map[string]any, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := prefix + a.Key
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			next := prefix
			if a.Key != "" {
				next = groupPrefix(key)
			}
			crumbAttr(m, next, g)
		}
		return
	}
	switch v := a.Value.Any().(type) {
	case error:
		m[key] = v.Error()
	case fmt.Stringer:
		m[key] = v.String()
	default:
		m[key] = v
	}
}

// WithAttrs returns a handler whose records will have the given attrs added.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.groupAttrs = append([]scopedAttr(nil), h.groupAttrs...)
	for _, a := range attrs {
		clone.groupAttrs = append(clone.groupAttrs, scopedAttr{groupPrefix(h.groupName), a})
	}
	if h.wrapped != nil {
		clone.wrapped = h.wrapped.WithAttrs(attrs)
	}
	return &clone
}

// WithGroup returns a handler that prefixes attrs with name. We flatten the
// group name into the attribute key so it round-trips through Observe's
// JSON-blob attribute storage.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
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

type scopedAttr struct {
	prefix string
	attr   slog.Attr
}

func groupPrefix(name string) string {
	if name == "" {
		return ""
	}
	return name + "."
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
