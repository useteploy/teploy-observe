package tracing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Ingest bounds for span links. A hostile or buggy exporter can attach an
// unbounded link list to every span; the caps keep one export from turning
// into an arbitrarily large span_links write. Anything past a cap is dropped
// and counted (IngestResponse.LinksTruncated, plus a warn log).
const (
	maxLinksPerSpan   = 128
	maxAttrsPerLink   = 32
	maxLinkIDLen      = 64
	maxTraceStateLen  = 512
	spanLinksCols     = 10
	spanLinksBatchLen = 50
	spanLinksColList  = `trace_id, span_id, link_idx, tenant_id, site_id, linked_trace_id, linked_span_id, trace_state, attributes, start_time`
)

// flatLink is one link row, already normalised and bounded.
type flatLink struct {
	LinkIdx        int
	LinkedTraceID  string
	LinkedSpanID   string
	TraceState     string
	AttributesJSON string
}

// flattenLinks bounds and normalises a span's links. A link without both a
// target trace id and span id is meaningless and is dropped (counted). The
// returned count covers every dropped link, clipped attribute and clipped
// field.
func flattenLinks(links []SpanLink) (out []flatLink, truncated int) {
	if len(links) == 0 {
		return nil, 0
	}
	if len(links) > maxLinksPerSpan {
		truncated += len(links) - maxLinksPerSpan
		links = links[:maxLinksPerSpan]
	}
	out = make([]flatLink, 0, len(links))
	for _, l := range links {
		if l.TraceID == "" || l.SpanID == "" || len(l.TraceID) > maxLinkIDLen || len(l.SpanID) > maxLinkIDLen {
			truncated++
			continue
		}
		attrs := l.Attributes
		if len(attrs) > maxAttrsPerLink {
			truncated += len(attrs) - maxAttrsPerLink
			attrs = attrs[:maxAttrsPerLink]
		}
		state := l.TraceState
		if len(state) > maxTraceStateLen {
			truncated++
			state = state[:maxTraceStateLen]
		}
		out = append(out, flatLink{
			LinkIdx:        len(out),
			LinkedTraceID:  l.TraceID,
			LinkedSpanID:   l.SpanID,
			TraceState:     state,
			AttributesJSON: jsonOrEmpty(AttrsToMap(attrs)),
		})
	}
	return out, truncated
}

func buildSpanLinkPlaceholders(rows int) string {
	var b strings.Builder
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < spanLinksCols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			n++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// spanLinkArgs flattens a batch's links into (args per chunk) rows. Split out
// from the INSERT so the row shape is unit-testable without a database.
type spanLinkRow struct {
	sp *flatSpan
	l  *flatLink
}

func collectLinkRows(flat []flatSpan) []spanLinkRow {
	var rows []spanLinkRow
	for i := range flat {
		for j := range flat[i].Links {
			rows = append(rows, spanLinkRow{&flat[i], &flat[i].Links[j]})
		}
	}
	return rows
}

func spanLinkArgs(dst []any, siteID string, r spanLinkRow) []any {
	return append(dst,
		r.sp.TraceID, r.sp.SpanID, r.l.LinkIdx, "default", siteID,
		r.l.LinkedTraceID, r.l.LinkedSpanID, r.l.TraceState,
		nullableJSON(r.l.AttributesJSON), r.sp.StartMs,
	)
}

// insertSpanLinks writes every link of the batch in chunked multi-row INSERTs
// on the caller's connection - the ingest passes its transaction so links
// commit or roll back with the spans they belong to.
func insertSpanLinks(ctx context.Context, sql *nucleus.SQLModel, siteID string, flat []flatSpan) error {
	rows := collectLinkRows(flat)
	for start := 0; start < len(rows); start += spanLinksBatchLen {
		end := start + spanLinksBatchLen
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*spanLinksCols)
		for _, r := range chunk {
			args = spanLinkArgs(args, siteID, r)
		}
		q := "INSERT INTO span_links (" + spanLinksColList + ") VALUES " + buildSpanLinkPlaceholders(len(chunk))
		if _, err := sql.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("batch insert span_links %d-%d: %w", start+1, end, err)
		}
	}
	return nil
}

// StoredSpanLink is a persisted span link as returned to API clients.
type StoredSpanLink struct {
	TraceID       string `json:"trace_id"`
	SpanID        string `json:"span_id"`
	LinkIdx       int64  `json:"link_idx"`
	LinkedTraceID string `json:"linked_trace_id"`
	LinkedSpanID  string `json:"linked_span_id"`
	TraceState    string `json:"trace_state"`
	Attributes    string `json:"attributes"`
}

