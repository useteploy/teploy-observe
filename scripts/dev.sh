#!/usr/bin/env bash
# Launch Nucleus + Observe together for local development.
# Ctrl-C cleans up both.
#
# Fast UI iteration: edit in teploy-observe/ui/src/, run scripts/ui-sync.sh.
# Observe serves the embedded dist; UI changes require ui-sync.sh + restart.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OBSERVE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
NUCLEUS_BIN="${NUCLEUS_BIN:-$OBSERVE_ROOT/../../tystack/nucleus/target/release/nucleus}"
OBSERVE_BIN="${OBSERVE_BIN:-/tmp/obs-launch/observe}"
NUCLEUS_PORT="${NUCLEUS_PORT:-5432}"
OBSERVE_PORT="${OBSERVE_PORT:-3000}"
DATA_DIR="${OBSERVE_DATA_DIR:-/tmp/obs-launch/data}"

log() { printf '\033[1;36m[dev]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[dev]\033[0m %s\n' "$*" >&2; exit 1; }

[ -x "$NUCLEUS_BIN" ] || fail "nucleus binary missing: $NUCLEUS_BIN (run: cd tystack/nucleus && cargo build --release --features server --bin nucleus)"
[ -x "$OBSERVE_BIN" ] || fail "observe binary missing: $OBSERVE_BIN (run: scripts/ui-sync.sh)"

NUCLEUS_PID=""
OBSERVE_PID=""
cleanup() {
  log "shutting down"
  [ -n "$OBSERVE_PID" ] && kill -TERM "$OBSERVE_PID" 2>/dev/null || true
  [ -n "$NUCLEUS_PID" ] && kill -TERM "$NUCLEUS_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Free the ports if a previous run died uncleanly.
for p in "$NUCLEUS_PORT" "$OBSERVE_PORT"; do
  pids=$(lsof -ti ":$p" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    log "port $p busy, killing: $pids"
    echo "$pids" | xargs kill -TERM 2>/dev/null || true
    sleep 1
  fi
done

mkdir -p "$DATA_DIR"

log "starting nucleus on :$NUCLEUS_PORT (data: $DATA_DIR)"
"$NUCLEUS_BIN" start --port "$NUCLEUS_PORT" --data "$DATA_DIR/nucleus" >"$DATA_DIR/nucleus.log" 2>&1 &
NUCLEUS_PID=$!

# Wait for nucleus to accept connections.
for _ in $(seq 1 40); do
  if (echo > "/dev/tcp/127.0.0.1/$NUCLEUS_PORT") 2>/dev/null; then
    break
  fi
  sleep 0.25
done
(echo > "/dev/tcp/127.0.0.1/$NUCLEUS_PORT") 2>/dev/null || fail "nucleus did not start (see $DATA_DIR/nucleus.log)"

log "starting observe on :$OBSERVE_PORT"
OBSERVE_NUCLEUS_URL="postgres://postgres@localhost:$NUCLEUS_PORT/postgres?sslmode=disable" \
OBSERVE_PORT="$OBSERVE_PORT" \
OBSERVE_DATA_DIR="$DATA_DIR" \
"$OBSERVE_BIN" &
OBSERVE_PID=$!

for _ in $(seq 1 40); do
  if curl -fs "http://localhost:$OBSERVE_PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done

log ""
log "  nucleus   tcp://localhost:$NUCLEUS_PORT   (logs: $DATA_DIR/nucleus.log)"
log "  observe   http://localhost:$OBSERVE_PORT  (admin / observe)"
log ""
log "Ctrl-C to stop."

wait "$OBSERVE_PID" "$NUCLEUS_PID"
