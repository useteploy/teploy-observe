// Package seed populates demo data for empty tables on first boot.
// Each table is seeded independently and only if it has no rows for site_id="default",
// so partial state is handled gracefully.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/logs"
	"github.com/useteploy/teploy-observe/internal/replays"
	"github.com/useteploy/teploy-observe/internal/tracing"
)

const defaultSiteID = "default"

// Run populates demo data for any empty tables tied to the default site.
// Safe to call on every boot — each seeding step is a no-op if data exists.
func Run(ctx context.Context, db *nucleus.Client, logger *slog.Logger) {
	steps := []struct {
		name string
		fn   func(context.Context, *nucleus.Client) error
	}{
		{"logs", seedLogs},
		{"traces", seedTraces},
		{"replays", seedReplays},
	}

	for _, s := range steps {
		count, err := countRows(ctx, db, s.name)
		if err != nil {
			logger.Warn("seed: count failed", "table", s.name, "err", err)
			continue
		}
		if count > 0 {
			continue
		}
		if err := s.fn(ctx, db); err != nil {
			logger.Warn("seed: step failed", "step", s.name, "err", err)
			continue
		}
		logger.Info("seed: populated", "table", s.name)
	}
}

func countRows(ctx context.Context, db *nucleus.Client, table string) (int64, error) {
	type row struct {
		Count int64 `db:"count"`
	}
	var query string
	switch table {
	case "logs":
		query = `SELECT COUNT(*) AS count FROM logs WHERE site_id = $1`
	case "traces":
		query = `SELECT COUNT(*) AS count FROM spans WHERE site_id = $1`
	case "replays":
		query = `SELECT COUNT(*) AS count FROM replay_sessions WHERE site_id = $1`
	default:
		return 0, fmt.Errorf("unknown table: %s", table)
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(), query, defaultSiteID)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Count, nil
}

