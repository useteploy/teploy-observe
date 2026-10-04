package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/queryguard"
	"github.com/useteploy/teploy-observe/internal/tracing"
)

func TestParseTraceSearchValidation(t *testing.T) {
	good := func(extra url.Values) url.Values {
		v := url.Values{"site_id": {"s1"}}
		for k, vs := range extra {
			v[k] = vs
		}
		return v
	}
	bad := map[string]url.Values{
		"missing site":     {},
		"bad op":           good(url.Values{"attr": {"k:regex:x"}}),
		"bad key":          good(url.Values{"attr": {"a b:eq:x"}}),
		"sql key":          good(url.Values{"attr": {"a';DROP TABLE spans;--:eq:x"}}),
		"eq without value": good(url.Values{"attr": {"k:eq"}}),
		"no op":            good(url.Values{"attr": {"k"}}),
		"too many": good(url.Values{"attr": {
			"a:exists", "b:exists", "c:exists", "d:exists", "e:exists", "f:exists"}}),
		"too many with shortcuts": good(url.Values{
			"attr":             {"a:exists", "b:exists", "c:exists", "d:exists"},
			"http_status_code": {"500"}, "http_method": {"GET"}}),
		"bad status shortcut": good(url.Values{"http_status_code": {"99"}}),
		"status injection":    good(url.Values{"http_status_code": {"500 OR 1=1"}}),
		"bad method":          good(url.Values{"http_method": {"GET;--"}}),
		"bad orphans flag":    good(url.Values{"include_orphans": {"maybe"}}),
		"bad limit":           good(url.Values{"limit": {"-1"}}),
		"bad min duration":    good(url.Values{"min_duration": {"x"}}),
		"bad time":            good(url.Values{"from": {"yesterday"}}),
	}
	for name, v := range bad {
		_, err := parseTraceSearch(v)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		var app *neutron.AppError
		if !errors.As(err, &app) || app.Status != http.StatusBadRequest {
			t.Errorf("%s: not a 400: %v", name, err)
		}
	}

	req, err := parseTraceSearch(good(url.Values{
		"attr":             {"db.system:eq:postgres", "http.route:contains:/a,b"},
		"http_status_code": {"503"},
		"http_method":      {"get"},
		"include_orphans":  {"true"},
		"limit":            {"25"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	o := req.opts
	if !o.IncludeOrphans || o.Limit != 25 || len(o.Attrs) != 4 {
		t.Fatalf("opts = %+v", o)
	}
	// Repeated attr values keep their commas (the typed binder would split).
	if o.Attrs[1].Value != "/a,b" {
		t.Errorf("comma value mangled: %+v", o.Attrs[1])
	}
	if o.Attrs[2] != (tracing.AttrFilter{Key: "http.status_code", Op: tracing.AttrEq, Value: "503"}) ||
		o.Attrs[3] != (tracing.AttrFilter{Key: "http.method", Op: tracing.AttrEq, Value: "GET"}) {
		t.Errorf("shortcuts = %+v %+v", o.Attrs[2], o.Attrs[3])
	}

	// Exactly at the bound is fine.
	if _, err := parseTraceSearch(good(url.Values{"attr": {"a:exists", "b:exists", "c:exists", "d:exists", "e:exists"}})); err != nil {
		t.Errorf("five filters refused: %v", err)
	}
}

// heldTraceSvc returns a guarded service whose only concurrency slot is taken,
// so any read is refused before it could reach the (nil) database.
func heldTraceSvc(t *testing.T) (*tracing.QueryService, func()) {
	t.Helper()
	l := queryguard.NewLimiter(1, 1)
	rel, err := l.Acquire(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	return tracing.NewQueryService(nil).WithQueryGuard(l, queryguard.DefaultBudgets()), rel
}

// A saturated limiter comes back as the labeled 429, not a bare 500.
func TestAdvancedSearchGuardRefusalIs429(t *testing.T) {
	svc, release := heldTraceSvc(t)
	defer release()
	for _, qs := range []string{"site_id=s1", "site_id=s1&include_orphans=true", "site_id=s1&attr=k:exists"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/traces/search-advanced?"+qs, nil)
		searchTracesAdvancedHandler(svc)(w, r)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("%s: status %d body %s", qs, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "query_concurrency") {
			t.Errorf("%s: unlabeled body %s", qs, w.Body.String())
		}
	}
}

func TestAdvancedSearchBadParamsAre400(t *testing.T) {
	svc, release := heldTraceSvc(t)
	defer release()
	for _, qs := range []string{
		"site_id=s1&attr=k:nope:x",
		"site_id=s1&attr=" + url.QueryEscape("a b:eq:x"),
		"site_id=s1&attr=a:exists&attr=b:exists&attr=c:exists&attr=d:exists&attr=e:exists&attr=f:exists",
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/traces/search-advanced?"+qs, nil)
		searchTracesAdvancedHandler(svc)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s", qs, w.Code, w.Body.String())
		}
	}
}

// The typed trace reads map refusals to a 429 AppError too.
func TestTypedTraceHandlersMapRefusals(t *testing.T) {
	svc, release := heldTraceSvc(t)
	defer release()
	ctx := context.Background()
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	errs := map[string]error{}
	_, errs["services"] = listServicesHandler(svc)(ctx, listServicesInput{SiteID: "s1"})
	_, errs["operations"] = listOperationsHandler(svc)(ctx, listOpsInput{SiteID: "s1", Service: "x"})
	_, errs["search"] = searchTracesHandler(svc)(ctx, searchTracesInput{SiteID: "s1"})
	_, errs["trace"] = getTraceHandler(svc)(ctx, getTraceInput{SiteID: "s1", TraceID: "t"})
	_, errs["deps"] = serviceDepsHandler(svc)(ctx, serviceDepsInput{SiteID: "s1", From: from})
	for name, err := range errs {
		var app *neutron.AppError
		if !errors.As(err, &app) || app.Status != http.StatusTooManyRequests {
			t.Errorf("%s: want 429 AppError, got %v", name, err)
		}
	}
}

// The advanced route must coexist with the typed trace routes (/{trace_id},
// /{trace_id}/errors, /dependencies, /search) without a pattern collision.
func TestTraceSearchRouteRegistersAlongsideTraceRoutes(t *testing.T) {
	r := neutron.New().Router()
	g := r.Group("/api/v1/traces")
	svc := tracing.NewQueryService(nil)
	neutron.Get(g, "/search", searchTracesHandler(svc))
	neutron.Get(g, "/{trace_id}", getTraceHandler(svc))
	neutron.Get(g, "/{trace_id}/errors", traceErrorsHandler(svc))
	neutron.Get(g, "/dependencies", serviceDepsHandler(svc))
	RegisterTraceSearchRoutes(g, svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/traces/search-advanced?attr=a+b:eq:x", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "site_id") {
		t.Fatalf("advanced route not reached: %d %s", w.Code, w.Body.String())
	}
}
