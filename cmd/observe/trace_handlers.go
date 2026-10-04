package main

// Trace query handlers, moved out of main.go so the O12 admission mapping and
// the attribute/orphan search live in one file. Behavior of the original
// handlers is unchanged except that query-guard refusals now keep their
// labeled 429/504 problem response (guardmap.HTTPError).

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/guardmap"
	"github.com/useteploy/teploy-observe/internal/tracing"
)

type listServicesInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func listServicesHandler(svc *tracing.QueryService) neutron.HandlerFunc[listServicesInput, []tracing.ServiceSummary] {
	return func(ctx context.Context, input listServicesInput) ([]tracing.ServiceSummary, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to, err := parseTimeRange(input.From, input.To)
		if err != nil {
			return nil, neutron.ErrBadRequest(err.Error())
		}
		rows, err := svc.ListServices(ctx, input.SiteID, from, to)
		return emptyOnNil(rows, guardmap.HTTPError(err))
	}
}

type listOpsInput struct {
	SiteID  string `query:"site_id"`
	Service string `path:"service"`
	From    string `query:"from"`
	To      string `query:"to"`
}

func listOperationsHandler(svc *tracing.QueryService) neutron.HandlerFunc[listOpsInput, []tracing.OperationSummary] {
	return func(ctx context.Context, input listOpsInput) ([]tracing.OperationSummary, error) {
		if input.SiteID == "" || input.Service == "" {
			return nil, neutron.ErrBadRequest("site_id and service required")
		}
		from, to, err := parseTimeRange(input.From, input.To)
		if err != nil {
			return nil, neutron.ErrBadRequest(err.Error())
		}
		rows, err := svc.ListOperations(ctx, input.SiteID, input.Service, from, to)
		return emptyOnNil(rows, guardmap.HTTPError(err))
	}
}

type searchTracesInput struct {
	SiteID      string `query:"site_id"`
	From        string `query:"from"`
	To          string `query:"to"`
	Service     string `query:"service"`
	Operation   string `query:"operation"`
	Offset      int    `query:"offset"`
	Status      string `query:"status"`
	MinDuration int64  `query:"min_duration"`
	MaxDuration int64  `query:"max_duration"`
	Limit       int    `query:"limit"`
}

// searchTracesHandler is the original root-only search. Attribute filters and
// the orphan mode are on searchTracesAdvancedHandler; this route ignores them
// (callers that need them use /search-advanced).
func searchTracesHandler(svc *tracing.QueryService) neutron.HandlerFunc[searchTracesInput, []tracing.TraceSummary] {
	return func(ctx context.Context, input searchTracesInput) ([]tracing.TraceSummary, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to, err := parseTimeRange(input.From, input.To)
		if err != nil {
			return nil, neutron.ErrBadRequest(err.Error())
		}
		rows, err := svc.SearchTraces(ctx, input.SiteID, from, to, input.Service, input.Operation, input.Status, input.MinDuration, input.MaxDuration, input.Limit, input.Offset)
		return emptyOnNil(rows, guardmap.HTTPError(err))
	}
}

type getTraceInput struct {
	TraceID string `path:"trace_id"`
	SiteID  string `query:"site_id"`
}

func getTraceHandler(svc *tracing.QueryService) neutron.HandlerFunc[getTraceInput, []tracing.Span] {
	return func(ctx context.Context, input getTraceInput) ([]tracing.Span, error) {
		if input.SiteID == "" || input.TraceID == "" {
			return nil, neutron.ErrBadRequest("site_id and trace_id required")
		}
		rows, err := svc.GetTrace(ctx, input.TraceID, input.SiteID)
		return emptyOnNil(rows, guardmap.HTTPError(err))
	}
}

type traceErrorsInput struct {
	TraceID string `path:"trace_id"`
	SiteID  string `query:"site_id"`
}

func traceErrorsHandler(svc *tracing.QueryService) neutron.HandlerFunc[traceErrorsInput, []tracing.TraceErrorHit] {
	return func(ctx context.Context, input traceErrorsInput) ([]tracing.TraceErrorHit, error) {
		if input.SiteID == "" || input.TraceID == "" {
			return nil, neutron.ErrBadRequest("site_id and trace_id required")
		}
		hits, err := svc.TraceErrors(ctx, input.TraceID, input.SiteID)
		if err != nil {
			return nil, err
		}
		if hits == nil {
			hits = []tracing.TraceErrorHit{}
		}
		return hits, nil
	}
}

type serviceDepsInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func serviceDepsHandler(svc *tracing.QueryService) neutron.HandlerFunc[serviceDepsInput, []tracing.Dependency] {
	return func(ctx context.Context, input serviceDepsInput) ([]tracing.Dependency, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to, err := parseTimeRange(input.From, input.To)
		if err != nil {
			return nil, neutron.ErrBadRequest(err.Error())
		}
		rows, err := svc.ServiceDependencies(ctx, input.SiteID, from, to)
		return emptyOnNil(rows, guardmap.HTTPError(err))
	}
}

