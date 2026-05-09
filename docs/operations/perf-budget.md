# Perf Budget

Roadmap target T041: **10,000 events/sec sustained on 1 vCPU / 2 GB over
10 minutes with p95 ingest latency below 50 ms.**

This is enforced by `.github/workflows/perfbudget.yml`, which boots
Nucleus + Observe end-to-end on a stock GitHub-hosted runner and runs
`scripts/perfbudget.sh` against the live stack.

## Triggering

### Weekly

Runs automatically every Sunday at 04:00 UTC. Failures are visible on
the Actions tab; results are uploaded as the `bench_results` artifact.

### Manual

GitHub UI → Actions → "Perf Budget" → "Run workflow". Override any of
the inputs:

| input          | default | meaning                                   |
| -------------- | ------- | ----------------------------------------- |
| `duration`     | `600s`  | bench duration (Go `time.Duration` form)  |
| `concurrency`  | `128`   | concurrent workers                        |
| `min_rps`      | `10000` | minimum sustained req/s to pass           |
| `max_p95_ms`   | `50`    | maximum p95 latency in milliseconds       |
| `max_fail_pct` | `1`     | maximum % of requests allowed to fail     |

For a fast smoke check after touching ingest code, run with
`duration=30s` and `min_rps=2000` — that's enough to catch a
multiple-x regression without waiting ten minutes.

### Local

```sh
# 1. Boot the dev stack (separate terminal):
scripts/dev.sh

# 2. Mint an API key + raise the per-site cap:
TOKEN=$(curl -s -X POST http://localhost:3000/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"observe"}' | jq -r .token)

curl -s -X PUT -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"rate_per_second":50000}' \
  http://localhost:3000/api/v1/sites/default/ratelimit

KEY=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"local-perf"}' \
  http://localhost:3000/api/v1/sites/default/keys | jq -r .key)

# 3. Run the budget script:
OBSERVE_API_KEY="$KEY" \
OBSERVE_SITE_ID=default \
BUDGET_DURATION=30s \
BUDGET_MIN_RPS=2000 \
sh scripts/perfbudget.sh
```

The workflow does the same provisioning automatically.

## Interpreting results

`bench_results.json` is the source of truth. Each entry has:

- `throughput_rps` — successful requests per second over the run
- `latency_p50_ms` / `p95` / `p99` / `avg` — round-trip from worker
- `total_requests` / `success_requests` / `failed_requests`
- `bytes_sent` — payload bytes pushed (sanity check the link wasn't
  saturated)

Workflow runs also write a markdown table to the job summary
("Summarize results" step) so you can read pass/fail at a glance
without downloading the artifact.

### Pass / fail logic

`scripts/perfbudget.sh` fails the run if **any** of:

- `throughput_rps < BUDGET_MIN_RPS`
- `latency_p95_ms > BUDGET_MAX_P95_MS`
- `failed_requests / total > BUDGET_MAX_FAIL_PCT`

A failed run uploads `service-logs` (Nucleus + Observe stdout) so you
can tell whether the regression is in ingest, the writer pool, or
Nucleus itself.

### Common failure shapes

- **All p95 numbers fine, throughput halved:** look at the writer pool
  size in `internal/ingest/`. Recent changes to flush cadence or batch
  size are the usual culprit.
- **Throughput meets budget, p95 doubled:** usually a new synchronous
  call inside the hot path (auth, geo lookup, JSON serialization).
- **High `failed_requests` count:** the rate limiter or the ingest
  validation rejected payloads. Workflow auto-bumps the cap to
  `5 * min_rps`; if you see this locally, raise the cap manually.

## Updating budgets when hardware changes

The defaults are calibrated for a GitHub-hosted `ubuntu-latest` runner
(2 vCPU, 7 GB RAM at the time of writing — generous compared to the
1 vCPU / 2 GB roadmap target, but the bench is single-process and the
runner shape is the closest reproducible knob we have).

When changing the target hardware profile:

1. Run a 10-minute calibration on the new shape using the manual
   trigger with deliberately low budgets (e.g. `min_rps=1`,
   `max_p95_ms=10000`) so the run finishes regardless of perf.
2. Read the actual throughput / p95 from the job summary.
3. Set the new defaults in `.github/workflows/perfbudget.yml` to ~80%
   of observed throughput and ~120% of observed p95 — that gives
   headroom for normal jitter without hiding regressions.
4. Document the calibration date and runner shape in the commit
   message so future-you knows when the numbers drifted.

If a budget change is forced by an upstream Nucleus regression,
surface it in the Nucleus dogfood findings file rather than just
relaxing the budget.
