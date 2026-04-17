#!/bin/sh
# Performance budget check.
#
# Runs the event-ingest benchmark against a running Observe and asserts that
# throughput and p95 latency stay above/below configurable thresholds.
#
# Defaults target a typical 1vCPU/2GB VPS over localhost. Tune via env:
#
#   OBSERVE_URL          http://localhost:3000 by default
#   OBSERVE_API_KEY      required when auth is on; empty for grace-period
#   BUDGET_DURATION      default 60s
#   BUDGET_CONCURRENCY   default 64
#   BUDGET_MIN_RPS       default 1000 (raise to 10000 for the roadmap target)
#   BUDGET_MAX_P95_MS    default 50
#   BUDGET_MAX_FAIL_PCT  default 1.0
#
# Exits 0 on success, 1 on threshold breach.

set -eu

OBSERVE_URL="${OBSERVE_URL:-http://localhost:3000}"
OBSERVE_API_KEY="${OBSERVE_API_KEY:?OBSERVE_API_KEY required — create one at /settings}"
OBSERVE_SITE_ID="${OBSERVE_SITE_ID:-default}"
BUDGET_DURATION="${BUDGET_DURATION:-60s}"
BUDGET_CONCURRENCY="${BUDGET_CONCURRENCY:-64}"
BUDGET_MIN_RPS="${BUDGET_MIN_RPS:-1000}"
BUDGET_MAX_P95_MS="${BUDGET_MAX_P95_MS:-50}"
BUDGET_MAX_FAIL_PCT="${BUDGET_MAX_FAIL_PCT:-1.0}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BENCH_BIN="$REPO_ROOT/bench/observe-bench"

# Build the bench if it's not already present.
if [ ! -x "$BENCH_BIN" ]; then
  echo "==> Building bench binary..."
  (cd "$REPO_ROOT/bench" && go build -o observe-bench .)
fi

OUT_FILE="$REPO_ROOT/bench_results.json"

echo "==> Running bench: $BUDGET_DURATION @ concurrency=$BUDGET_CONCURRENCY"
(
  cd "$REPO_ROOT"
  "$BENCH_BIN" \
    -target "$OBSERVE_URL" \
    -duration "$BUDGET_DURATION" \
    -c "$BUDGET_CONCURRENCY" \
    -mode analytics \
    -site "$OBSERVE_SITE_ID" \
    -key "$OBSERVE_API_KEY"
)

echo
echo "==> Checking thresholds..."
python3 - <<PY
import json, sys
with open("$OUT_FILE") as f:
    results = json.load(f)
if not isinstance(results, list) or not results:
    print("FAIL: bench did not produce a results array")
    sys.exit(1)

r = results[0]  # analytics
total = r.get("total_requests", 0)
failed = r.get("failed_requests", 0)
rps = r.get("throughput_rps", 0.0)
p95 = r.get("latency_p95_ms", 0.0)
fail_pct = (failed / total * 100) if total else 0

print(f"  throughput: {rps:.0f} req/s  (min required: $BUDGET_MIN_RPS)")
print(f"  p95:        {p95:.2f} ms      (max allowed:  $BUDGET_MAX_P95_MS)")
print(f"  failures:   {fail_pct:.2f}%    (max allowed:  $BUDGET_MAX_FAIL_PCT%)")

ok = True
if rps < $BUDGET_MIN_RPS:
    print(f"  FAIL: throughput below budget")
    ok = False
if p95 > $BUDGET_MAX_P95_MS:
    print(f"  FAIL: p95 above budget")
    ok = False
if fail_pct > $BUDGET_MAX_FAIL_PCT:
    print(f"  FAIL: failure rate above budget")
    ok = False

if ok:
    print("  PASS: all thresholds met")
    sys.exit(0)
else:
    sys.exit(1)
PY
