#!/bin/sh
# Signed direct installer for Observe. Homebrew and container installations
# should be upgraded through their respective package managers.

set -eu

OBSERVE_VERSION="${OBSERVE_VERSION:-latest}"
OBSERVE_PREFIX="${OBSERVE_PREFIX:-/usr/local/bin}"
OBSERVE_NO_SERVICE="${OBSERVE_NO_SERVICE:-}"
OBSERVE_HEALTH_URL="${OBSERVE_HEALTH_URL:-http://127.0.0.1:3000/healthz}"
REPO="useteploy/teploy-observe"
PUBLIC_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAHzMEb0L9K+7ea7HToYTCLAInCvIaumZ90j6pXGa5Esw=
-----END PUBLIC KEY-----'

log() { printf '\033[1;34m==\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }
require() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

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
    linux) OS=linux ;;
    darwin) OS=darwin ;;
    *) die "unsupported OS: $UNAME_OS" ;;
  esac
  case "$UNAME_ARCH" in
    x86_64|amd64) ARCHIVE_ARCH=x86_64 ;;
    aarch64|arm64) ARCHIVE_ARCH=arm64 ;;
    *) die "unsupported architecture: $UNAME_ARCH" ;;
  esac
  if [ "$OS" = linux ] && [ -z "$OBSERVE_NO_SERVICE" ] && [ "$OBSERVE_PREFIX" != /usr/local/bin ]; then
    die "custom OBSERVE_PREFIX requires OBSERVE_NO_SERVICE=1"
  fi
}

resolve_release() {
  if [ "$OBSERVE_VERSION" = latest ]; then
    TAG=$(curl -sSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')
  else
    TAG="$OBSERVE_VERSION"
  fi
  [ -n "$TAG" ] || die "could not determine release version"
  case "$TAG" in v*) ;; *) TAG="v$TAG" ;; esac
  VERSION="${TAG#v}"
  ASSET="observe_${VERSION}_${OS}_${ARCHIVE_ARCH}.tar.gz"
  RELEASE_URL="https://github.com/$REPO/releases/download/$TAG"
}

verify_release() {
  TMP=$(mktemp -d)
  trap cleanup EXIT
  printf '%s\n' "$PUBLIC_KEY" > "$TMP/release.pub"

  log "Authenticating Observe $VERSION"
  curl -fsSL -o "$TMP/checksums.txt" "$RELEASE_URL/checksums.txt" || die "could not download checksums"
  curl -fsSL -o "$TMP/checksums.txt.sig" "$RELEASE_URL/checksums.txt.sig" || die "could not download checksum signature"
  openssl pkeyutl -verify -pubin -inkey "$TMP/release.pub" -rawin \
    -in "$TMP/checksums.txt" -sigfile "$TMP/checksums.txt.sig" >/dev/null 2>&1 ||
    die "release signature verification failed; use Homebrew if this OpenSSL lacks Ed25519 support"

  HASH_COUNT=$(awk -v asset="$ASSET" '$2 == asset || $2 == "*" asset { count++ } END { print count + 0 }' "$TMP/checksums.txt")
  [ "$HASH_COUNT" -eq 1 ] || die "expected exactly one checksum for $ASSET"
  EXPECTED_HASH=$(awk -v asset="$ASSET" '$2 == asset || $2 == "*" asset { print $1 }' "$TMP/checksums.txt")

  log "Downloading $ASSET"
  curl -fSL -o "$TMP/$ASSET" "$RELEASE_URL/$ASSET" || die "release download failed"
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_HASH=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
  else
    ACTUAL_HASH=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
  fi
  [ "$ACTUAL_HASH" = "$EXPECTED_HASH" ] || die "release checksum verification failed"

  tar -xzf "$TMP/$ASSET" -C "$TMP"
  [ -f "$TMP/observe" ] && [ ! -L "$TMP/observe" ] || die "archive missing regular observe binary"
  [ -f "$TMP/packaging/systemd/observe.service" ] || die "archive missing systemd unit"
  chmod 0755 "$TMP/observe"
  [ "$("$TMP/observe" version)" = "observe $VERSION" ] || die "archive version does not match release tag"
}

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    if [ -n "${WAS_ACTIVE:-}" ]; then
      warn "installation interrupted; restoring the previous service"
      $SUDO systemctl stop observe.service >/dev/null 2>&1 || true
    fi
    if [ -n "${SWITCHED:-}" ] && [ -f "$OBSERVE_PREFIX/observe.prev" ]; then
      $SUDO mv "$OBSERVE_PREFIX/observe.prev" "$OBSERVE_PREFIX/observe" || true
    fi
    if [ -n "${UNIT_REPLACED:-}" ]; then
      $SUDO rm -f /etc/systemd/system/.observe.service.new || true
      if [ -f "$TMP/observe.service.prev" ]; then
        $SUDO install -m 0644 "$TMP/observe.service.prev" /etc/systemd/system/observe.service || true
      else
        $SUDO rm -f /etc/systemd/system/observe.service || true
      fi
      $SUDO systemctl daemon-reload >/dev/null 2>&1 || true
    fi
    if [ -n "${WAS_ACTIVE:-}" ]; then
      $SUDO systemctl start observe.service >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "${TMP:-}"
}

