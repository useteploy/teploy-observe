package tracing

// Trace search depth: span-attribute filters and an opt-in orphan mode.
//
// Attribute filters are a TWO-STAGE design because the repo has no verified
// JSON extraction over spans.attributes (a JSONB column, migration 003;
// metrics/attrs.go records "Nucleus has no JSONB extract", while
// internal/query/goals.go uses `->>` on events.properties). Stage one is SQL:
// the existing indexed predicates plus a conservative LIKE over
// CAST(attributes AS TEXT) that can only over-select. Stage two is Go: the
// attribute JSON of every candidate is decoded and each filter is verified
// exactly. The candidate set is bounded (searchCandidateCap) and hitting the
// bound is reported as `truncated`, never hidden.
//
// UNVERIFIED against live Nucleus: that CAST(jsonb AS TEXT) LIKE behaves as
// Postgres does. Correctness does not depend on the LIKE being selective, only
// on it not rejecting a true match, so the patterns are built to be supersets
// regardless of how the engine serialises the JSON (see likeLiteral).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

const (
	// MaxAttrFilters bounds the attribute filters one search may carry.
	MaxAttrFilters = 5
	// MaxAttrValueLen bounds a filter value in bytes.
	MaxAttrValueLen = 256

	// searchCandidateCap is the most root spans one attribute/orphan search
	// verifies in Go. One past it is fetched to detect overflow.
	searchCandidateCap = 2000
	// orphanTraceCap is the most distinct candidate traces the orphan pass
	// inspects, and orphanSpanCap the most spans it loads for them.
	orphanTraceCap = 200
	orphanSpanCap  = 5000
	// maxSearchLimit is the largest page size; it also bounds the trace-id
	// list used to restrict the span-count aggregate.
	maxSearchLimit = 200
)

// attrKeyRe is the only shape of attribute key the search accepts. It is
// matched before a key reaches any query text or LIKE pattern.
var attrKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.\-/]{1,128}$`)

// AttrOp is a comparison applied to one span attribute.
type AttrOp string

const (
	AttrEq       AttrOp = "eq"
	AttrNeq      AttrOp = "neq"
	AttrExists   AttrOp = "exists"
	AttrContains AttrOp = "contains"
)

// AttrFilter is one `key:op:value` attribute predicate. neq means "present
// with a different value" (an absent key does not match); contains is
// case-sensitive.
type AttrFilter struct {
	Key   string
	Op    AttrOp
	Value string
}

// ValidAttrKey reports whether key is acceptable as a filter key.
func ValidAttrKey(key string) bool { return attrKeyRe.MatchString(key) }

// Validate checks a filter without trusting any field.
func (f AttrFilter) Validate() error {
	if !ValidAttrKey(f.Key) {
		return fmt.Errorf("invalid attribute key %q (allowed: A-Z a-z 0-9 _ . - /, 1-128 chars)", truncateForError(f.Key))
	}
	switch f.Op {
	case AttrExists:
		if f.Value != "" {
			return fmt.Errorf("attribute op exists takes no value")
		}
	case AttrEq, AttrNeq, AttrContains:
		if f.Value == "" {
			return fmt.Errorf("attribute op %s requires a value", f.Op)
		}
		if len(f.Value) > MaxAttrValueLen {
			return fmt.Errorf("attribute value exceeds %d bytes", MaxAttrValueLen)
		}
		if !utf8.ValidString(f.Value) || strings.ContainsRune(f.Value, 0) {
			return fmt.Errorf("attribute value must be valid text without NUL")
		}
	default:
		return fmt.Errorf("invalid attribute op %q (allowed: eq, neq, exists, contains)", truncateForError(string(f.Op)))
	}
	return nil
}

func truncateForError(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// ParseAttrFilter parses `key:op[:value]`. The key regex excludes ':' so the
// first colon always ends the key; the value may itself contain colons.
func ParseAttrFilter(s string) (AttrFilter, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return AttrFilter{}, fmt.Errorf("attr must be key:op[:value], got %q", truncateForError(s))
	}
	f := AttrFilter{Key: parts[0], Op: AttrOp(parts[1])}
	if len(parts) == 3 {
		f.Value = parts[2]
	}
	if err := f.Validate(); err != nil {
		return AttrFilter{}, err
	}
	return f, nil
}

// ValidateAttrFilters enforces the count bound and validates each filter.
func ValidateAttrFilters(fs []AttrFilter) error {
	if len(fs) > MaxAttrFilters {
		return fmt.Errorf("at most %d attribute filters are allowed, got %d", MaxAttrFilters, len(fs))
	}
	for _, f := range fs {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// likeLiteral renders s for use inside a LIKE pattern. It never emits an
// escape sequence (the engine's escape handling is unverified); instead it
// guarantees the result can only match MORE than the literal:
//   - '_' is kept as is (a single-character wildcard, a superset of itself);
//   - '%' and '\' cannot be neutralised that way, and any byte that JSON may
//     serialise differently (quote, control, non-ASCII) cannot be matched
//     textually, so ok=false and the caller skips that narrowing and leaves it
//     to the exact Go-side check.
//
// The caller's `%` wildcards are added around the result, never inside it.
func likeLiteral(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f || c == '%' || c == '\\' || c == '"' {
			return "", false
		}
	}
	return s, true
}

// SearchOptions are the filters of one trace search. Service, Operation,
// Status and the duration bounds apply to the listed (root) span.
type SearchOptions struct {
	Service        string
	Operation      string
	Status         string
	MinDuration    int64
	MaxDuration    int64
	Limit          int
	Offset         int
	Attrs          []AttrFilter
	IncludeOrphans bool
}

// SearchResult is a page of trace summaries plus the honesty labels.
type SearchResult struct {
	Traces []TraceSummary `json:"traces"`
	// Truncated is true when a bound cut the candidate set, so the page may be
	// missing matches that exist. TruncatedReason names which bound.
	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`
	CandidateCap    int    `json:"candidate_cap,omitempty"`
}

