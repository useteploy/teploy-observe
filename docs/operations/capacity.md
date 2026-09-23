# Capacity Operations (O12)

Query admission budgets, concurrency bounds, ingest cardinality limits,
and the disk-pressure surface. Defaults are declared; every knob is
env-tunable at startup. The live posture of all of it is one `/healthz`
read away (the `capacity`, `wal` and `metrics_ingest` blocks).

## Query admission and budgets

The expensive read paths — funnel, funnel breakdown, retention, journeys,
correlation — run under three declared budgets plus concurrency
admission. A query that exceeds a budget is REFUSED with a labeled error
(code + remedy), never truncated into a misleading partial answer:

| refusal code               | status | meaning                                   |
| -------------------------- | ------ | ----------------------------------------- |
| `query_budget_rows`        | 429    | scan exceeded the declared row budget     |
| `query_budget_time`        | 504    | query exceeded its wall-time budget       |
| `query_concurrency_global` | 429    | all global query slots in use             |
| `query_concurrency_site`   | 429    | all of this site's query slots in use     |

The funnel / breakdown / retention scans STREAM rows in the pinned
`(timestamp, event_id)` total order (one walker per entity, O(entities)
memory — the engine pins the ordering, verified by
`TestO12_StreamOrderPinnedAtEngine`). Journeys and correlation run as
whole-result reads bounded by a `LIMIT budget+1` that converts to a
labeled refusal past the ceiling; below the ceiling their results are
unchanged.

The explorer (`/api/v1/query`) keeps its own pre-existing containment
(10s timeout, 4 concurrent, 1000-row scan cap, outer LIMIT 100) —
audited, unchanged by O12.

### Knobs

| env                                   | default | meaning                                        |
| ------------------------------------- | ------- | ---------------------------------------------- |
| `OBSERVE_QUERY_TIMEOUT_MS`            | `30000` | per-query wall-time budget                     |
| `OBSERVE_QUERY_MAX_SCAN_ROWS`         | `1000000` | per-query scanned-row budget                 |
| `OBSERVE_QUERY_MAX_WINDOW_DAYS`       | `186`   | funnel/retention range clamp (retention's pre-O04 clamp, now shared and tunable) |
| `OBSERVE_QUERY_GLOBAL_CONCURRENCY`    | `8`     | total in-flight expensive queries              |
| `OBSERVE_QUERY_SITE_CONCURRENCY`      | `4`     | in-flight expensive queries per site           |

Non-positive or unparsable values keep the default and log a warning at
startup.

v1 posture: admission REFUSES when a slot is not free (no wait queue).
The in-flight set and every refusal are visible at `/healthz`
(`capacity.query`: `global_running`, per-site `sites` map,
`refused_total` counters by code, and the `budgets` in force).

## Metrics ingest cardinality limits

OTLP metrics ingest (JSON and protobuf paths) is guarded at the data
point: label maps are truncated with counters, request sizes above the
ceiling are refused permanently (413 — the exporter must split), and a
data point introducing a NEW series past the per-site cap is dropped and
counted while known series keep flowing. The series registry is
in-memory (a restart re-learns it from traffic) and bounded, so ingest
memory cannot grow with label cardinality. Counters live at `/healthz`
under `metrics_ingest`.

| env                                        | default | meaning                                  |
| ------------------------------------------ | ------- | ---------------------------------------- |
| `OBSERVE_METRICS_MAX_ATTRS_PER_POINT`      | `32`    | labels per data point (truncated past)   |
| `OBSERVE_METRICS_MAX_ATTR_VALUE_BYTES`     | `200`   | one label value (UTF-8-safe cut)         |
| `OBSERVE_METRICS_MAX_POINTS_PER_REQUEST`   | `20000` | data points per export (413 past)        |
| `OBSERVE_METRICS_MAX_SERIES_PER_SITE`      | `20000` | distinct (metric, service, labels) series |

## Disk pressure

`/healthz` carries:

- `capacity.disk` — statfs of the observe data dir (`OBSERVE_DATA_DIR`):
  free/total bytes and used percentage.
- `capacity.engine_disk` — the ENGINE's data directory only when it runs
  on the same host and `OBSERVE_NUCLEUS_DATA_DIR` declares the path;
  otherwise the block says so explicitly (engine-side disk reporting is
  an upstream capability ask, see AUDIT_OPEN).
- `wal` — the pre-existing WAL high-water posture (segments, bytes vs
  cap, breach counters, sync mode).

Together these are the O12 capacity view: one read answers "is the
engine's disk under pressure, is the WAL at its high-water, are queries
being refused and why".

## Table-layout audit (2026-09-23, this slice)

Hot tables, physical layout, and how retention interacts with it:

| table           | layout                                                       | retention column | fit                                                                 |
| --------------- | ------------------------------------------------------------ | ---------------- | ------------------------------------------------------------------- |
| `events`        | plain OLTP since migration 027 (mergetree layout dropped as a Nucleus 0.1.0 workaround) — NO physical order, no index | `timestamp`      | every range read is scan+sort at the engine; retention's chunked DELETE (`ORDER BY col LIMIT 1 OFFSET 4999`) re-scans per 5k chunk |
| `events_recent` | plain OLTP (same 027 rebuild), 7-day TTL                     | `timestamp`      | same shape as `events`, bounded by the 7d window                    |
| `error_events`  | mergetree `ORDER BY (tenant_id, site_id, timestamp, group_hash)` | `timestamp`    | aligned: TTL column is the order key's 3rd component — deletes are prefix-friendly |
| `spans`         | mergetree `ORDER BY (tenant_id, site_id, start_time, trace_id, span_id)` | `start_time` | aligned (3rd component)                                             |
| `logs`          | mergetree `ORDER BY (tenant_id, site_id, timestamp)`          | `timestamp`      | aligned (3rd component)                                             |

The one measured worst offender this slice FIXED: the funnel /
breakdown / retention reads pulled the ENTIRE range into application
memory with no clamp (funnel had none at all). They now stream under the
row/time/window budgets above. The scan+sort cost of range reads on the
unordered `events` table remains an ENGINE-side layout gap — bounded by
the budgets on our side, properly fixed by an index/ordering capability
upstream (recorded in AUDIT_OPEN).

Also audited, bounded this slice via `LIMIT budget+1` + refusal:
journeys and correlation whole-range reads. Trace funnels
(`internal/tracing/funnels.go`) filter spans by `operation_name IN (...)`
before the read and are bounded by spans' 14-day retention — acceptable
for now, noted for the next capacity pass.
