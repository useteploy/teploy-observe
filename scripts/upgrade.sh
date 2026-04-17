#!/bin/sh
# Graceful upgrade helper for Observe.
#
# Downloads a new release, validates it, swaps the binary atomically, and
# restarts the systemd service. Observe's SIGTERM handler drains the ingest
# buffer before exiting, so no in-flight events are lost as long as buffered
# data fits within the default 30s stop timeout.
#
# Usage:
#   observe upgrade              # install latest release
#   OBSERVE_VERSION=v0.6.0 observe upgrade  # pin a version
#
# Env:
#   OBSERVE_PREFIX   install location (default /usr/local/bin)
#   OBSERVE_SERVICE  systemd service name (default observe)

set -eu

OBSERVE_VERSION="${OBSERVE_VERSION:-latest}"
OBSERVE_PREFIX="${OBSERVE_PREFIX:-/usr/local/bin}"
OBSERVE_SERVICE="${OBSERVE_SERVICE:-observe}"
REPO="teploy/observe"

log() { printf '\033[1;34m==\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }

need_sudo() {
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 || die "run as root or install sudo"
    SUDO=sudo
  else
    SUDO=
  fi
}

detect() {
  UNAME_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  UNAME_ARCH=$(uname -m)
  case "$UNAME_OS" in
    linux)  OS=linux ;;
    darwin) OS=darwin ;;
    *) die "unsupported OS: $UNAME_OS" ;;
  esac
  case "$UNAME_ARCH" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported arch: $UNAME_ARCH" ;;
  esac
}

fetch() {
  if [ "$OBSERVE_VERSION" = "latest" ]; then
    TAG=$(curl -sL -o /dev/null -w '%{url_effective}' \
      "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')
    [ -n "$TAG" ] || die "could not determine latest release"
  else
    TAG="$OBSERVE_VERSION"
  fi
  URL="https://github.com/$REPO/releases/download/$TAG/observe-$OS-$ARCH.tar.gz"
  log "Downloading $URL"
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  curl -fL -o "$TMP/observe.tar.gz" "$URL" || die "download failed"
  tar -xzf "$TMP/observe.tar.gz" -C "$TMP"
  NEW_BIN="$TMP/observe"
  [ -f "$NEW_BIN" ] || die "archive missing observe binary"

  # Smoke-test the new binary: make sure it at least prints its version.
  NEW_VERSION=$("$NEW_BIN" version 2>/dev/null | head -1)
  [ -n "$NEW_VERSION" ] || die "new binary didn't print a version — refusing to swap"
  log "New binary validated: $NEW_VERSION"
}

current_version() {
  if [ -x "$OBSERVE_PREFIX/observe" ]; then
    "$OBSERVE_PREFIX/observe" version 2>/dev/null | head -1
  else
    echo "(none)"
  fi
}

swap() {
  need_sudo
  # Back up the old binary next to itself.
  if [ -x "$OBSERVE_PREFIX/observe" ]; then
    $SUDO cp "$OBSERVE_PREFIX/observe" "$OBSERVE_PREFIX/observe.prev"
    log "Previous binary preserved at $OBSERVE_PREFIX/observe.prev"
  fi
  # Atomic-ish: install to a temp path and rename.
  $SUDO install -m 0755 "$NEW_BIN" "$OBSERVE_PREFIX/observe.new"
  $SUDO mv "$OBSERVE_PREFIX/observe.new" "$OBSERVE_PREFIX/observe"
}

restart() {
  if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$OBSERVE_SERVICE.service" >/dev/null 2>&1; then
    log "Restarting $OBSERVE_SERVICE via systemd (SIGTERM drains ingest buffer first)..."
    $SUDO systemctl restart "$OBSERVE_SERVICE"
    log "Restart complete — observe is back up"
  else
    log "No systemd unit found. Signal the running observe process manually:"
    log "  kill -TERM \$(pgrep -f /usr/local/bin/observe)"
    log "  /usr/local/bin/observe &   # or restart however you run it"
  fi
}

detect
log "Current: $(current_version)"
fetch
swap
log "New:     $(current_version)"
restart

cat <<EOF

Upgrade done.

Roll back:
  sudo cp $OBSERVE_PREFIX/observe.prev $OBSERVE_PREFIX/observe
  sudo systemctl restart $OBSERVE_SERVICE

Verify:
  curl http://localhost:3000/healthz
  journalctl -u $OBSERVE_SERVICE --since "2 min ago"
EOF