func seedLogs(ctx context.Context, db *nucleus.Client) error {
	svc := logs.NewLogService(db)
	rng := rand.New(rand.NewSource(1))

	services := []string{"api", "worker", "web"}
	levels := []string{"info", "info", "info", "info", "warn", "warn", "error", "debug"}
	messages := map[string][]string{
		"info": {
			"request completed",
			"cache hit for user profile",
			"job processed successfully",
			"user logged in",
			"email queued for delivery",
		},
		"warn": {
			"slow database query",
			"retry attempt 2 of 3",
			"rate limit approaching",
		},
		"error": {
			"failed to connect to redis",
			"database timeout",
			"payment gateway unreachable",
		},
		"debug": {
			"cache lookup: hit=true key=user:42",
			"request headers parsed",
		},
	}

	now := time.Now().UTC()
	for i := 0; i < 120; i++ {
		level := levels[rng.Intn(len(levels))]
		msgs := messages[level]
		input := logs.LogInput{
			SiteID:      defaultSiteID,
			Level:       level,
			Message:     msgs[rng.Intn(len(msgs))],
			ServiceName: services[rng.Intn(len(services))],
			Attributes: map[string]any{
				"duration_ms": rng.Intn(500),
				"user_id":     fmt.Sprintf("u_%d", rng.Intn(100)),
			},
		}
		// Spread across the last 24 hours.
		offset := time.Duration(rng.Intn(24*60)) * time.Minute
		ts := now.Add(-offset).UnixMilli()
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO logs (log_id, tenant_id, site_id, timestamp, level, message, service_name, trace_id, span_id, attributes)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6, '', '', $7)`,
			genID(), input.SiteID, strconv.FormatInt(ts, 10),
			input.Level, input.Message, input.ServiceName,
			mapToJSON(input.Attributes),
		)
		if err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
		_ = svc // kept for future if we switch to service-level ingest
	}
	return nil
}

func seedTraces(ctx context.Context, db *nucleus.Client) error {
	ingest := tracing.NewIngestService(db)
	rng := rand.New(rand.NewSource(2))

	type opSpec struct {
		service, operation string
		minMs, maxMs       int
	}
	traces := []struct {
		rootService, rootOp string
		children            []opSpec
		errorChance         float64
	}{
		{"api", "GET /users/:id", []opSpec{
			{"api", "db.query users", 5, 30},
			{"api", "cache.get", 1, 5},
		}, 0.05},
		{"api", "POST /checkout", []opSpec{
			{"api", "stripe.charge", 80, 400},
			{"api", "db.insert order", 10, 40},
			{"worker", "email.send", 20, 100},
		}, 0.15},
		{"web", "GET /dashboard", []opSpec{
			{"api", "GET /users/:id", 20, 80},
			{"api", "GET /stats", 40, 120},
		}, 0.02},
		{"worker", "job.reportGenerator", []opSpec{
			{"worker", "db.query events", 100, 400},
			{"worker", "s3.upload", 50, 200},
		}, 0.08},
		{"api", "DELETE /session", []opSpec{
			{"api", "cache.invalidate", 1, 8},
			{"api", "db.delete", 5, 25},
		}, 0.0},
	}

	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		t := traces[rng.Intn(len(traces))]
		startNs := now.Add(-time.Duration(rng.Intn(12*60)) * time.Minute).UnixNano()
		traceID := genID() + genID()
		rootSpanID := genID()[:16]

		var spans []tracing.OTLPSpan
		rootDur := 20 + rng.Intn(200)
		for _, c := range t.children {
			rootDur += c.minMs + rng.Intn(c.maxMs-c.minMs+1)
		}
		errored := rng.Float64() < t.errorChance
		statusCode := 0
		if errored {
			statusCode = 2
		}

		spans = append(spans, tracing.OTLPSpan{
			TraceID:           traceID,
			SpanID:            rootSpanID,
			ParentSpanID:      "",
			Name:              t.rootOp,
			Kind:              2,
			StartTimeUnixNano: strconv.FormatInt(startNs, 10),
			EndTimeUnixNano:   strconv.FormatInt(startNs+int64(rootDur)*1_000_000, 10),
			Status:            tracing.SpanStatus{Code: statusCode},
		})

		childStart := startNs + 2_000_000
		for _, c := range t.children {
			dur := c.minMs + rng.Intn(c.maxMs-c.minMs+1)
			spanID := genID()[:16]
			spans = append(spans, tracing.OTLPSpan{
				TraceID:           traceID,
				SpanID:            spanID,
				ParentSpanID:      rootSpanID,
				Name:              c.operation,
				Kind:              3,
				StartTimeUnixNano: strconv.FormatInt(childStart, 10),
				EndTimeUnixNano:   strconv.FormatInt(childStart+int64(dur)*1_000_000, 10),
				Attributes: []tracing.KeyValue{
					{Key: "service.name", Value: tracing.AnyValue{StringValue: c.service}},
				},
			})
			childStart += int64(dur) * 1_000_000
		}

		req := tracing.ExportTraceRequest{
			ResourceSpans: []tracing.ResourceSpans{{
				Resource: tracing.Resource{
					Attributes: []tracing.KeyValue{
						{Key: "service.name", Value: tracing.AnyValue{StringValue: t.rootService}},
					},
				},
				ScopeSpans: []tracing.ScopeSpans{{Spans: spans}},
			}},
		}
		// IngestSync (vs Ingest) blocks on the rollup write so a fresh
		// install shows Services + ServiceMap data the moment the HTTP
		// server is ready, instead of racing the background goroutine.
		if _, err := ingest.IngestSync(ctx, defaultSiteID, req); err != nil {
			return fmt.Errorf("ingest trace %d: %w", i, err)
		}
	}

	// Synthesize one trace per perf-issue detector so a fresh install shows
	// real findings on the Performance tab. Each helper builds an OTLP
	// envelope with exactly the span shape that detector looks for.
	for _, build := range []func() tracing.ExportTraceRequest{
		buildNPlusOneTrace,
		buildSlowDBTrace,
		buildConsecutiveDBTrace,
		buildSlowHTTPTrace,
	} {
		if _, err := ingest.IngestSync(ctx, defaultSiteID, build()); err != nil {
			return fmt.Errorf("ingest perf-detector trace: %w", err)
		}
	}
	return nil
}

// buildNPlusOneTrace creates a parent + 5 sibling DB spans whose statements
// share a fingerprint. NPlusOneDB needs >=4 to fire, 5 keeps it well above.
func buildNPlusOneTrace() tracing.ExportTraceRequest {
	startNs := time.Now().UTC().Add(-5 * time.Minute).UnixNano()
	traceID := genID() + genID()
	parent := genID()[:16]

	spans := []tracing.OTLPSpan{{
		TraceID: traceID, SpanID: parent, Name: "GET /users-with-orders", Kind: 2,
		StartTimeUnixNano: strconv.FormatInt(startNs, 10),
		EndTimeUnixNano:   strconv.FormatInt(startNs+250_000_000, 10),
	}}
	cursor := startNs + 5_000_000
	for i := 0; i < 5; i++ {
		dur := int64(20_000_000)
		spans = append(spans, tracing.OTLPSpan{
			TraceID: traceID, SpanID: genID()[:16], ParentSpanID: parent,
			Name: "db.query", Kind: 3,
			StartTimeUnixNano: strconv.FormatInt(cursor, 10),
			EndTimeUnixNano:   strconv.FormatInt(cursor+dur, 10),
			Attributes: []tracing.KeyValue{
				{Key: "service.name", Value: tracing.AnyValue{StringValue: "api"}},
				{Key: "db.system", Value: tracing.AnyValue{StringValue: "postgres"}},
				{Key: "db.statement", Value: tracing.AnyValue{StringValue: fmt.Sprintf("SELECT * FROM users WHERE id = %d", i+1)}},
			},
		})
		cursor += dur
	}
	return wrapTrace("api", spans)
}

// buildSlowDBTrace creates a single 1.5s DB span — well above the 1s
// SlowDBQuery threshold.
func buildSlowDBTrace() tracing.ExportTraceRequest {
	startNs := time.Now().UTC().Add(-4 * time.Minute).UnixNano()
	traceID := genID() + genID()
	parent := genID()[:16]
	dbDur := int64(1_500_000_000)

	spans := []tracing.OTLPSpan{
		{
			TraceID: traceID, SpanID: parent, Name: "GET /report", Kind: 2,
			StartTimeUnixNano: strconv.FormatInt(startNs, 10),
			EndTimeUnixNano:   strconv.FormatInt(startNs+dbDur+10_000_000, 10),
		},
		{
			TraceID: traceID, SpanID: genID()[:16], ParentSpanID: parent,
			Name: "db.query", Kind: 3,
			StartTimeUnixNano: strconv.FormatInt(startNs+1_000_000, 10),
			EndTimeUnixNano:   strconv.FormatInt(startNs+1_000_000+dbDur, 10),
			Attributes: []tracing.KeyValue{
				{Key: "service.name", Value: tracing.AnyValue{StringValue: "api"}},
				{Key: "db.system", Value: tracing.AnyValue{StringValue: "postgres"}},
				{Key: "db.statement", Value: tracing.AnyValue{StringValue: "SELECT * FROM events JOIN users ON events.user_id = users.id WHERE created_at > NOW() - INTERVAL '30 days'"}},
			},
		},
	}
	return wrapTrace("api", spans)
}

// buildConsecutiveDBTrace creates 3 serially-executed sibling DB spans
// totalling >100ms — meets the ConsecutiveDB threshold without the same
// fingerprint (so it doesn't double-fire as N+1).
func buildConsecutiveDBTrace() tracing.ExportTraceRequest {
	startNs := time.Now().UTC().Add(-3 * time.Minute).UnixNano()
	traceID := genID() + genID()
	parent := genID()[:16]

	spans := []tracing.OTLPSpan{{
		TraceID: traceID, SpanID: parent, Name: "GET /dashboard-load", Kind: 2,
		StartTimeUnixNano: strconv.FormatInt(startNs, 10),
		EndTimeUnixNano:   strconv.FormatInt(startNs+200_000_000, 10),
	}}
	statements := []string{
		"SELECT * FROM users WHERE org_id = 42",
		"SELECT * FROM orders WHERE org_id = 42",
		"SELECT * FROM payments WHERE org_id = 42",
	}
	cursor := startNs + 5_000_000
	for _, stmt := range statements {
		dur := int64(50_000_000) // 50ms each → 150ms total > 100ms floor
		spans = append(spans, tracing.OTLPSpan{
			TraceID: traceID, SpanID: genID()[:16], ParentSpanID: parent,
			Name: "db.query", Kind: 3,
			StartTimeUnixNano: strconv.FormatInt(cursor, 10),
			EndTimeUnixNano:   strconv.FormatInt(cursor+dur, 10),
			Attributes: []tracing.KeyValue{
				{Key: "service.name", Value: tracing.AnyValue{StringValue: "api"}},
				{Key: "db.system", Value: tracing.AnyValue{StringValue: "postgres"}},
				{Key: "db.statement", Value: tracing.AnyValue{StringValue: stmt}},
			},
		})
		cursor += dur
	}
	return wrapTrace("api", spans)
}

// buildSlowHTTPTrace creates a single 5s outbound HTTP client span — above
// the 3s SlowHTTPCall threshold.
func buildSlowHTTPTrace() tracing.ExportTraceRequest {
	startNs := time.Now().UTC().Add(-2 * time.Minute).UnixNano()
	traceID := genID() + genID()
	parent := genID()[:16]
	dur := int64(5_000_000_000)

	spans := []tracing.OTLPSpan{
		{
			TraceID: traceID, SpanID: parent, Name: "POST /webhook-fanout", Kind: 2,
			StartTimeUnixNano: strconv.FormatInt(startNs, 10),
			EndTimeUnixNano:   strconv.FormatInt(startNs+dur+10_000_000, 10),
		},
		{
			TraceID: traceID, SpanID: genID()[:16], ParentSpanID: parent,
			Name: "HTTP POST", Kind: 3,
			StartTimeUnixNano: strconv.FormatInt(startNs+1_000_000, 10),
			EndTimeUnixNano:   strconv.FormatInt(startNs+1_000_000+dur, 10),
			Attributes: []tracing.KeyValue{
				{Key: "service.name", Value: tracing.AnyValue{StringValue: "api"}},
				{Key: "http.url", Value: tracing.AnyValue{StringValue: "https://hooks.example.com/notify"}},
				{Key: "http.method", Value: tracing.AnyValue{StringValue: "POST"}},
			},
		},
	}
	return wrapTrace("api", spans)
}

func wrapTrace(rootService string, spans []tracing.OTLPSpan) tracing.ExportTraceRequest {
	return tracing.ExportTraceRequest{
		ResourceSpans: []tracing.ResourceSpans{{
			Resource: tracing.Resource{
				Attributes: []tracing.KeyValue{
					{Key: "service.name", Value: tracing.AnyValue{StringValue: rootService}},
				},
			},
			ScopeSpans: []tracing.ScopeSpans{{Spans: spans}},
		}},
	}
}

func seedReplays(ctx context.Context, db *nucleus.Client) error {
	svc := replays.NewReplayService(db)
	rng := rand.New(rand.NewSource(3))

	browsers := []string{"Chrome", "Firefox", "Safari"}
	osList := []string{"macOS", "Windows", "Linux"}
	urls := []string{"https://demo.local/", "https://demo.local/dashboard", "https://demo.local/pricing"}

	now := time.Now().UTC().UnixMilli()
	for i := 0; i < 3; i++ {
		sessionID := genID()
		startMs := now - int64(rng.Intn(24*60))*60_000

		events := []struct {
			Type      string `json:"type"`
			Timestamp int64  `json:"timestamp"`
			Data      any    `json:"data"`
		}{}

		eventCount := 25 + rng.Intn(40)
		tsCursor := startMs
		events = append(events, struct {
			Type      string `json:"type"`
			Timestamp int64  `json:"timestamp"`
			Data      any    `json:"data"`
		}{"snapshot", tsCursor, map[string]any{"html": "<body><div class=\"demo\">seed snapshot</div></body>"}})
		for j := 1; j < eventCount; j++ {
			tsCursor += int64(200 + rng.Intn(2000))
			kind := []string{"mousemove", "click", "input", "scroll", "mutation"}[rng.Intn(5)]
			events = append(events, struct {
				Type      string `json:"type"`
				Timestamp int64  `json:"timestamp"`
				Data      any    `json:"data"`
			}{kind, tsCursor, map[string]any{"x": rng.Intn(1280), "y": rng.Intn(800)}})
		}

		input := replays.IngestInput{
			SiteID:    defaultSiteID,
			SessionID: sessionID,
			URL:       urls[rng.Intn(len(urls))],
			Browser:   browsers[rng.Intn(len(browsers))],
			OS:        osList[rng.Intn(len(osList))],
			Device:    "desktop",
			HasError:  i == 1,
			Events:    events,
		}
		if _, err := svc.Ingest(ctx, input); err != nil {
			return fmt.Errorf("ingest replay %d: %w", i, err)
		}
	}
	return nil
}

func mapToJSON(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	raw, err := jsonMarshal(m)
	if err != nil {
		return ""
	}
	return raw
}