install_binary() {
  need_sudo
  $SUDO install -m 0755 "$TMP/observe" "$OBSERVE_PREFIX/.observe.new"
  WAS_ACTIVE=
  if [ "$OS" = linux ] && [ -z "$OBSERVE_NO_SERVICE" ] && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet observe.service; then
    WAS_ACTIVE=1
    log "Stopping Observe through systemd"
    if ! $SUDO systemctl stop observe.service; then
      $SUDO rm -f "$OBSERVE_PREFIX/.observe.new"
      die "could not stop Observe"
    fi
  fi
  if [ -f "$OBSERVE_PREFIX/observe" ]; then
    $SUDO cp "$OBSERVE_PREFIX/observe" "$OBSERVE_PREFIX/observe.prev"
  fi
  if ! $SUDO mv "$OBSERVE_PREFIX/.observe.new" "$OBSERVE_PREFIX/observe"; then
    [ -z "${WAS_ACTIVE:-}" ] || $SUDO systemctl start observe.service || true
    die "could not install Observe"
  fi
  SWITCHED=1
}

install_service() {
  if [ "$OS" != linux ] || [ -n "$OBSERVE_NO_SERVICE" ]; then return 0; fi
  if ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found; skipping service installation"
    return 0
  fi

  log "Installing systemd service"
  if ! id -u observe >/dev/null 2>&1; then
    $SUDO useradd --system --home /var/lib/observe --shell /usr/sbin/nologin observe
  fi
  $SUDO mkdir -p /var/lib/observe /etc/observe
  $SUDO chown observe:observe /var/lib/observe

  if [ ! -f /etc/observe/observe.env ]; then
    JWT=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
    SALT=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
    PASS=$(head -c 12 /dev/urandom | base64 | tr -d '/+=' | cut -c1-16)
    $SUDO tee /etc/observe/observe.env >/dev/null <<EOF
OBSERVE_JWT_SECRET=$JWT
OBSERVE_SESSION_SALT=$SALT
OBSERVE_ADMIN_USER=admin
OBSERVE_ADMIN_PASSWORD=$PASS
EOF
    $SUDO chmod 0600 /etc/observe/observe.env
    $SUDO chown observe:observe /etc/observe/observe.env
    GENERATED_PASSWORD="$PASS"
  fi

  if [ -f /etc/systemd/system/observe.service ]; then
    $SUDO cp /etc/systemd/system/observe.service "$TMP/observe.service.prev"
  fi
  UNIT_REPLACED=1
  $SUDO install -m 0644 "$TMP/packaging/systemd/observe.service" /etc/systemd/system/.observe.service.new
  $SUDO mv /etc/systemd/system/.observe.service.new /etc/systemd/system/observe.service
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable observe.service >/dev/null 2>&1 || true
}

restart_existing_service() {
  [ -n "${WAS_ACTIVE:-}" ] || return 0
  log "Starting Observe $VERSION"
  if $SUDO systemctl start observe.service; then
    i=0
    while [ "$i" -lt 60 ]; do
      HEALTH=$(curl -fsS "$OBSERVE_HEALTH_URL" 2>/dev/null || true)
      case "$HEALTH" in *'"status":"ok"'*) STATUS_OK=1 ;; *) STATUS_OK= ;; esac
      case "$HEALTH" in *'"version":"'"$VERSION"'"'*) VERSION_OK=1 ;; *) VERSION_OK= ;; esac
      if [ -n "$STATUS_OK" ] && [ -n "$VERSION_OK" ]; then
        SWITCHED=
        return 0
      fi
      i=$((i + 1))
      sleep 1
    done
  fi

  warn "new version failed readiness; restoring previous binary"
  $SUDO systemctl stop observe.service || true
  [ -f "$OBSERVE_PREFIX/observe.prev" ] || die "rollback binary is missing"
  $SUDO mv "$OBSERVE_PREFIX/observe.prev" "$OBSERVE_PREFIX/observe"
  $SUDO systemctl start observe.service || die "rollback restart failed"
  die "installation failed; previous version restored"
}

require curl
require tar
require openssl
detect
resolve_release
verify_release
install_binary
install_service
restart_existing_service
SWITCHED=

log "Observe $VERSION installed at $OBSERVE_PREFIX/observe"
if [ "$OS" = linux ] && [ -z "$OBSERVE_NO_SERVICE" ] && command -v systemctl >/dev/null 2>&1; then
  printf 'Service: systemctl start observe\n'
  printf 'Logs:    journalctl -u observe -f\n'
fi
if [ -n "${GENERATED_PASSWORD:-}" ]; then
  printf 'Initial admin password: %s\n' "$GENERATED_PASSWORD"
fi
printf 'UI: http://%s:3000\n' "$(hostname)"
