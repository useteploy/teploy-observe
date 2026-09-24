package query

// The O12 acceptance load fixture (bounded duration): concurrent real
// ingest (the ingest.Buffer pipeline, no WAL — memory-buffered with
// flushes to the live engine) plus concurrent expensive queries under
// shrunk budgets. Asserts the O12 contract:
//
//   - every query either answers or refuses LABELED (queryguard code)
//     within its budget — no hangs, no unbounded scans, no OOM;
//   - a query over a range larger than the declared row budget refuses
//     with query_budget_rows; a narrow in-budget query answers;
//   - ingest keeps flowing, the buffer's own byte/count bound holds
//     (surplus pushes yield on ErrBufferFull, never error), and the
//     backlog DRAINS to zero after the load stops;
//   - the metrics cardinality guard drops past-cap series with counters
//     instead of growing memory.
//
// Rates are CLIENT-REALISTIC by design (a querier that retried in a tight
// loop was measured to issue 1.7M refusals/s and starve the engine —
// real clients pay an HTTP round trip between attempts, so the fixture
// pays 25-50ms). Duration is bounded by construction: 30s load window
// plus bounded seed and drain phases.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/metrics"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

const (
	o12LoadSeedEvents = 12000
	o12LoadWindow     = 30 * time.Second
	o12LoadIngesters  = 3
	o12LoadQueriers   = 6
	o12LoadRowBudget  = 2000
	o12LoadQueryTime  = 5 * time.Second
	// o12LoadBacklogBound is the during-load backlog ceiling the buffer
	// must hold: pushes beyond it yield on ErrBufferFull (the buffer's
	// own size bound), never error, never grow without limit.
	o12LoadBacklogBound = 60000
)