// buildSearchWhere assembles the shared WHERE clause and its bound params.
// siteID and every filter value travel as bound parameters; the only
// interpolations are integers from int64 (%d) and the placeholder numbers.
// Attribute LIKE narrowing is included only when narrowAttrs is set.
func buildSearchWhere(siteID string, from, to time.Time, o SearchOptions, narrowAttrs bool) (string, []any) {
	where := "site_id = $1 AND start_time >= $2 AND start_time < $3"
	params := []any{siteID, dbutil.IntParam(from.UnixMilli()), dbutil.IntParam(to.UnixMilli())}
	idx := 4
	if o.Service != "" {
		where += fmt.Sprintf(" AND service_name = $%d", idx)
		params = append(params, o.Service)
		idx++
	}
	if o.Operation != "" {
		where += fmt.Sprintf(" AND operation_name = $%d", idx)
		params = append(params, o.Operation)
		idx++
	}
	if o.Status != "" {
		where += fmt.Sprintf(" AND status_code = $%d", idx)
		params = append(params, o.Status)
		idx++
	}
	if o.MinDuration > 0 {
		where += fmt.Sprintf(" AND CAST(duration_ms AS BIGINT) >= %d", o.MinDuration)
	}
	if o.MaxDuration > 0 {
		where += fmt.Sprintf(" AND CAST(duration_ms AS BIGINT) <= %d", o.MaxDuration)
	}
	if narrowAttrs {
		for _, f := range o.Attrs {
			// Every op needs the key present. Key chars are regex-validated, so
			// the quoted form is what JSON would hold verbatim.
			if k, ok := likeLiteral(f.Key); ok {
				where += fmt.Sprintf(" AND CAST(attributes AS TEXT) LIKE $%d", idx)
				params = append(params, `%"`+k+`"%`)
				idx++
			}
			if f.Op == AttrEq || f.Op == AttrContains {
				if v, ok := likeLiteral(f.Value); ok {
					where += fmt.Sprintf(" AND CAST(attributes AS TEXT) LIKE $%d", idx)
					params = append(params, "%"+v+"%")
					idx++
				}
			}
		}
	}
	return where, params
}

// spanSelectCols is the column list of the Go-verified search path.
const spanSelectCols = `trace_id, span_id, parent_span_id, service_name, operation_name,
			span_kind,
			CAST(start_time AS TEXT) AS start_time,
			CAST(end_time AS TEXT) AS end_time,
			CAST(duration_ms AS TEXT) AS duration_ms,
			status_code, status_message,
			COALESCE(attributes, '') AS attributes`

// buildRootCandidateSQL selects up to cap+1 root spans, newest first.
func buildRootCandidateSQL(where string, cap int) string {
	return fmt.Sprintf(`SELECT %s
		 FROM spans
		 WHERE %s AND parent_span_id = ''
		 ORDER BY start_time DESC
		 LIMIT %d`, spanSelectCols, where, cap+1)
}

