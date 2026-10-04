package tracing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

func TestParseAttrFilter(t *testing.T) {
	ok := []struct {
		in   string
		want AttrFilter
	}{
		{"http.status_code:eq:500", AttrFilter{"http.status_code", AttrEq, "500"}},
		{"http.route:contains:/api/v1", AttrFilter{"http.route", AttrContains, "/api/v1"}},
		{"db.system:exists", AttrFilter{"db.system", AttrExists, ""}},
		{"db.system:exists:", AttrFilter{"db.system", AttrExists, ""}},
		{"url.full:eq:http://x:80/a", AttrFilter{"url.full", AttrEq, "http://x:80/a"}},
		{"a-b/c_d.e:neq:x y", AttrFilter{"a-b/c_d.e", AttrNeq, "x y"}},
	}
	for _, c := range ok {
		got, err := ParseAttrFilter(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: got %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"",
		"key",
		"key:eq",                  // eq needs a value
		":eq:x",                   // empty key
		"key:regex:x",             // unknown op
		"key:EQ:x",                // ops are lowercase
		"key:exists:nope",         // exists takes no value
		"a'b:eq:x",                // quote in key
		"a b:eq:x",                // space in key
		"a\"b:eq:x",               // json quote in key
		"a;DROP TABLE spans:eq:x", // statement in key
		"a%:eq:x",                 // LIKE wildcard in key
		"key\n:eq:x",              // control char
		"kéy:eq:x",                // non-ASCII key
		strings.Repeat("k", 129) + ":eq:x",
		"key:eq:" + strings.Repeat("v", MaxAttrValueLen+1),
		"key:eq:a\x00b",
		"key:eq:\xff\xfe",
	}
	for _, in := range bad {
		if f, err := ParseAttrFilter(in); err == nil {
			t.Errorf("%q: accepted as %+v", truncateForError(in), f)
		}
	}
}

func TestValidateAttrFiltersBound(t *testing.T) {
	f := AttrFilter{"k", AttrExists, ""}
	if err := ValidateAttrFilters(make5(f, MaxAttrFilters)); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if err := ValidateAttrFilters(make5(f, MaxAttrFilters+1)); err == nil {
		t.Fatal("one past the bound accepted")
	}
}

func make5(f AttrFilter, n int) []AttrFilter {
	out := make([]AttrFilter, n)
	for i := range out {
		out[i] = f
	}
	return out
}

// Hostile values and keys never reach the SQL text: the WHERE clause holds
// placeholders only, and every user string travels as a bound parameter.
func TestBuildSearchWhereNeverInterpolatesUserText(t *testing.T) {
	hostile := []string{
		"x' OR '1'='1", `"; DROP TABLE spans; --`, "100%", "a_b", `back\slash`,
		"x') UNION SELECT 1 --", "é中", "$1", "'",
	}
	from, to := time.Unix(100, 0), time.Unix(200, 0)
	for _, v := range hostile {
		o := SearchOptions{
			Service: v, Operation: v, Status: v,
			Attrs: []AttrFilter{{"http.route", AttrEq, v}, {"k_1", AttrContains, v}},
		}
		where, params := buildSearchWhere("site", from, to, o, true)
		if strings.Contains(where, v) && len(v) > 2 {
			t.Errorf("value %q leaked into SQL: %s", v, where)
		}
		if strings.Contains(where, "'") {
			t.Errorf("quote in SQL for %q: %s", v, where)
		}
		// Placeholders are dense and match the param count.
		for i := 1; i <= len(params); i++ {
			if !strings.Contains(where, fmt.Sprintf("$%d", i)) {
				t.Errorf("param $%d unused in %s", i, where)
			}
		}
		if strings.Contains(where, fmt.Sprintf("$%d", len(params)+1)) {
			t.Errorf("placeholder beyond params in %s", where)
		}
	}
}

func TestBuildSearchWhereLikePatterns(t *testing.T) {
	from, to := time.Unix(100, 0), time.Unix(200, 0)
	cases := []struct {
		name       string
		f          AttrFilter
		wantParams []string // trailing LIKE params
	}{
		{"exists", AttrFilter{"k.a", AttrExists, ""}, []string{`%"k.a"%`}},
		{"eq plain", AttrFilter{"k.a", AttrEq, "GET"}, []string{`%"k.a"%`, "%GET%"}},
		{"neq narrows by key only", AttrFilter{"k.a", AttrNeq, "GET"}, []string{`%"k.a"%`}},
		{"contains", AttrFilter{"k.a", AttrContains, "/api"}, []string{`%"k.a"%`, "%/api%"}},
		// Wildcards that cannot be neutralised drop the value narrowing; the
		// exact Go-side check still enforces the value.
		{"percent value", AttrFilter{"k.a", AttrEq, "100%"}, []string{`%"k.a"%`}},
		{"backslash value", AttrFilter{"k.a", AttrEq, `a\b`}, []string{`%"k.a"%`}},
		{"quote value", AttrFilter{"k.a", AttrEq, `a"b`}, []string{`%"k.a"%`}},
		{"non-ascii value", AttrFilter{"k.a", AttrEq, "café"}, []string{`%"k.a"%`}},
		// '_' stays a single-char wildcard: a superset of the literal, safe.
		{"underscore value", AttrFilter{"k_a", AttrEq, "a_b"}, []string{`%"k_a"%`, "%a_b%"}},
	}
	for _, c := range cases {
		_, params := buildSearchWhere("s", from, to, SearchOptions{Attrs: []AttrFilter{c.f}}, true)
		got := params[3:]
		if len(got) != len(c.wantParams) {
			t.Errorf("%s: params %v, want %v", c.name, got, c.wantParams)
			continue
		}
		for i := range got {
			if got[i] != c.wantParams[i] {
				t.Errorf("%s: param %d = %v, want %v", c.name, i, got[i], c.wantParams[i])
			}
		}
	}
	// narrowing off: no attribute SQL at all.
	where, params := buildSearchWhere("s", from, to, SearchOptions{Attrs: []AttrFilter{{"k", AttrEq, "v"}}}, false)
	if strings.Contains(where, "attributes") || len(params) != 3 {
		t.Errorf("narrowing leaked when off: %s %v", where, params)
	}
}

func TestLikeLiteral(t *testing.T) {
	for _, s := range []string{"%", "a%b", `\`, `"`, "\n", "é"} {
		if _, ok := likeLiteral(s); ok {
			t.Errorf("%q should not be narrowable", s)
		}
	}
	for _, s := range []string{"GET", "a_b", "/api/v1", "x y", "a.b-c"} {
		if got, ok := likeLiteral(s); !ok || got != s {
			t.Errorf("%q: %q %v", s, got, ok)
		}
	}
}

func TestMatchAttrs(t *testing.T) {
	raw := `{"http.status_code":"500","http.route":"/api/users","empty":"","n":42,"b":true}`
	cases := []struct {
		f    AttrFilter
		want bool
	}{
		{AttrFilter{"http.status_code", AttrEq, "500"}, true},
		{AttrFilter{"http.status_code", AttrEq, "50"}, false},
		{AttrFilter{"http.status_code", AttrNeq, "200"}, true},
		{AttrFilter{"http.status_code", AttrNeq, "500"}, false},
		{AttrFilter{"missing", AttrNeq, "x"}, false}, // neq needs presence
		{AttrFilter{"missing", AttrExists, ""}, false},
		{AttrFilter{"empty", AttrExists, ""}, true},
		{AttrFilter{"http.route", AttrContains, "users"}, true},
		{AttrFilter{"http.route", AttrContains, "Users"}, false}, // case-sensitive
		{AttrFilter{"n", AttrEq, "42"}, true},
		{AttrFilter{"b", AttrEq, "true"}, true},
		{AttrFilter{"http.route", AttrEq, "%"}, false}, // value wildcards are literal in Go
		{AttrFilter{"http.route", AttrContains, "_"}, false},
	}
	for _, c := range cases {
		if got := matchAttrs(raw, []AttrFilter{c.f}); got != c.want {
			t.Errorf("%+v: got %v want %v", c.f, got, c.want)
		}
	}
	both := []AttrFilter{{"http.status_code", AttrEq, "500"}, {"http.route", AttrContains, "nope"}}
	if matchAttrs(raw, both) {
		t.Error("all filters must hold")
	}
	for _, bad := range []string{"", "not json", "[]", "null"} {
		if matchAttrs(bad, []AttrFilter{{"k", AttrExists, ""}}) {
			t.Errorf("%q matched", bad)
		}
	}
	if !matchAttrs("", nil) {
		t.Error("no filters matches everything")
	}
}

// --- search pipeline through the test seams ---

func tsAt(sec int64) time.Time { return time.Unix(sec, 0) }

func rootSpan(trace string, start int64, attrs string) Span {
	return Span{TraceID: trace, SpanID: trace + "-root", ServiceName: "svc", OperationName: "op",
		StartTime: tsAt(start), EndTime: tsAt(start + 1), DurationMs: 1000, StatusCode: "ok", Attributes: attrs}
}

func TestSearchTracesExAttributeVerifyAndPaging(t *testing.T) {
	q := &QueryService{}
	var gotSQL string
	var gotParams []any
	q.querySpans = func(_ context.Context, sql string, args ...any) ([]Span, error) {
		gotSQL, gotParams = sql, args
		// The LIKE stage is a superset; Go must drop the false positives.
		return []Span{
			rootSpan("t5", 50, `{"http.status_code":"500"}`),
			rootSpan("t4", 40, `{"http.status_code":"5000"}`), // contains "500" textually, not equal
			rootSpan("t3", 30, `{"http.status_code":"500"}`),
			rootSpan("t2", 20, ``),
			rootSpan("t1", 10, `{"http.status_code":"500"}`),
		}, nil
	}
	var countSQL string
	var countArgs []any
	q.queryCounts = func(_ context.Context, sql string, args ...any) ([]spanCountRow, error) {
		countSQL, countArgs = sql, args
		return []spanCountRow{{"t5", 7}, {"t3", 3}}, nil
	}
	o := SearchOptions{Limit: 2, Attrs: []AttrFilter{{"http.status_code", AttrEq, "500"}}}
	res, err := q.SearchTracesEx(context.Background(), "site", tsAt(0), tsAt(100), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Traces) != 2 || res.Traces[0].TraceID != "t5" || res.Traces[1].TraceID != "t3" {
		t.Fatalf("page = %+v", res.Traces)
	}
	if res.Truncated {
		t.Error("not truncated")
	}
	if res.Traces[0].SpanCount != 7 || res.Traces[1].SpanCount != 3 {
		t.Errorf("span counts = %d %d", res.Traces[0].SpanCount, res.Traces[1].SpanCount)
	}
	if !strings.Contains(gotSQL, "parent_span_id = ''") || !strings.Contains(gotSQL, "LIKE") ||
		!strings.Contains(gotSQL, fmt.Sprintf("LIMIT %d", searchCandidateCap+1)) {
		t.Errorf("candidate SQL: %s", gotSQL)
	}
	if len(gotParams) < 5 {
		t.Errorf("params %v", gotParams)
	}
	// The count aggregate is restricted to the page's ids, not the window.
	if !strings.Contains(countSQL, "trace_id IN ($4,$5)") || len(countArgs) != 5 ||
		countArgs[3] != "t5" || countArgs[4] != "t3" {
		t.Errorf("count SQL/args: %s %v", countSQL, countArgs)
	}

	// Second page.
	o.Offset = 2
	res, _ = q.SearchTracesEx(context.Background(), "site", tsAt(0), tsAt(100), o)
	if len(res.Traces) != 1 || res.Traces[0].TraceID != "t1" {
		t.Fatalf("page 2 = %+v", res.Traces)
	}
}

func TestSearchTracesExTruncatesAtCandidateCap(t *testing.T) {
	q := &QueryService{}
	q.querySpans = func(context.Context, string, ...any) ([]Span, error) {
		out := make([]Span, searchCandidateCap+1)
		for i := range out {
			out[i] = rootSpan(fmt.Sprintf("t%04d", i), int64(10000-i), `{"k":"v"}`)
		}
		return out, nil
	}
	q.queryCounts = func(context.Context, string, ...any) ([]spanCountRow, error) { return nil, nil }
	res, err := q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(20000),
		SearchOptions{Limit: 5, Attrs: []AttrFilter{{"k", AttrExists, ""}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncatedReason == "" || res.CandidateCap != searchCandidateCap {
		t.Fatalf("truncation not labeled: %+v", res)
	}
	if len(res.Traces) != 5 {
		t.Fatalf("page = %d", len(res.Traces))
	}
}

func TestSearchTracesExRejectsBadFilters(t *testing.T) {
	q := &QueryService{}
	_, err := q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(1),
		SearchOptions{Attrs: []AttrFilter{{"bad key", AttrEq, "v"}}})
	if err == nil {
		t.Fatal("bad key accepted")
	}
	_, err = q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(1),
		SearchOptions{Attrs: make5(AttrFilter{"k", AttrExists, ""}, MaxAttrFilters+1)})
	if err == nil {
		t.Fatal("too many filters accepted")
	}
}

func TestLegacySearchStaysRootOnlyAndSQLPaged(t *testing.T) {
	q := &QueryService{}
	var sql string
	q.querySummaries = func(_ context.Context, s string, _ ...any) ([]TraceSummary, error) {
		sql = s
		return []TraceSummary{{TraceID: "a"}, {TraceID: "b"}}, nil
	}
	var countSQL string
	q.queryCounts = func(_ context.Context, s string, _ ...any) ([]spanCountRow, error) {
		countSQL = s
		return []spanCountRow{{"a", 2}, {"b", 9}}, nil
	}
	out, err := q.SearchTraces(context.Background(), "s", tsAt(0), tsAt(10), "", "", "", 0, 0, 0, 0)
	if err != nil || len(out) != 2 || out[1].SpanCount != 9 {
		t.Fatalf("%v %+v", err, out)
	}
	if !strings.Contains(sql, "parent_span_id = ''") || !strings.Contains(sql, "LIMIT 20 OFFSET 0") || strings.Contains(sql, "attributes") {
		t.Errorf("legacy SQL changed: %s", sql)
	}
	if strings.Contains(countSQL, "FROM spans WHERE site_id = $1 AND start_time >= $2 AND start_time < $3 GROUP BY") ||
		!strings.Contains(countSQL, "trace_id IN") {
		t.Errorf("count aggregate not restricted to page ids: %s", countSQL)
	}
}

// --- orphans ---

func child(trace, id, parent string, start int64, status string) Span {
	return Span{TraceID: trace, SpanID: id, ParentSpanID: parent, ServiceName: "svc", OperationName: "op-" + id,
		StartTime: tsAt(start), EndTime: tsAt(start + 2), DurationMs: 2000, StatusCode: status}
}

func TestClassifyOrphan(t *testing.T) {
	// Missing root: earliest span's parent "r" is absent.
	spans := []Span{child("t", "b", "a", 12, "ok"), child("t", "a", "r", 10, "error")}
	sum, earliest, ok := classifyOrphan(spans)
	if !ok || !sum.RootMissing || sum.RootOp != "op-a" || earliest.SpanID != "a" ||
		sum.SpanCount != 2 || sum.StatusCode != "error" || sum.DurationMs != 4000 {
		t.Fatalf("%v %+v", ok, sum)
	}
	// A trace with a root is not an orphan.
	if _, _, ok := classifyOrphan([]Span{rootSpan("t", 1, ""), child("t", "c", "t-root", 2, "ok")}); ok {
		t.Error("trace with root classified as orphan")
	}
	// Earliest span's parent present in the trace: not a missing root.
	if _, _, ok := classifyOrphan([]Span{child("t", "a", "b", 10, "ok"), child("t", "b", "a", 11, "ok")}); ok {
		t.Error("cycle classified as orphan")
	}
	if _, _, ok := classifyOrphan(nil); ok {
		t.Error("empty trace")
	}
}

func TestSearchTracesExIncludeOrphans(t *testing.T) {
	q := &QueryService{}
	var traceSQL string
	var traceArgs []any
	q.querySpans = func(_ context.Context, sql string, args ...any) ([]Span, error) {
		switch {
		case strings.Contains(sql, "parent_span_id = ''"): // root candidates
			return []Span{rootSpan("rooted", 60, "")}, nil
		case strings.Contains(sql, "parent_span_id <> ''") && !strings.Contains(sql, "trace_id IN"):
			// orphan candidates: two orphan traces and one that has a root
			return []Span{{TraceID: "late"}, {TraceID: "late"}, {TraceID: "gone"}, {TraceID: "rooted"}}, nil
		default:
			traceSQL, traceArgs = sql, args
			return []Span{
				child("gone", "g1", "missing", 30, "ok"),
				child("late", "l1", "root-not-yet", 50, "ok"),
				child("late", "l2", "l1", 51, "ok"),
				rootSpan("rooted", 60, ""),
				child("rooted", "x", "rooted-root", 61, "ok"),
			}, nil
		}
	}
	q.queryCounts = func(context.Context, string, ...any) ([]spanCountRow, error) {
		return []spanCountRow{{"rooted", 2}}, nil
	}
	res, err := q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(100), SearchOptions{IncludeOrphans: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tr := range res.Traces {
		ids = append(ids, fmt.Sprintf("%s:%v", tr.TraceID, tr.RootMissing))
	}
	if strings.Join(ids, ",") != "rooted:false,late:true,gone:true" {
		t.Fatalf("traces = %v", ids)
	}
	if !strings.Contains(traceSQL, "trace_id IN ($4,$5,$6)") || !strings.Contains(traceSQL, fmt.Sprintf("LIMIT %d", orphanSpanCap+1)) {
		t.Errorf("trace span SQL: %s", traceSQL)
	}
	// The span-set load is time-bounded: window widened by orphanWindowSlack.
	if !strings.Contains(traceSQL, "start_time >= CAST($2 AS BIGINT) AND start_time < CAST($3 AS BIGINT)") || len(traceArgs) != 6 {
		t.Fatalf("trace span SQL not time-bounded: %s (%d args)", traceSQL, len(traceArgs))
	}
	if traceArgs[1] != dbutil.IntParam(tsAt(0).Add(-orphanWindowSlack).UnixMilli()) ||
		traceArgs[2] != dbutil.IntParam(tsAt(100).Add(orphanWindowSlack).UnixMilli()) {
		t.Errorf("window args = %v %v", traceArgs[1], traceArgs[2])
	}
	if !strings.Contains(traceSQL, "ORDER BY trace_id, CAST(start_time AS BIGINT) ASC") {
		t.Errorf("trace span SQL must order numerically: %s", traceSQL)
	}

	// Default mode never reports orphans.
	q.querySummaries = func(context.Context, string, ...any) ([]TraceSummary, error) {
		return []TraceSummary{{TraceID: "rooted"}}, nil
	}
	res, _ = q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(100), SearchOptions{Limit: 10})
	for _, tr := range res.Traces {
		if tr.RootMissing {
			t.Fatalf("orphan leaked into default mode: %+v", tr)
		}
	}

	// Filters apply to the orphan's earliest span.
	res, _ = q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(100), SearchOptions{IncludeOrphans: true, Limit: 10, Operation: "op-l1"})
	// (the stub returns the same root candidate regardless of SQL filters)
	var orphans []string
	for _, tr := range res.Traces {
		if tr.RootMissing {
			orphans = append(orphans, tr.TraceID)
		}
	}
	if strings.Join(orphans, ",") != "late" {
		t.Fatalf("filtered orphans = %v", orphans)
	}
}

