#!/usr/bin/env bash
# Rebuild twice from locked authored input, then check (default) or update.
set -euo pipefail
mode=${1:---check}
case "$mode" in --check|--update) ;; *) echo 'usage: regenerate-replay.sh [--check|--update]' >&2; exit 2;; esac
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source_dir="$root/cmd/observe/tracker/replay-delta"
stage=$(mktemp -d "${TMPDIR:-/tmp}/observe-replay.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
for pass in first second; do
  mkdir -p "$stage/$pass/replay-delta" "$stage/$pass/player"
  cp "$source_dir/package.json" "$source_dir/package-lock.json" "$stage/$pass/replay-delta/"
  cp -R "$source_dir/src" "$stage/$pass/replay-delta/"
  (
    cd "$stage/$pass/replay-delta"
    npm ci --ignore-scripts
    npm run build
    OBSERVE_PLAYER_OUTPUT="$stage/$pass/player" npm run build:player
  )
done
for path in observe-replay-delta.js player/replayer.js player/sanitize.js; do
  cmp "$stage/first/$path" "$stage/second/$path"
done
outputs=("cmd/observe/tracker/observe-replay-delta.js" "ui/public/rrweb/replayer.js" "ui/public/rrweb/sanitize.js")
inputs=("observe-replay-delta.js" "player/replayer.js" "player/sanitize.js")
for i in 0 1 2; do
  if [ "$mode" = --update ]; then
    cp "$stage/first/${inputs[$i]}" "$root/${outputs[$i]}"
  else
    cmp "$stage/first/${inputs[$i]}" "$root/${outputs[$i]}"
  fi
done
