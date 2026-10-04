# Installing Observe

Observe is two processes: the `observe` binary and a Nucleus database. Every
install path below starts both (Compose) or expects you to run Nucleus yourself
(systemd, script, source). Configuration reference: the environment-variable
tables in the top-level [README](../../README.md).

Pick the path, then read "After install".

## Docker Compose (recommended for a single host)

`docker-compose.yml` in the repo root runs the `nucleus` and `observe`
services (and an optional Caddy TLS proxy).

```bash
git clone https://github.com/useteploy/teploy-observe.git
cd teploy-observe
docker compose up -d
```

- The dashboard binds **loopback only** (`127.0.0.1:3000`). Reach it over SSH
  or change the `ports:` binding to a tailnet/LAN address (see the header
  comment in `docker-compose.yml`).
- Nucleus is on an internal `backend` network and is not published.
- Public TLS via Caddy: `OBSERVE_DOMAIN=observe.example.com docker compose --profile tls up -d`
  (needs ports 80/443 and DNS pointing at the host).
- To collect telemetry from the public internet without exposing the dashboard,
  set `OBSERVE_INGEST_ADDR: ":3001"` on the `observe` service and publish only
  that port.
- Pin a release with `OBSERVE_VERSION=1.2.3` (image
  `ghcr.io/useteploy/teploy-observe:${OBSERVE_VERSION:-latest}`).
- Data lives in the named volumes `nucleus_data` (database) and `observe_data`
  (`/var/lib/observe`: WAL queue, `audit.key`). Back up both; see
  [backup-restore.md](backup-restore.md).
- No default password: the first browser visit lands on the `/setup` wizard.
  Setting `OBSERVE_ADMIN_PASSWORD` in the environment skips the wizard and
  creates that account at boot.
- Warning: the Compose file starts Nucleus with `NUCLEUS_ALLOW_NO_AUTH=1`,
  relying on the internal network for protection. Do not publish the Nucleus
  port.

## Install script (Linux with systemd; also macOS binary only)

`scripts/install.sh` (published as `install.sh` on each release). Verify the
script against the release `checksums.txt` before running it, exactly as the
README shows:

```bash
(
  set -e
  curl -fsSLO https://github.com/useteploy/teploy-observe/releases/latest/download/install.sh
  curl -fsSLO https://github.com/useteploy/teploy-observe/releases/latest/download/checksums.txt
  grep " install.sh\$" checksums.txt > checksum.txt
  if command -v sha256sum >/dev/null 2>&1; then sha256sum -c checksum.txt || exit 1; else shasum -a 256 -c checksum.txt || exit 1; fi
  sh install.sh
)
```

Environment variables read by the script (`scripts/install.sh`):

| Variable | Default | Meaning |
|---|---|---|
| `OBSERVE_VERSION` | `latest` | Release tag to install (`v` prefix optional). |
| `OBSERVE_PREFIX` | `/usr/local/bin` | Install directory. A non-default prefix requires `OBSERVE_NO_SERVICE=1`. |
| `OBSERVE_NO_SERVICE` | (unset) | Set to skip systemd user/unit/env-file creation. |
| `OBSERVE_HEALTH_URL` | `http://127.0.0.1:3000/healthz` | Readiness URL polled after restarting an already-running service. |

What it does on Linux with systemd: verifies the Ed25519-signed
`checksums.txt` and the archive hash (fails closed), installs the binary,
creates the `observe` system user and `/var/lib/observe`, writes
`/etc/observe/observe.env` (mode 0600) **only if it does not exist** with a
random `OBSERVE_JWT_SECRET`, `OBSERVE_SESSION_SALT` and a random
`OBSERVE_ADMIN_PASSWORD` (printed once at the end), and installs and enables
`observe.service`. If the service was already running it restarts it and, on
failed readiness, restores the previous binary (`observe.prev`).

The script does **not** install Nucleus. Install it separately (next section)
and point `OBSERVE_NUCLEUS_URL` at it. The default unit uses
`postgres://localhost:5432/observe`.

## systemd (manual)

Unit files and the full step list are in `packaging/systemd/` (`README.md`,
`observe.service`, `nucleus.service`). Summary of that README:

```bash
sudo useradd --system --home /var/lib/observe --shell /usr/sbin/nologin observe
sudo useradd --system --home /var/lib/nucleus --shell /usr/sbin/nologin nucleus
sudo mkdir -p /var/lib/observe /var/lib/nucleus/data /var/log/observe /etc/observe
sudo chown observe:observe /var/lib/observe /var/log/observe /etc/observe
sudo chown nucleus:nucleus /var/lib/nucleus /var/lib/nucleus/data

sudo install -m 0755 observe /usr/local/bin/observe
sudo install -m 0755 nucleus /usr/local/bin/nucleus

# /etc/observe/observe.env (mode 0600, owner observe): set at least
#   OBSERVE_JWT_SECRET, OBSERVE_SESSION_SALT  (openssl rand -hex 32)
#   OBSERVE_ADMIN_PASSWORD                    (or leave unset to use /setup)

sudo install -m 0644 nucleus.service /etc/systemd/system/nucleus.service
sudo install -m 0644 observe.service /etc/systemd/system/observe.service
sudo systemctl daemon-reload
sudo systemctl enable --now nucleus observe
sudo journalctl -u observe -f
```

Notes taken from the units: `nucleus.service` runs
`nucleus start --port 5432 --data /var/lib/nucleus/data --max-memory 512` with
`NUCLEUS_ALLOW_NO_AUTH=1`, so keep port 5432 on loopback or a firewalled
interface. `observe.service` sets `TimeoutStopSec=90s` (graceful flush on
SIGTERM) and `ReadWritePaths=/var/lib/observe`; add a reverse proxy for TLS.

## Homebrew and from source

```bash
brew install useteploy/tap/observe
go build ./cmd/observe        # from a clone; dependencies are vendored
```

You still need a running Nucleus; see `docker-compose.yml` for the exact image
and flags.

## After install

1. `curl -fsS http://127.0.0.1:3000/healthz` returns status `ok` and the
   running `version`.
2. Create the admin account (`/setup`, or the bootstrap env vars).
3. Create a site-scoped API key (Settings > API keys, or
   `POST /api/v1/sites/default/keys`) and send a first event; the README
   "First success" section has the tested path.
4. Back up before you rely on it: [backup-restore.md](backup-restore.md).
5. Understand how to upgrade: [upgrade.md](upgrade.md).