// buildOrphanCandidateSQL selects trace ids of non-root spans in the window.
func buildOrphanCandidateSQL(cap int) string {
	return fmt.Sprintf(`SELECT trace_id
		 FROM spans
		 WHERE site_id = $1 AND start_time >= $2 AND start_time < $3 AND parent_span_id <> ''
		 ORDER BY start_time DESC
		 LIMIT %d`, cap+1)
}

// buildTraceSpansSQL loads the spans of the given trace ids ($2..). Ordered by
// trace so a row-cap cut only ever splits the final trace.
func buildTraceSpansSQL(n, cap int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+2)
	}
	return fmt.Sprintf(`SELECT %s
		 FROM spans
		 WHERE site_id = $1 AND trace_id IN (%s)
		 ORDER BY trace_id, start_time ASC
		 LIMIT %d`, spanSelectCols, strings.Join(ph, ","), cap+1)
}

// buildSpanCountSQL counts spans per trace for exactly the page's trace ids
// ($4..), inside the same site+window as the search (precedent for bound IN
// lists: internal/jobs/rollups.go, funnels.go).
func buildSpanCountSQL(n int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+4)
	}
	return fmt.Sprintf(`SELECT trace_id, COUNT(*) AS n FROM spans
		 WHERE site_id = $1 AND start_time >= $2 AND start_time < $3 AND trace_id IN (%s)
		 GROUP BY trace_id`, strings.Join(ph, ","))
}

// attrText renders one decoded JSON attribute value the way the filters
// compare it. Stored values are strings (AttrsToMap); other scalars are
// tolerated for rows written by other paths.
func attrText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// decodeAttrs parses a stored attributes JSON object; anything else is nil.
func decodeAttrs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	return m
}