func TestOrphanTraceCapIsLabeled(t *testing.T) {
	q := &QueryService{}
	q.querySpans = func(_ context.Context, sql string, _ ...any) ([]Span, error) {
		if strings.Contains(sql, "parent_span_id = ''") {
			return nil, nil
		}
		if !strings.Contains(sql, "trace_id IN") {
			out := make([]Span, orphanTraceCap+10)
			for i := range out {
				out[i] = Span{TraceID: fmt.Sprintf("t%03d", i)}
			}
			return out, nil
		}
		return nil, nil
	}
	res, err := q.SearchTracesEx(context.Background(), "s", tsAt(0), tsAt(100), SearchOptions{IncludeOrphans: true})
	if err != nil || !res.Truncated || res.TruncatedReason == "" {
		t.Fatalf("%v %+v", err, res)
	}
}

// --- O12 guard ---

func refusalOf(t *testing.T, err error) *queryguard.Refusal {
	t.Helper()
	var ref *queryguard.Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a *queryguard.Refusal, got %v", err)
	}
	return ref
}

func guardedTracing(rows int64, timeout time.Duration, l *queryguard.Limiter) *QueryService {
	b := queryguard.DefaultBudgets()
	b.MaxScanRows = rows
	b.Timeout = timeout
	return (&QueryService{}).WithQueryGuard(l, b)
}

