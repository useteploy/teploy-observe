# Hardening a self-hosted Observe

This describes what the repository ships and what the operator must still do.
It does not claim properties the code does not enforce.

## Nucleus runs with NUCLEUS_ALLOW_NO_AUTH=1

`docker-compose.yml`, `packaging/systemd/nucleus.service` and every CI job
start Nucleus with `NUCLEUS_ALLOW_NO_AUTH=1`. Observe connects with
`postgres://nucleus:5432/observe` (no password), so the flag is how the
shipped topology works; it has not been verified here whether the engine can
run Observe with authentication enabled, so the flag was not removed.
`NUCLEUS_ALLOW_INSECURE_CLUSTER=1` and `NUCLEUS_ALLOW_INSECURE_REPLICATION=1`
are set for the same reason (single node, no peers).

The consequence is that **network isolation is the only access control on the
database**.

## Network isolation requirement

- Never publish port 5432 (or the cluster port 5433) on a public or shared
  interface.
- Compose: the `nucleus` service has no `ports:` and joins only the
  `backend` network, which is `internal: true` (no route to or from the
  outside). Only `observe` shares it. Caddy is on `frontend` only and cannot
  reach the database. Do not add a `ports:` entry to `nucleus`.
- systemd: `nucleus.service` starts with `--host 127.0.0.1`. `observe.service`
  uses `postgres://localhost:5432/observe`, so both stay on one host. Do not
  change the host to `0.0.0.0`.
- Image version: compose defaults to `ghcr.io/neutron-build/nucleus:v1.1.1`
  (override with `NUCLEUS_VERSION`); CI uses the same value.

## What an attacker with network access to Nucleus gets

Full read/write/DDL over everything Observe stores, with no credential:
events, errors, logs, traces, replays, users and password hashes, API keys,
the audit log, and the SQL surface to rewrite or drop any of it (including
forging audit history). Nucleus authentication state is not a second line of
defense. Treat a reachable Nucleus port as total compromise of the instance.

## JWT secret and session salt persistence

- `OBSERVE_JWT_SECRET`: unset means a random per-process secret is generated
  (logged as "generated random JWT secret"), so all sessions are invalidated
  on every restart. If set it must be at least 32 characters or startup
  validation rejects it. Set it from a secret store, not the repo; rotating it
  logs everyone out.
- `OBSERVE_SESSION_SALT`: unset means a random salt (a warning is logged), so
  visitor/session IDs change across restarts. Set it if you want them stable.
  Changing it re-keys visitor IDs; do it deliberately.
- Neither has a public default. Compose passes both through as empty unless
  you export them.

## TLS and response headers (Caddy profile)

`docker compose --profile tls up -d` runs Caddy with
`packaging/caddy/Caddyfile` (mounted read-only). It obtains a certificate for
`OBSERVE_DOMAIN` and adds, as defaults: `Strict-Transport-Security:
max-age=31536000`, `X-Content-Type-Options: nosniff`, `Referrer-Policy:
strict-origin-when-cross-origin`, `Content-Security-Policy: frame-ancestors
'none'`, and strips `Server`. The last three use Caddy's `?` default-only
form so they never override stricter values the application sets itself.

Not set: a full `script-src`/`style-src` CSP. It has not been validated against
the embedded UI. The direct `127.0.0.1:3000` binding bypasses Caddy and gets
none of these headers.

What the Go server sets itself (no global middleware does this): the shared
dashboard page (`Referrer-Policy: no-referrer`, `X-Content-Type-Options:
nosniff`, `Cache-Control: private, no-store`, in `cmd/observe/main.go`),
replay asset responses (`X-Content-Type-Options: nosniff`, `Content-Security-Policy:
default-src 'none'; sandbox`, in `internal/replays/assets.go`), and
`Referrer-Policy: no-referrer` for stream-ticket requests and the OIDC flow.
All other responses carry no security headers from the application, so a
deployment without the Caddy profile (or an equivalent proxy) has none.

## Trusted proxies

`OBSERVE_TRUSTED_PROXIES` lists the CIDRs whose forwarded-client-IP headers
are honored. Compose defaults it to `172.30.10.2/32`, Caddy's fixed address on
the project-private `frontend` subnet, so no other container is trusted.
Behind a different proxy, set it to that proxy's address only; an overly broad
range lets any client spoof its IP and evade rate limits and IP-based audit
data. `OBSERVE_PUBLIC_URL` sets the canonical external URL used for generated
links (share links, SSO redirects).

## Ingest listener separation

To collect telemetry from the public internet, do not expose the dashboard
port. Set `OBSERVE_INGEST_ADDR` (for example `:3001`) and publish only that
port. The second listener serves a default-deny allowlist of telemetry-write
routes (`ingestRoutes` in `cmd/observe/ingest_listener.go`); everything else
returns 404 there. Keep `OBSERVE_ADDR` on loopback or a private network. With
the listener unset, ingest shares the dashboard port, which is the right
default only for private networks.

## Checklist

1. Nucleus unreachable from outside the host/compose network.
2. `OBSERVE_JWT_SECRET` (32+ chars) and `OBSERVE_SESSION_SALT` set and stored
   outside the repo.
3. TLS terminated by the Caddy profile or an equivalent proxy that sets the
   headers above.
4. `OBSERVE_TRUSTED_PROXIES` limited to that proxy.
5. Public telemetry served via `OBSERVE_INGEST_ADDR` only.