func TestO12_LoadFixture_MixedIngestAndQuery(t *testing.T) {
	db, ctx, site := o12Connect(t)
	base := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	// --- Seed a wide range once: 12k events across 120 sessions. ---
	seedStart := time.Now()
	const seedBatch = 500
	for start := 0; start < o12LoadSeedEvents; start += seedBatch {
		var q strings.Builder
		q.WriteString(`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, pathname, distinct_id, properties)
			VALUES `)
		args := []any{site} // $1
		for i := 0; i < seedBatch; i++ {
			n := start + i
			if i > 0 {
				q.WriteString(",")
			}
			q.WriteString(fmt.Sprintf("($%d, 'default', $1, $%d, $%d, 'pageview', $%d, $%d, '', 'null')",
				len(args)+1, len(args)+2, len(args)+3, len(args)+4, len(args)+5))
			args = append(args,
				fmt.Sprintf("seed-ev-%d", n),
				fmt.Sprintf("sess-%d", n%120),
				fmt.Sprintf("sess-%d", n%120),
				base.Add(time.Duration(n)*time.Second).UnixMilli(),
				fmt.Sprintf("/p/%d", n%25))
		}
		if _, err := db.SQL().Exec(ctx, q.String(), args...); err != nil {
			t.Fatalf("seed batch %d: %v", start, err)
		}
	}
	t.Logf("seeded %d events in %s", o12LoadSeedEvents, time.Since(seedStart).Round(time.Millisecond))

	// --- The service under test: shrunk budgets, tight concurrency. ---
	limiter := queryguard.NewLimiter(2, 2)
	svc := NewStatsService(db).WithQueryGuard(limiter, queryguard.Budgets{
		Timeout:     o12LoadQueryTime,
		MaxScanRows: o12LoadRowBudget,
		MaxWindow:   186 * 24 * time.Hour,
	})

	// --- The REAL ingest pipeline (memory buffer, flush to engine). ---
	buf := ingest.NewBuffer(db, 50000, 200, 100*time.Millisecond, slog.Default())
	buf.Start()
	t.Cleanup(buf.Stop)

	// --- Metrics cardinality under the same load (tiny series cap). ---
	metricsSvc := metrics.NewService(db).WithCardinalityLimits(metrics.CardinalityLimits{
		MaxAttrsPerPoint:    8,
		MaxAttrValueBytes:   64,
		MaxPointsPerRequest: 500,
		MaxSeriesPerSite:    50,
	})

	loadStart := time.Now()
	stop := make(chan struct{})

	var pushed, yielded, queryOK, refusedRows, refusedConcurrency, queryUnexpected atomic.Int64
	var maxBacklog atomic.Int64
	var wg sync.WaitGroup

	// Ingesters: sustained realistic ingest (~600 events/s total).
	for w := 0; w < o12LoadIngesters; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var n int
			for {
				select {
				case <-stop:
					return
				default:
				}
				n++
				batch := make([]ingest.Event, 25)
				for i := range batch {
					batch[i] = ingest.Event{
						EventID:   fmt.Sprintf("load-%d-%d", w, n),
						TenantID:  "default",
						SiteID:    site,
						SessionID: fmt.Sprintf("load-sess-%d", n%40),
						VisitID:   fmt.Sprintf("load-sess-%d", n%40),
						EventType: "pageview",
						Timestamp: time.Now().UTC().UnixMilli(),
						Pathname:  "/load",
					}
				}
				if err := buf.PushBatch(batch); err != nil {
					if errors.Is(err, ingest.ErrBufferFull) {
						// The buffer's own bound refusing admission is
						// the designed behavior — yield and retry.
						yielded.Add(1)
						time.Sleep(50 * time.Millisecond)
						continue
					}
					queryUnexpected.Add(1)
					t.Errorf("ingest PushBatch: %v", err)
					return
				}
				pushed.Add(int64(len(batch)))
				time.Sleep(100 * time.Millisecond) // 25 events / 100ms / ingester
			}
		}(w)
	}

	// Backlog sampler: the during-load bound (independent watchdog).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int64(buf.Len()); n > maxBacklog.Load() {
				maxBacklog.Store(n)
			}
			if n := int64(buf.Len()); n > o12LoadBacklogBound {
				queryUnexpected.Add(1)
				t.Errorf("ingest backlog %d exceeded the during-load bound %d", n, o12LoadBacklogBound)
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	// Queriers: alternate wide (row-budget refusal), narrow (answers),
	// retention narrow, journeys wide (LIMIT-bounded refusal) — with a
	// client-realistic pause between attempts.
	for w := 0; w < o12LoadQueriers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			round := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				round++
				callCtx, cancel := context.WithTimeout(context.Background(), o12LoadQueryTime+5*time.Second)
				started := time.Now()
				var err error
				switch round % 4 {
				case 0: // wide funnel: 12k rows >> 2000 budget
					_, err = svc.FunnelWithOptions(callCtx, site, base, base.Add(o12LoadSeedEvents*time.Second), []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
				case 1: // narrow funnel: one minute of seeded traffic
					_, err = svc.FunnelWithOptions(callCtx, site, base, base.Add(time.Minute), []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
				case 2: // narrow retention
					_, err = svc.RetentionWithOptions(callCtx, site, base, base.Add(time.Hour), 1, RetentionOptions{})
				case 3: // wide journeys: LIMIT-bounded refusal
					_, err = svc.Journeys(callCtx, site, base, base.Add(o12LoadSeedEvents*time.Second), 10)
				}
				elapsed := time.Since(started)
				cancel()
				if elapsed > o12LoadQueryTime+5*time.Second {
					queryUnexpected.Add(1)
					t.Errorf("query kind %d exceeded its hard deadline: %s", round%4, elapsed)
					return
				}
				if err == nil {
					queryOK.Add(1)
				} else {
					var r *queryguard.Refusal
					if errors.As(err, &r) {
						switch r.Code {
						case queryguard.CodeBudgetRows:
							refusedRows.Add(1)
						case queryguard.CodeConcurrencyGlobal, queryguard.CodeConcurrencySite:
							refusedConcurrency.Add(1)
						default:
							queryUnexpected.Add(1)
							t.Errorf("unexpected refusal code %q: %v", r.Code, err)
							return
						}
					} else {
						queryUnexpected.Add(1)
						t.Errorf("non-refusal query error: %v", err)
						return
					}
				}
				time.Sleep(50 * time.Millisecond) // client round trip
			}
		}(w)
	}

	// Metrics ingester: OTLP exports whose every data point carries a
	// UNIQUE label value against the 50-series cap. Past the cap the
	// points are dropped and counted — whole-batch refusal must NOT
	// happen (only the points-per-request ceiling refuses a batch, and
	// these batches are small).
	var metricsBatches, metricsDroppedBatches atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		n := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			n++
			resp, err := metricsSvc.Ingest(context.Background(), site+"_mx", metricsExportWithUniqueSeries(n))
			if err != nil {
				queryUnexpected.Add(1)
				t.Errorf("metrics ingest must not refuse small batches: %v", err)
				return
			}
			metricsBatches.Add(1)
			if resp.Points == 0 {
				metricsDroppedBatches.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(o12LoadWindow)
	close(stop)
	wg.Wait()

	// --- Drain: everything the buffer accepted must flush. ---
	drainDeadline := time.Now().Add(60 * time.Second)
	for buf.Len() > 0 && time.Now().Before(drainDeadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if n := buf.Len(); n != 0 {
		t.Fatalf("ingest backlog did not drain: %d events pending after load (max during load: %d)", n, maxBacklog.Load())
	}
	stats := buf.Stats()
	buf.Stop()

	// --- Assertions on the O12 contract. ---
	total := time.Since(loadStart)
	if total > 2*time.Minute {
		t.Fatalf("load fixture ran %s, above its bounded duration", total)
	}
	if got := refusedRows.Load(); got == 0 {
		t.Fatal("wide queries over the row budget must have produced labeled row-budget refusals")
	}
	if got := queryOK.Load(); got == 0 {
		t.Fatal("narrow in-budget queries must have answered")
	}
	if got := queryUnexpected.Load(); got != 0 {
		t.Fatalf("%d unexpected outcomes (hangs, unlabeled errors, or bound breaches)", got)
	}
	snap := limiter.Snapshot(queryguard.Budgets{Timeout: o12LoadQueryTime, MaxScanRows: o12LoadRowBudget})
	if snap.GlobalRunning != 0 {
		t.Fatalf("queries still in-flight after quiesce: %d", snap.GlobalRunning)
	}
	if metricsDroppedBatches.Load() == 0 {
		t.Fatal("unique-series metrics past the per-site cap must be dropped (zero-accepted batches observed)")
	}
	card := metricsSvc.CardinalityStats()
	if card.SeriesDropped == 0 || card.SitesAtSeriesCap == 0 {
		t.Fatalf("cardinality counters must record drops and cap state: %+v", card)
	}

	// Seeded rows are still queryable through the budgeted paths after
	// the load (no degradation of the data itself).
	checkCtx, cancel := context.WithTimeout(context.Background(), o12LoadQueryTime)
	defer cancel()
	res, err := svc.FunnelWithOptions(checkCtx, site, base, base.Add(time.Minute), []FunnelStep{{Type: "event", Value: "pageview"}}, FunnelOptions{})
	if err != nil || len(res) != 1 || res[0].Visitors <= 0 {
		t.Fatalf("post-load narrow funnel = (%v, %v)", res, err)
	}

	t.Logf("load fixture: %s total | queries ok=%d rows-refused=%d concurrency-refused=%d | ingest pushed=%d yielded-on-full=%d accepted=%d max-backlog=%d | metrics batches=%d fully-dropped=%d series-dropped=%d sites-at-cap=%d",
		total.Round(time.Millisecond), queryOK.Load(), refusedRows.Load(), refusedConcurrency.Load(),
		pushed.Load(), yielded.Load(), stats.Accepted, maxBacklog.Load(),
		metricsBatches.Load(), metricsDroppedBatches.Load(), card.SeriesDropped, card.SitesAtSeriesCap)
}

// metricsExportWithUniqueSeries builds a small OTLP export whose every
// data point carries a unique label value — a cardinality attack shape.
func metricsExportWithUniqueSeries(n int) metrics.ExportMetricsRequest {
	dp := metrics.NumberDataPoint{
		TimeUnixNano: "1757894400000000000",
		AsDouble:     float64(n),
		Attributes:   []metrics.KeyValue{{Key: "run", Value: metrics.AnyValue{StringValue: fmt.Sprintf("r%d", n)}}},
	}
	return metrics.ExportMetricsRequest{
		ResourceMetrics: []metrics.ResourceMetrics{{
			Resource: metrics.Resource{Attributes: []metrics.KeyValue{{Key: "service.name", Value: metrics.AnyValue{StringValue: "o12-load"}}}},
			ScopeMetrics: []metrics.ScopeMetrics{{
				Metrics: []metrics.OTLPMetric{{
					Name:  "o12_load_gauge",
					Gauge: &metrics.Gauge{DataPoints: []metrics.NumberDataPoint{dp}},
				}},
			}},
		}},
	}
}