// Concurrency admission refuses before any engine access (the service has no
// db, so reaching a query would panic).
func TestTraceReadsConcurrencyRefusal(t *testing.T) {
	l := queryguard.NewLimiter(1, 1)
	hold, err := l.Acquire(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	q := guardedTracing(100, time.Second, l)
	ctx := context.Background()
	from, to := tsAt(0), tsAt(100)

	if _, err := q.ListServices(ctx, "site", from, to); refusalOf(t, err).Status != 429 {
		t.Errorf("ListServices not refused: %v", err)
	}
	if _, err := q.ListOperations(ctx, "site", "svc", from, to); refusalOf(t, err).Status != 429 {
		t.Errorf("ListOperations not refused: %v", err)
	}
	if _, err := q.SearchTraces(ctx, "site", from, to, "", "", "", 0, 0, 10, 0); refusalOf(t, err).Status != 429 {
		t.Errorf("SearchTraces not refused: %v", err)
	}
	if _, err := q.SearchTracesEx(ctx, "site", from, to, SearchOptions{IncludeOrphans: true}); refusalOf(t, err).Status != 429 {
		t.Errorf("SearchTracesEx not refused: %v", err)
	}
	if _, err := q.GetTrace(ctx, "trace", "site"); refusalOf(t, err).Status != 429 {
		t.Errorf("GetTrace not refused: %v", err)
	}
	if _, err := q.ServiceDependencies(ctx, "site", from, to); refusalOf(t, err).Status != 429 {
		t.Errorf("ServiceDependencies not refused: %v", err)
	}
}

// A page reaching past the row budget is refused, not shortened.
func TestSearchTracesPageBeyondBudgetRefuses(t *testing.T) {
	q := guardedTracing(100, time.Second, nil) // nil db: must refuse before querying
	for _, c := range []struct{ limit, offset int }{{50, 51}, {10, 1 << 40}} {
		_, err := q.SearchTracesEx(context.Background(), "site", tsAt(0), tsAt(100), SearchOptions{Limit: c.limit, Offset: c.offset})
		if ref := refusalOf(t, err); ref.Code != queryguard.CodeBudgetRows || ref.Status != 429 {
			t.Fatalf("limit=%d offset=%d: %+v", c.limit, c.offset, ref)
		}
	}
}

func TestSearchTracesTimeBudgetIs504(t *testing.T) {
	q := guardedTracing(1000, 20*time.Millisecond, nil)
	q.querySummaries = func(ctx context.Context, _ string, _ ...any) ([]TraceSummary, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := q.SearchTraces(context.Background(), "site", tsAt(0), tsAt(100), "", "", "", 0, 0, 10, 0)
	if ref := refusalOf(t, err); ref.Code != queryguard.CodeBudgetTime || ref.Status != 504 {
		t.Fatalf("refusal = %+v", ref)
	}
	// The verified path honors the same budget.
	q.querySpans = func(ctx context.Context, _ string, _ ...any) ([]Span, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err = q.SearchTracesEx(context.Background(), "site", tsAt(0), tsAt(100),
		SearchOptions{Attrs: []AttrFilter{{"k", AttrExists, ""}}})
	if ref := refusalOf(t, err); ref.Status != 504 {
		t.Fatalf("refusal = %+v", ref)
	}
}

// A caller that cancels is not mislabeled as a time-budget refusal.
func TestSearchTracesCallerCancelPropagates(t *testing.T) {
	q := guardedTracing(1000, time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	q.querySummaries = func(c context.Context, _ string, _ ...any) ([]TraceSummary, error) {
		cancel()
		return nil, context.Canceled
	}
	_, err := q.SearchTraces(ctx, "site", tsAt(0), tsAt(100), "", "", "", 0, 0, 10, 0)
	var ref *queryguard.Refusal
	if errors.As(err, &ref) {
		t.Fatalf("cancellation reported as refusal: %v", err)
	}
}
