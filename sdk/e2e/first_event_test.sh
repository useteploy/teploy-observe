#!/bin/bash
# O11 end-to-end first-event test: installation -> credential -> first
# event -> QUERY-VISIBLE against a real Observe binary and real ingest
# (API-key middleware enforced), plus the keyless-request 401 proof.
#
# Not part of the normal test suites: it needs podman and exclusive ports.
# Run explicitly:
#
#   bash sdk/e2e/first_event_test.sh
#
# Own fixture only (never 55432/55433): nucleus on :55446, observe on :38080.
#
# BLOCKED UPSTREAM (2026-09-23): the vendored Neutron lifecycle has a
# success-path rollback bug — go/neutron/lifecycle.go's deferred
# stopLimited runs even when every hook started cleanly, so app.Run()
# boots Observe with its own hooks already stopped (ingest buffer,
# scheduler, nucleus pool; /healthz answers 503 "nucleus: exec: closed
# pool"). This script FAILS at the healthz gate until that one-line
# framework fix lands in the Neutron repo (guard the defer with the
# start error); the failure is the pin working, not a broken test. See
# the O11 slice entry in AUDIT_OPEN.md for the probe that isolated it.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d /tmp/observe-o11-e2e.XXXXXX)"
NUCLEUS_CONTAINER=nucleus-o11b
NUCLEUS_PORT=55446
OBSERVE_PORT=38080
BASE="http://127.0.0.1:$OBSERVE_PORT"
ADMIN_USER=admin
ADMIN_PASSWORD=o11-e2e-password
OBSERVE_PID=""

cleanup() {
  if [ -n "$OBSERVE_PID" ]; then kill "$OBSERVE_PID" 2>/dev/null || true; fi
  podman rm -f "$NUCLEUS_CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  if [ -f "$WORK/observe.log" ]; then
    echo "— observe.log —" >&2
    grep -v "worker registered" "$WORK/observe.log" | tail -40 >&2 || true
  fi
  exit 1
}
log() { echo "— $*"; }

log "starting nucleus fixture on :$NUCLEUS_PORT (container $NUCLEUS_CONTAINER)"
podman rm -f "$NUCLEUS_CONTAINER" >/dev/null 2>&1 || true
podman run -d --name "$NUCLEUS_CONTAINER" -p "$NUCLEUS_PORT:5432" \
  -e NUCLEUS_ALLOW_NO_AUTH=1 \
  -e NUCLEUS_ALLOW_INSECURE_CLUSTER=1 \
  -e NUCLEUS_ALLOW_INSECURE_REPLICATION=1 \
  ghcr.io/neutron-build/nucleus:v1.1.1 \
  start --host 0.0.0.0 --port 5432 --cluster-port 5433 --data /data --max-memory 512 \
  >/dev/null || fail "podman run nucleus"

for _ in $(seq 1 60); do
  if (echo > /dev/tcp/127.0.0.1/$NUCLEUS_PORT) 2>/dev/null; then break; fi
  sleep 0.5
done
(echo > /dev/tcp/127.0.0.1/$NUCLEUS_PORT) 2>/dev/null || fail "nucleus did not come up"

log "building observe binary"
(cd "$REPO_ROOT" && go build -o "$WORK/observe" ./cmd/observe) || fail "go build observe"

log "starting observe on :$OBSERVE_PORT"
OBSERVE_NUCLEUS_URL="postgres://nucleus@127.0.0.1:$NUCLEUS_PORT/observe?sslmode=disable" \
OBSERVE_ADDR=":$OBSERVE_PORT" \
OBSERVE_ADMIN_USER="$ADMIN_USER" \
OBSERVE_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
OBSERVE_SESSION_SALT="o11-e2e-salt" \
OBSERVE_DATA_DIR="$WORK/data" \
  "$WORK/observe" >"$WORK/observe.log" 2>&1 &
OBSERVE_PID=$!

for _ in $(seq 1 60); do
  if curl -fs "$BASE/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
curl -fs "$BASE/healthz" >/dev/null || fail "observe did not come up — log tail: $(tail -5 "$WORK/observe.log" 2>/dev/null | tr '\n' ' ')"

log "credential: admin login -> site-scoped API key"
TOKEN=$(curl -fs -X POST "$BASE/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASSWORD\"}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])') \
  || fail "admin login"
API_KEY=$(curl -fs -X POST "$BASE/api/v1/sites/default/keys" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"label":"o11-e2e"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])') \
  || fail "API key creation"
[ -n "$API_KEY" ] || fail "empty API key"

log "keyless ingest is refused with 401 (required-key proof)"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/api/v1/events/batch" \
  -H "Content-Type: application/json" \
  -d '{"v":2,"producer_id":"e2e","batch_id":"e2e1","events":[{"site_id":"default","event_type":"x","event_id":"e2e-x-1"}]}')
[ "$CODE" = "401" ] || fail "keyless batch got $CODE, want 401"

export OBSERVE_E2E_URL="$BASE" OBSERVE_E2E_KEY="$API_KEY"

log "browser SDK: first event"
(cd "$REPO_ROOT/sdk/browser" && npx tsx "$REPO_ROOT/sdk/e2e/browser_first_event.mts") \
  || fail "browser SDK first event"

log "sentry shim: first capture"
(cd "$REPO_ROOT/sdk/sentry-shim" && npx tsx "$REPO_ROOT/sdk/e2e/sentry_first_event.mts") \
  || fail "sentry shim first capture"

log "Go SDK: first event"
(cd "$REPO_ROOT/sdk/go" && go run ./examples/firstevent) \
  || fail "Go SDK first event"

log "Python SDK: first event"
python3 "$REPO_ROOT/sdk/e2e/python_first_event.py" \
  || fail "Python SDK first event"

log "query-visible: polling the stats API for the SDK-written records"
visible=1
for _ in $(seq 1 30); do
  EVENTS=$(curl -fs "$BASE/api/v1/stats/events?site_id=default" -H "Authorization: Bearer $TOKEN")
  LOGS=$(curl -fs "$BASE/api/v1/logs/search?site_id=default&q=o11&limit=50" -H "Authorization: Bearer $TOKEN" || echo "{}")
  if echo "$EVENTS" | grep -q "o11_browser_first_event" \
     && echo "$LOGS" | grep -q "o11 go first event" \
     && echo "$LOGS" | grep -q "o11 python first event" \
     && echo "$LOGS" | grep -q "o11 sentry-shim first event"; then
    visible=0
    break
  fi
  sleep 1
done
[ "$visible" = "0" ] || fail "SDK records not query-visible:
events: $EVENTS
logs: $LOGS"

log "PASS: capture -> credential -> first event -> query-visible for browser/shim/Go/Python"
