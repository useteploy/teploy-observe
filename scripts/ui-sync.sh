#!/usr/bin/env bash
# Canonical UI rebuild: edit in teploy-observe/ui/src/, build in Neutron canonical,
# embed dist back into cmd/observe/ui/dist/, rebuild Go binary.
#
# Idempotent: running against unchanged source produces byte-identical dist.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OBSERVE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OBSERVE_UI_SRC="$OBSERVE_ROOT/ui/src"
CANONICAL_APP="$OBSERVE_ROOT/../../Neutron/typescript/apps/observe"
CANONICAL_SRC="$CANONICAL_APP/src"
EMBED_DIST="$OBSERVE_ROOT/cmd/observe/ui/dist"
BIN_OUT="${OBSERVE_BIN:-/tmp/obs-launch/observe}"

log() { printf '\033[1;34m[ui-sync]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[ui-sync]\033[0m %s\n' "$*" >&2; exit 1; }

[ -d "$OBSERVE_UI_SRC" ] || fail "edit source missing: $OBSERVE_UI_SRC"
[ -d "$CANONICAL_APP" ] || fail "canonical app missing: $CANONICAL_APP"
command -v rsync >/dev/null || fail "rsync not on PATH"
command -v pnpm  >/dev/null || fail "pnpm not on PATH"
command -v go    >/dev/null || fail "go not on PATH"

log "rsync $OBSERVE_UI_SRC/ -> $CANONICAL_SRC/"
rsync -a --delete \
  --exclude '.neutron-routes.d.ts' \
  "$OBSERVE_UI_SRC/" "$CANONICAL_SRC/"

# The HTML shell also lives beside src/, so an edit to ui/index.html (e.g. the
# favicon link) would otherwise never reach the app that actually builds.
if [ -f "$OBSERVE_ROOT/ui/index.html" ]; then
  log "copy ui/index.html -> $CANONICAL_APP/index.html"
  cp "$OBSERVE_ROOT/ui/index.html" "$CANONICAL_APP/index.html"
fi

# Static assets (favicon, etc.) live beside src/, not inside it — Neutron copies
# publicDir into the build, so it has to reach the canonical app too or the
# built dashboard silently ships without them.
if [ -d "$OBSERVE_ROOT/ui/public" ]; then
  log "rsync ui/public/ -> $CANONICAL_APP/public/"
  mkdir -p "$CANONICAL_APP/public"
  rsync -a --delete "$OBSERVE_ROOT/ui/public/" "$CANONICAL_APP/public/"
fi

log "patching imports (neutron/client -> @neutron-build/core/client)"
# Escape `@` in the replacement — perl interpolates `@name` as an array.
find "$CANONICAL_SRC" -type f \( -name '*.ts' -o -name '*.tsx' \) -print0 \
  | xargs -0 perl -pi -e '
      s|(["\x27])neutron/client(["\x27])|$1\@neutron-build/core/client$2|g;
      s|(["\x27])neutron(["\x27])|$1\@neutron-build/core$2|g;
    '

log "pnpm build (canonical)"
pnpm -C "$CANONICAL_APP" build

log "replacing embedded dist"
rm -rf "$EMBED_DIST"
mkdir -p "$EMBED_DIST"
cp -R "$CANONICAL_APP/dist/." "$EMBED_DIST/"

log "go build -> $BIN_OUT"
mkdir -p "$(dirname "$BIN_OUT")"
( cd "$OBSERVE_ROOT" && go build -o "$BIN_OUT" ./cmd/observe )

log "done. binary: $BIN_OUT  dist: $EMBED_DIST"