// RegisterTraceSearchRoutes wires GET /search-advanced onto the trace group
// (JWT). It is an untyped handler because the typed binder reads only the
// first value of a query key and comma-splits slices, which would corrupt
// repeated attr= parameters whose values may contain commas.
func RegisterTraceSearchRoutes(r *neutron.Router, svc *tracing.QueryService) {
	r.Handle("GET /search-advanced", searchTracesAdvancedHandler(svc))
}

// traceSearchRequest is a validated /search-advanced request.
type traceSearchRequest struct {
	siteID   string
	from, to time.Time
	opts     tracing.SearchOptions
}

// parseTraceSearch validates the query string. Every failure is a client
// error (400); nothing here touches the database.
//
//	attr             repeated key:op[:value], op in eq|neq|exists|contains
//	http_status_code first-class shortcut for attr=http.status_code:eq:<n>
//	http_method      first-class shortcut for attr=http.method:eq:<METHOD>
//	include_orphans  true to also list traces whose root span is missing
//
// At most tracing.MaxAttrFilters filters in total (shortcuts included).
func parseTraceSearch(q url.Values) (traceSearchRequest, error) {
	var req traceSearchRequest
	req.siteID = q.Get("site_id")
	if req.siteID == "" {
		return req, neutron.ErrBadRequest("site_id required")
	}
	from, to, err := parseTimeRange(q.Get("from"), q.Get("to"))
	if err != nil {
		return req, neutron.ErrBadRequest(err.Error())
	}
	req.from, req.to = from, to

	o := &req.opts
	o.Service, o.Operation, o.Status = q.Get("service"), q.Get("operation"), q.Get("status")
	for _, f := range []struct {
		name string
		dst  *int64
	}{{"min_duration", &o.MinDuration}, {"max_duration", &o.MaxDuration}} {
		if v := q.Get(f.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return req, neutron.ErrBadRequest(f.name + " must be a non-negative integer")
			}
			*f.dst = n
		}
	}
	for _, f := range []struct {
		name string
		dst  *int
	}{{"limit", &o.Limit}, {"offset", &o.Offset}} {
		if v := q.Get(f.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return req, neutron.ErrBadRequest(f.name + " must be a non-negative integer")
			}
			*f.dst = n
		}
	}
	if v := q.Get("include_orphans"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return req, neutron.ErrBadRequest("include_orphans must be true or false")
		}
		o.IncludeOrphans = b
	}

	raw := q["attr"]
	shortcuts := 0
	if q.Get("http_status_code") != "" {
		shortcuts++
	}
	if q.Get("http_method") != "" {
		shortcuts++
	}
	// Count before parsing so an oversized request is refused cheaply.
	if len(raw)+shortcuts > tracing.MaxAttrFilters {
		return req, neutron.ErrBadRequest("at most " + strconv.Itoa(tracing.MaxAttrFilters) + " attribute filters are allowed")
	}
	for _, s := range raw {
		f, err := tracing.ParseAttrFilter(s)
		if err != nil {
			return req, neutron.ErrBadRequest(err.Error())
		}
		o.Attrs = append(o.Attrs, f)
	}
	if v := q.Get("http_status_code"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 || n > 599 {
			return req, neutron.ErrBadRequest("http_status_code must be a status code between 100 and 599")
		}
		o.Attrs = append(o.Attrs, tracing.AttrFilter{Key: "http.status_code", Op: tracing.AttrEq, Value: strconv.Itoa(n)})
	}
	if v := q.Get("http_method"); v != "" {
		v = strings.ToUpper(v)
		if len(v) > 16 {
			return req, neutron.ErrBadRequest("http_method is not a valid HTTP method")
		}
		for _, c := range v {
			if c < 'A' || c > 'Z' {
				return req, neutron.ErrBadRequest("http_method is not a valid HTTP method")
			}
		}
		o.Attrs = append(o.Attrs, tracing.AttrFilter{Key: "http.method", Op: tracing.AttrEq, Value: v})
	}
	if err := tracing.ValidateAttrFilters(o.Attrs); err != nil {
		return req, neutron.ErrBadRequest(err.Error())
	}
	return req, nil
}

func searchTracesAdvancedHandler(svc *tracing.QueryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := parseTraceSearch(r.URL.Query())
		if err != nil {
			neutron.WriteError(w, r, err)
			return
		}
		res, err := svc.SearchTracesEx(r.Context(), req.siteID, req.from, req.to, req.opts)
		if err != nil {
			// Refusals keep their labeled 429/504; anything else keeps the
			// framework's problem mapping.
			neutron.WriteError(w, r, guardmap.HTTPError(err))
			return
		}
		neutron.JSON(w, http.StatusOK, res)
	}
}