// Read-side bounds for span links. Ingest caps links per span (maxLinksPerSpan);
// these keep the read bounded regardless of what is stored.
const (
	maxTraceLinksPerSpan = 128
	maxTraceLinksTotal   = 5000
	// linkWindowSlack widens a caller's time hint on both sides.
	linkWindowSlack = time.Hour
)

// LinkWindow is an optional time hint for link reads: the range in which the
// owning spans started. span_links is ordered by start time, so a hint turns a
// whole-retention scan into a narrow one. The zero value means no hint.
type LinkWindow struct {
	From, To time.Time
}

func (w LinkWindow) bounded() bool { return !w.From.IsZero() && !w.To.IsZero() }

// buildTraceLinksSQL returns the statement and params. With a hint the owning
// span start_time is bounded to [From-slack, To+slack]; the total is always
// capped.
func buildTraceLinksSQL(traceID, siteID string, w LinkWindow) (string, []any) {
	where := "trace_id = $1 AND site_id = $2"
	params := []any{traceID, siteID}
	if w.bounded() {
		where += " AND start_time >= CAST($3 AS BIGINT) AND start_time <= CAST($4 AS BIGINT)"
		params = append(params,
			dbutil.IntParam(w.From.Add(-linkWindowSlack).UnixMilli()),
			dbutil.IntParam(w.To.Add(linkWindowSlack).UnixMilli()))
	}
	return fmt.Sprintf(`SELECT trace_id, span_id,
			CAST(link_idx AS TEXT) AS link_idx,
			linked_trace_id, linked_span_id, trace_state,
			COALESCE(attributes, '') AS attributes
		 FROM span_links
		 WHERE %s
		 ORDER BY span_id ASC, link_idx ASC
		 LIMIT %d`, where, maxTraceLinksTotal), params
}

// capLinksPerSpan keeps at most maxTraceLinksPerSpan links per span. Input is
// ordered by span_id so a span's links are contiguous.
func capLinksPerSpan(in []StoredSpanLink) []StoredSpanLink {
	out := in[:0:0]
	run, prev := 0, ""
	for _, l := range in {
		if l.SpanID != prev {
			prev, run = l.SpanID, 0
		}
		if run++; run <= maxTraceLinksPerSpan {
			out = append(out, l)
		}
	}
	return out
}

// GetTraceLinks returns the links of every span in a trace, scoped to the
// site. w is an optional time hint (zero value: none); the result is always
// capped (128 per span, 5000 total).
func (q *QueryService) GetTraceLinks(ctx context.Context, traceID, siteID string, w LinkWindow) ([]StoredSpanLink, error) {
	sql, params := buildTraceLinksSQL(traceID, siteID, w)
	rows, err := nucleus.Query[StoredSpanLink](ctx, q.db.SQL(), sql, params...)
	if err != nil {
		return nil, err
	}
	return capLinksPerSpan(rows), nil
}

// GetSpanLinks returns the links of one span.
func (q *QueryService) GetSpanLinks(ctx context.Context, traceID, spanID, siteID string, w LinkWindow) ([]StoredSpanLink, error) {
	all, err := q.GetTraceLinks(ctx, traceID, siteID, w)
	if err != nil {
		return nil, err
	}
	out := make([]StoredSpanLink, 0)
	for _, l := range all {
		if l.SpanID == spanID {
			out = append(out, l)
		}
	}
	return out, nil
}

// spansWindow is the start-time range of spans (zero value when empty or when
// any start is unknown: no hint rather than a wrong one).
func spansWindow(spans []Span) LinkWindow {
	var w LinkWindow
	for _, s := range spans {
		if s.StartTime.IsZero() {
			return LinkWindow{}
		}
		if w.From.IsZero() || s.StartTime.Before(w.From) {
			w.From = s.StartTime
		}
		if s.StartTime.After(w.To) {
			w.To = s.StartTime
		}
	}
	return w
}

// attachLinks fills Span.Links from one trace-wide links query. Best effort:
// a failure (for example the table not yet migrated) leaves Links empty rather
// than failing the waterfall, which is still correct without them.
func (q *QueryService) attachLinks(ctx context.Context, traceID, siteID string, spans []Span) {
	if len(spans) == 0 {
		return
	}
	links, err := q.GetTraceLinks(ctx, traceID, siteID, spansWindow(spans))
	if err != nil || len(links) == 0 {
		return
	}
	bySpan := make(map[string][]StoredSpanLink, len(links))
	for _, l := range links {
		bySpan[l.SpanID] = append(bySpan[l.SpanID], l)
	}
	for i := range spans {
		spans[i].Links = bySpan[spans[i].SpanID]
	}
}
