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
export OUT_FILE BUDGET_MIN_RPS BUDGET_MAX_P95_MS BUDGET_MAX_FAIL_PCT
rm -f "$OUT_FILE"

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
python3 - <<'PY'
import json, math, os, sys
try:
    with open(os.environ["OUT_FILE"]) as f:
        results = json.load(f)
    if not isinstance(results, list) or len(results) != 1:
        raise ValueError("bench must produce exactly one analytics result")
    r = results[0]
    if r["mode"] != "analytics":
        raise ValueError("unexpected benchmark mode")
    total, failed = r["total_requests"], r["failed_requests"]
    if type(total) is not int or type(failed) is not int or total <= 0 or not 0 <= failed <= total:
        raise ValueError("invalid request counts")
    rps, p95 = float(r["throughput_rps"]), float(r["latency_p95_ms"])
    minimum = float(os.environ["BUDGET_MIN_RPS"])
    maximum = float(os.environ["BUDGET_MAX_P95_MS"])
    failures = float(os.environ["BUDGET_MAX_FAIL_PCT"])
    if not all(math.isfinite(v) and v >= 0 for v in (rps, p95, minimum, maximum, failures)):
        raise ValueError("nonfinite or negative metric/budget")
    fail_pct = failed / total * 100
except (OSError, ValueError, KeyError, TypeError) as e:
    print(f"FAIL: invalid benchmark results: {e}")
    sys.exit(1)

print(f"  throughput: {rps:.0f} req/s  (min required: {minimum})")
print(f"  p95:        {p95:.2f} ms      (max allowed: {maximum})")
print(f"  failures:   {fail_pct:.2f}%    (max allowed: {failures}%)")
ok = rps >= minimum and p95 <= maximum and fail_pct <= failures
print("  PASS: all thresholds met" if ok else "  FAIL: performance budget exceeded")
sys.exit(0 if ok else 1)
PY
