#!/bin/sh
# Observe installer. Usage:
#   curl -sL https://observe.dev/install.sh | sh
#
# Flags (set as env vars):
#   OBSERVE_VERSION=v0.5.0        pin a release tag (default: latest)
#   OBSERVE_PREFIX=/usr/local/bin install location for the binary
#   OBSERVE_NO_SERVICE=1          skip creating the systemd unit
#
# The script is intentionally a POSIX shell (not bash) so it runs on minimal
# Alpine/Debian/macOS installs.

set -eu

OBSERVE_VERSION="${OBSERVE_VERSION:-latest}"
OBSERVE_PREFIX="${OBSERVE_PREFIX:-/usr/local/bin}"
OBSERVE_NO_SERVICE="${OBSERVE_NO_SERVICE:-}"
REPO="teploy/observe"

#
# ─── helpers ────────────────────────────────────────────────────────────
#

log() { printf '\033[1;34m==\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"
}

need_sudo() {
  if [ "$(id -u)" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
      SUDO=sudo
    else
      die "run as root or install sudo"
    fi
  else
    SUDO=
  fi
}

#
# ─── detect platform ────────────────────────────────────────────────────
#

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
  log "Detected platform: $OS/$ARCH"
}

#
# ─── fetch release ──────────────────────────────────────────────────────
#

fetch_url() {
  if [ "$OBSERVE_VERSION" = "latest" ]; then
    TAG=$(curl -sL -o /dev/null -w '%{url_effective}' \
      "https://github.com/$REPO/releases/latest" \
      | sed 's#.*/tag/##')
    [ -n "$TAG" ] || die "could not determine latest release"
  else
    TAG="$OBSERVE_VERSION"
  fi
  URL="https://github.com/$REPO/releases/download/$TAG/observe-$OS-$ARCH.tar.gz"
  log "Downloading $URL"
}

install_binary() {
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT

  curl -fL -o "$TMP/observe.tar.gz" "$URL" || die "download failed"
  tar -xzf "$TMP/observe.tar.gz" -C "$TMP"
  [ -f "$TMP/observe" ] || die "archive missing observe binary"

  need_sudo
  $SUDO install -m 0755 "$TMP/observe" "$OBSERVE_PREFIX/observe"
  log "Installed $OBSERVE_PREFIX/observe"
}

#
# ─── optional: systemd unit ─────────────────────────────────────────────
#

install_service() {
  if [ "$OS" != "linux" ]; then return 0; fi
  if [ -n "$OBSERVE_NO_SERVICE" ]; then return 0; fi
  if ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found — skipping service install"
    return 0
  fi

  need_sudo
  log "Creating observe service account and directories"
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

  $SUDO tee /etc/systemd/system/observe.service >/dev/null <<'UNIT'
[Unit]
Description=Observe — self-hosted analytics, errors, logs, traces, replays
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=observe
Group=observe
WorkingDirectory=/var/lib/observe
EnvironmentFile=-/etc/observe/observe.env
Environment=OBSERVE_ADDR=:3000
Environment=OBSERVE_NUCLEUS_URL=postgres://localhost:5432/observe
ExecStart=/usr/local/bin/observe
Restart=always
RestartSec=5s
TimeoutStopSec=30s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/observe
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=observe

[Install]
WantedBy=multi-user.target
UNIT

  $SUDO systemctl daemon-reload
  $SUDO systemctl enable observe.service >/dev/null 2>&1 || true
  log "Installed systemd unit: observe.service"
}

#
# ─── go ─────────────────────────────────────────────────────────────────
#

require curl
require tar
detect
fetch_url
install_binary
install_service

echo
log "Observe installed successfully."
echo
echo "  Binary:  $OBSERVE_PREFIX/observe"
if [ "$OS" = "linux" ] && [ -z "$OBSERVE_NO_SERVICE" ] && command -v systemctl >/dev/null 2>&1; then
  echo "  Service: systemctl start observe"
  echo "  Logs:    journalctl -u observe -f"
  if [ -n "${GENERATED_PASSWORD:-}" ]; then
    echo
    echo "  Initial admin password: $GENERATED_PASSWORD"
    echo "  (stored in /etc/observe/observe.env — rotate via /settings)"
  fi
  echo
  echo "  Next: set up Nucleus (the database). Either:"
  echo "    - run \`nucleus start --port 5432\` on this host, or"
  echo "    - point OBSERVE_NUCLEUS_URL at a remote Nucleus / Postgres instance."
else
  echo "  Run:     observe"
fi
echo
echo "  UI:      http://$(hostname):3000"
