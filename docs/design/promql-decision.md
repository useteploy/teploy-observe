# PromQL support (NOT BUILT; decision pending)

Status: Observe does not implement PromQL. The README calls PromQL "an
explicit future decision, not an implied compatibility". This note frames that
decision; it does not make it.

## Current state

- Metrics arrive via OTLP (`POST /v1/metrics`, protobuf or JSON) and are stored
  per series. They are queried through a structured API,
  `GET /api/v1/metrics/query` and `GET /api/v1/metrics/series`
  (`cmd/observe/metrics_handlers.go`): metric name, exact-match label filters,
  group-by, step and the reducers `last|avg|sum|min|max|rate|p50|p95|p99`.
- Reducer semantics are pinned by `internal/metrics/o07_reference_test.go`:
  per-series `rate` before aggregation, counter-reset handling by the
  restart-at-zero convention, cumulative-histogram differencing, bucket
  interpolation, and an `estimate`/`method` label on every point.
- There is no PromQL parser, no Prometheus-compatible `query_range` endpoint,
  and no remote-write or scrape ingest. The only PromQL mentions in the code
  are the "deliberately does not implement" comment in
  `internal/metrics/metrics.go` and README text.
- Consequences: Grafana's Prometheus data source, PromQL alert rules and
  existing PromQL dashboards do not work against Observe. Observe's own alert
  rules cover only `pageviews`, `visitors`, `error_count` and `error_rate`;
  there are no metric-series alerts.

## Requirements if built

1. A scope decision: (a) a read-only PromQL query API for Grafana, (b) plus
   Prometheus remote-write ingest, (c) plus PromQL alerting.
2. Semantic parity for the advertised subset. PromQL has staleness handling,
   lookback delta, `rate`/`increase` extrapolation and `histogram_quantile`
   rules that differ from the pinned O07 semantics (Prometheus extrapolates at
   window edges; Observe time-weights buckets). The two would disagree on the
   same data unless one is declared authoritative.
3. Query admission: PromQL range queries easily exceed the row and wall-time
   budgets in `docs/operations/capacity.md`; they must run under the same
   admission control.
4. The existing cardinality limits (20k series per site) bound what is
   queryable.

## Risks

- A hand-rolled subset invites silent wrong answers. The README already
  commits that "if it ever lands it will wrap the upstream Prometheus engine,
  never a hand-rolled subset".
- Embedding the Prometheus engine pulls a large dependency tree. New
  dependencies must already be vendored (no network here), and binary size and
  the idle footprint in BENCHMARKS.md would change.
- The engine needs a storage interface implemented over Observe's series
  store, backed by Nucleus queries that CI here cannot exercise.

## Recommended approach

Defer until there is concrete demand. If Grafana compatibility is needed,
build only (a): a read-only adapter implementing the Prometheus storage
interface over the existing series reader, wrapping the upstream engine, under
the same admission control, with a published compatibility table in the style
of `docs/sdk/COMPATIBILITY.md` that states the differences from O07. Leave
PromQL alerting and remote-write out of the first pass.

## Open owner decisions

- Is Grafana compatibility a goal, and for which panels?
- Accept the dependency and size cost of the upstream Prometheus engine?
- Which semantics are authoritative when the engine and O07 disagree?
- Should metric-threshold alerts exist at all before any PromQL work?