// matchAttrs is the exact check: every filter must hold.
func matchAttrs(raw string, fs []AttrFilter) bool {
	if len(fs) == 0 {
		return true
	}
	m := decodeAttrs(raw)
	if m == nil {
		return false
	}
	for _, f := range fs {
		v, present := m[f.Key]
		if !present {
			return false
		}
		s := attrText(v)
		switch f.Op {
		case AttrExists:
		case AttrEq:
			if s != f.Value {
				return false
			}
		case AttrNeq:
			if s == f.Value {
				return false
			}
		case AttrContains:
			if !strings.Contains(s, f.Value) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type spanCountRow struct {
	TraceID string `db:"trace_id"`
	N       int64  `db:"n"`
}

func (q *QueryService) runSpans(ctx context.Context, sql string, args ...any) ([]Span, error) {
	if q.querySpans != nil {
		return q.querySpans(ctx, sql, args...)
	}
	return nucleus.Query[Span](ctx, q.db.SQL(), sql, args...)
}

func (q *QueryService) runSummaries(ctx context.Context, sql string, args ...any) ([]TraceSummary, error) {
	if q.querySummaries != nil {
		return q.querySummaries(ctx, sql, args...)
	}
	return nucleus.Query[TraceSummary](ctx, q.db.SQL(), sql, args...)
}

func (q *QueryService) runCounts(ctx context.Context, sql string, args ...any) ([]spanCountRow, error) {
	if q.queryCounts != nil {
		return q.queryCounts(ctx, sql, args...)
	}
	return nucleus.Query[spanCountRow](ctx, q.db.SQL(), sql, args...)
}

// fillSpanCounts sets SpanCount for the page's traces that lack one, using one
// aggregate restricted to those trace ids. A failure leaves counts at zero
// (the page is still correct without them), as before.
func (q *QueryService) fillSpanCounts(ctx context.Context, siteID string, from, to time.Time, page []TraceSummary) {
	ids := make([]string, 0, len(page))
	for _, t := range page {
		if t.SpanCount == 0 {
			ids = append(ids, t.TraceID)
		}
	}
	if len(ids) == 0 {
		return
	}
	params := []any{siteID, dbutil.IntParam(from.UnixMilli()), dbutil.IntParam(to.UnixMilli())}
	for _, id := range ids {
		params = append(params, id)
	}
	counts, err := q.runCounts(ctx, buildSpanCountSQL(len(ids)), params...)
	if err != nil {
		return
	}
	byTrace := make(map[string]int64, len(counts))
	for _, c := range counts {
		byTrace[c.TraceID] = c.N
	}
	for i := range page {
		if page[i].SpanCount == 0 {
			page[i].SpanCount = byTrace[page[i].TraceID]
		}
	}
}

func summaryOf(s Span) TraceSummary {
	return TraceSummary{
		TraceID: s.TraceID, RootService: s.ServiceName, RootOp: s.OperationName,
		StartTime: s.StartTime, DurationMs: s.DurationMs, StatusCode: s.StatusCode,
	}
}

// SearchTracesEx is SearchTraces with attribute filters and the optional
// orphan mode. Guarded (O12): concurrency slot, time budget, page-depth cap.
// With no attribute filters and no orphans it runs the original SQL-paged
// root search; otherwise it verifies a bounded candidate set in Go.
func (q *QueryService) SearchTracesEx(ctx context.Context, siteID string, from, to time.Time, o SearchOptions) (SearchResult, error) {
	if err := ValidateAttrFilters(o.Attrs); err != nil {
		return SearchResult{}, err
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.Limit > maxSearchLimit {
		o.Limit = maxSearchLimit
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	if int64(o.Limit)+int64(o.Offset) > q.qguard.MaxRows() {
		return SearchResult{}, q.qguard.RowRefusal()
	}
	qctx, release, err := q.qguard.Begin(ctx, siteID)
	if err != nil {
		return SearchResult{}, err
	}
	defer release()

	var res SearchResult
	if len(o.Attrs) == 0 && !o.IncludeOrphans {
		res, err = q.searchRootsPaged(qctx, siteID, from, to, o)
	} else {
		res, err = q.searchVerified(qctx, siteID, from, to, o)
	}
	if err != nil {
		return SearchResult{}, q.qguard.DeadlineError(ctx, err)
	}
	if res.Traces == nil {
		res.Traces = []TraceSummary{}
	}
	return res, nil
}

// searchRootsPaged is the original root-only search, SQL-paged.
func (q *QueryService) searchRootsPaged(ctx context.Context, siteID string, from, to time.Time, o SearchOptions) (SearchResult, error) {
	where, params := buildSearchWhere(siteID, from, to, o, false)
	sql := fmt.Sprintf(`SELECT trace_id,
			service_name AS root_service,
			operation_name AS root_op,
			CAST(start_time AS TEXT) AS start_time,
			CAST(duration_ms AS TEXT) AS duration_ms,
			'0' AS span_count,
			status_code
		 FROM spans
		 WHERE %s AND parent_span_id = ''
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, where, o.Limit, o.Offset)
	page, err := q.runSummaries(ctx, sql, params...)
	if err != nil || len(page) == 0 {
		return SearchResult{Traces: page}, err
	}
	q.fillSpanCounts(ctx, siteID, from, to, page)
	return SearchResult{Traces: page}, nil
}

// searchVerified is the Go-paged path: bounded root candidates, exact
// attribute verification, optional orphan traces, then one merged page.
func (q *QueryService) searchVerified(ctx context.Context, siteID string, from, to time.Time, o SearchOptions) (SearchResult, error) {
	res := SearchResult{CandidateCap: searchCandidateCap}
	where, params := buildSearchWhere(siteID, from, to, o, true)
	cands, err := q.runSpans(ctx, buildRootCandidateSQL(where, searchCandidateCap), params...)
	if err != nil {
		return SearchResult{}, err
	}
	if len(cands) > searchCandidateCap {
		cands = cands[:searchCandidateCap]
		res.Truncated = true
		res.TruncatedReason = "root candidate cap reached; narrow the time range or filters"
	}
	var all []TraceSummary
	for _, s := range cands {
		if matchAttrs(s.Attributes, o.Attrs) {
			all = append(all, summaryOf(s))
		}
	}
	if o.IncludeOrphans {
		orphans, trunc, reason, err := q.findOrphans(ctx, siteID, from, to, o)
		if err != nil {
			return SearchResult{}, err
		}
		if trunc && !res.Truncated {
			res.Truncated, res.TruncatedReason = true, reason
		}
		all = append(all, orphans...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].StartTime.Equal(all[j].StartTime) {
			return all[i].StartTime.After(all[j].StartTime)
		}
		return all[i].TraceID < all[j].TraceID
	})
	if o.Offset >= len(all) {
		return res, nil
	}
	end := o.Offset + o.Limit
	if end > len(all) {
		end = len(all)
	}
	res.Traces = all[o.Offset:end]
	q.fillSpanCounts(ctx, siteID, from, to, res.Traces)
	return res, nil
}

// findOrphans surfaces traces in the window with no root span whose earliest
// span points at a parent that is not in the trace (root missing or late).
// Best effort over the newest searchCandidateCap non-root spans and at most
// orphanTraceCap traces; hitting either bound is reported via truncated.
// Service/operation/status/duration/attribute filters apply to the earliest
// span (the span listed in place of the root).
func (q *QueryService) findOrphans(ctx context.Context, siteID string, from, to time.Time, o SearchOptions) ([]TraceSummary, bool, string, error) {
	base := []any{siteID, dbutil.IntParam(from.UnixMilli()), dbutil.IntParam(to.UnixMilli())}
	cands, err := q.runSpans(ctx, buildOrphanCandidateSQL(searchCandidateCap), base...)
	if err != nil {
		return nil, false, "", err
	}
	truncated, reason := false, ""
	if len(cands) > searchCandidateCap {
		cands = cands[:searchCandidateCap]
		truncated, reason = true, "orphan candidate cap reached; only the newest non-root spans were inspected"
	}
	var ids []string
	seen := map[string]bool{}
	for _, c := range cands {
		if c.TraceID == "" || seen[c.TraceID] {
			continue
		}
		if len(ids) == orphanTraceCap {
			truncated, reason = true, "orphan trace cap reached; only the newest traces were inspected"
			break
		}
		seen[c.TraceID] = true
		ids = append(ids, c.TraceID)
	}
	if len(ids) == 0 {
		return nil, truncated, reason, nil
	}
	params := []any{siteID}
	for _, id := range ids {
		params = append(params, id)
	}
	spans, err := q.runSpans(ctx, buildTraceSpansSQL(len(ids), orphanSpanCap), params...)
	if err != nil {
		return nil, false, "", err
	}
	if len(spans) > orphanSpanCap {
		// The cut can only split the final trace (ordered by trace_id); drop it
		// rather than classify a partial span set.
		spans = spans[:orphanSpanCap]
		last := spans[len(spans)-1].TraceID
		for len(spans) > 0 && spans[len(spans)-1].TraceID == last {
			spans = spans[:len(spans)-1]
		}
		truncated, reason = true, "orphan span cap reached; some candidate traces were not inspected"
	}
	byTrace := map[string][]Span{}
	for _, s := range spans {
		byTrace[s.TraceID] = append(byTrace[s.TraceID], s)
	}
	var out []TraceSummary
	for _, id := range ids {
		sum, earliest, ok := classifyOrphan(byTrace[id])
		if !ok || !orphanMatches(sum, earliest, from, to, o) {
			continue
		}
		out = append(out, sum)
	}
	return out, truncated, reason, nil
}

// classifyOrphan returns the summary for a trace with no root span whose
// earliest span's parent is absent from the trace.
func classifyOrphan(spans []Span) (TraceSummary, Span, bool) {
	if len(spans) == 0 {
		return TraceSummary{}, Span{}, false
	}
	ids := make(map[string]bool, len(spans))
	earliest := spans[0]
	for _, s := range spans {
		if s.ParentSpanID == "" {
			return TraceSummary{}, Span{}, false // has a root: listed by the root search
		}
		ids[s.SpanID] = true
		if s.StartTime.Before(earliest.StartTime) ||
			(s.StartTime.Equal(earliest.StartTime) && s.SpanID < earliest.SpanID) {
			earliest = s
		}
	}
	if ids[earliest.ParentSpanID] {
		return TraceSummary{}, Span{}, false
	}
	sum := summaryOf(earliest)
	sum.RootMissing = true
	sum.SpanCount = int64(len(spans))
	// Extent of the whole trace, since there is no root duration to show.
	var end time.Time
	for _, s := range spans {
		if s.EndTime.After(end) {
			end = s.EndTime
		}
		if s.StatusCode == "error" {
			sum.StatusCode = "error"
		}
	}
	if end.After(earliest.StartTime) {
		sum.DurationMs = end.Sub(earliest.StartTime).Milliseconds()
	}
	return sum, earliest, true
}

// orphanMatches applies the search filters to an orphan trace's earliest span.
func orphanMatches(sum TraceSummary, earliest Span, from, to time.Time, o SearchOptions) bool {
	if sum.StartTime.Before(from) || !sum.StartTime.Before(to) {
		return false
	}
	if o.Service != "" && sum.RootService != o.Service {
		return false
	}
	if o.Operation != "" && sum.RootOp != o.Operation {
		return false
	}
	if o.Status != "" && sum.StatusCode != o.Status {
		return false
	}
	if o.MinDuration > 0 && sum.DurationMs < o.MinDuration {
		return false
	}
	if o.MaxDuration > 0 && sum.DurationMs > o.MaxDuration {
		return false
	}
	return matchAttrs(earliest.Attributes, o.Attrs)
}
