# Service tokens and discovery

How another Teploy product (Dash first) finds an Observe instance and provisions
it without holding a person's session. An admin creates the token; everything
else is machine to machine.

## Discovery

The image carries `LABEL dev.teploy.product=observe`. A candidate is confirmed by

```
GET /.well-known/teploy.json
```

```json
{
  "product": "observe",
  "version": "0.2.0",
  "instance_id": "9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80",
  "dashboard_url": "https://observe.example.com",
  "ingest_url": "https://ingest.example.com",
  "capabilities": ["service_tokens", "sites_external_ref", "otlp_http"]
}
```

Unauthenticated, no secrets. `instance_id` is generated once and stored in the
database, so it survives container replacement and a restore brings it back. That
also means a restored copy looks like the original: same id, same tokens. If the copy
will run alongside the original, run `observe reidentify --yes` before starting it,
which gives it a new id and revokes every service token. See
`docs/operations/backup-restore.md`.

- `dashboard_url` is `OBSERVE_DASHBOARD_URL`, which nothing else reads. With no
  separate ingest listener it falls back to `OBSERVE_PUBLIC_URL`. With a separate
  listener and no `OBSERVE_DASHBOARD_URL` it is empty, because `OBSERVE_PUBLIC_URL`
  may be the ingest host. It is never taken from the request's Host header, which a
  client can spoof.
- `ingest_url` is `OBSERVE_INGEST_URL`. With no separate ingest listener it is the
  same as `dashboard_url`, because they are the same port. With a separate listener
  and no `OBSERVE_INGEST_URL` it is empty.
- Treat an empty URL as unknown and ask. Never guess.
- `otlp_grpc` is listed only when `OBSERVE_OTLP_GRPC_ADDR` is set.

**Served on the dashboard listener only.** The ingest listener keeps answering 404.
It is the one port published to the internet, this document names the private
dashboard URL and the running version, and everything discovery leads to lives on
the dashboard listener anyway. A client that can only see ingest has nothing to do
with the answer.

## Tokens

Settings > Service Tokens, or as an admin:

```
POST /api/v1/service-tokens
{"name": "teploy-dash", "managed_by": "teploy-dash", "scopes": ["sites:create", "keys:mint"]}
-> 201 {"token": "obs_svc_...", "record": {"id": "...", "name": "...", "managed_by": "...",
        "scopes": [...], "created_by": "...", "created_at": 0, "last_used_at": 0, "revoked_at": 0}}

GET    /api/v1/service-tokens              -> 200 [record, ...]
DELETE /api/v1/service-tokens/{token_id}   -> 200 {"ok": true}
```

The token is shown once and stored as a SHA-256 hash. A signed-in admin is
required even during the first-run grace period, when every other route is open.

| Scope | Allows |
|---|---|
| `sites:create` | `POST /api/v1/service/sites` |
| `keys:mint` | `POST /api/v1/service/sites/{site_id}/keys`, telemetry keys only |
| `share:mint` | `POST /api/v1/service/sites/{site_id}/share` |
| `webhooks:write` | `POST` and `DELETE /api/v1/service/sites/{site_id}/webhooks[/{webhook_id}]` |
| `alerts:read` | `GET /api/v1/service/sites/{site_id}/alerts/rules`, `.../alerts/history` |

### The boundary

There is no scope for users, SQL, stored telemetry or site deletion, and no way to
add one by configuration. Enforcement is structural: a service token authenticates
to `/api/v1/service/*` and nothing else. Every other route still takes a JWT, a
telemetry key or a share token, and a service token is none of those, so a route
added later cannot accept one by forgetting a check.
`TestServiceTokenIsRefusedEverywhereElseOnTheRealBinary` boots the binary and sweeps
its whole route table to hold this.

**Ownership.** Sites belong to a manager name, not to a token. `managed_by` defaults
to the token's name, so a replacement token with the same manager name keeps its
sites after the old one is revoked. Every `{site_id}` route is limited to the
manager's own sites, and answers a foreign site exactly like a missing one (404).
Sites an operator created have no manager and cannot be reached; there is no
adoption path yet. This covers `share:mint`, `webhooks:write` and `alerts:read`
too, because a share link on someone else's site would be a telemetry read, and a
webhook there an outbound feed.

**Share links are still a read credential.** `share:mint` lets a token hold read
access to its own sites' dashboards at one remove. That is how machine reads stay
on one credential instead of growing another. Grant it only to a client that should
have that.

**Audit.** Writes record `started` before they run and the result after; if the
first write fails, the call returns 503 and changes nothing. Every refusal is
recorded. Successful reads are not, since `alerts:read` is meant to be polled and
`last_used_at` shows the token is alive. Actor type is `service`, actor is the token
id, and no row holds a credential or request body.

## Service API

All calls: `Authorization: Bearer obs_svc_...`. Bodies are decoded strictly, so an
unknown field is a 400. Errors are `{"error": "..."}`.

| Status | Meaning |
|---|---|
| 401 | missing, invalid or revoked token |
| 403 | token lacks the scope |
| 404 | site not found or not this manager's |
| 503 + `Retry-After` | store or audit unavailable; nothing changed, retry. Not a verdict on the token |

### Create a site by external reference

```
POST /api/v1/service/sites
{"external_ref": "app:shop", "name": "Shop", "domain": "shop.example.com"}
-> 201 {"created": true,  "site": {"site_id": "...", "tenant_id": "default",
        "domain": "shop.example.com", "name": "Shop", "created_at": 0,
        "raw_distinct_id": false, "external_ref": "app:shop", "managed_by": "teploy-dash"}}
-> 200 {"created": false, "site": {...same site...}}
```

`external_ref` is required (at most 256 printable bytes), and is unique per manager:
another manager's identical ref is a different site. `domain` is optional and `name`
defaults to the ref. A replay returns the site unchanged; it does not rewrite name
or domain. If the site was deleted, the ref is free and the next call makes a new
site with a new id.

### Mint a telemetry key

```
POST /api/v1/service/sites/{site_id}/keys
{"label": "shop-production"}
-> 201 {"key": "obs_...", "info": {"key_id": "...", "site_id": "...", "label": "shop-production",
        "created_at": "...", "revoked": false, "scopes": ["telemetry"],
        "created_by": "service:<token id>"}}
```

`label` is required. There is no scopes field: publish keys stay with an admin. The
key is shown once. Every call mints a new key, so a client that retries after a lost
response leaves a spare; admins revoke keys in Settings.

### Share links, webhooks, alerts

```
POST /api/v1/service/sites/{site_id}/share
{"ttl_days": 30}
-> 201 {"id": "...", "token": "...", "site_id": "...", "created_at": 0, "expires_at": 0, "status": "active"}

POST /api/v1/service/sites/{site_id}/webhooks
{"name": "deploys", "webhook_type": "http", "url": "https://...", "severities": "", "kinds": ""}
-> 201 {"webhook_id": "...", "site_id": "...", "secret": "<shown once>", ...}

DELETE /api/v1/service/sites/{site_id}/webhooks/{webhook_id}
-> 200 {"ok": true}

GET /api/v1/service/sites/{site_id}/alerts/rules           -> 200 [rule, ...]
GET /api/v1/service/sites/{site_id}/alerts/history?limit=  -> 200 [entry, ...]
```

`ttl_days` defaults to 30 and accepts up to 3650. History `limit` defaults to 20 and
caps at 200. Webhook destinations are checked the same way as admin-created ones,
including the dial-time block on private addresses.

## Environment contract for an app

What a platform sets on an app once it holds a key. Send `OTEL_*` to OpenTelemetry
exporters and `OBSERVE_*` to the Observe SDKs; the Go and Python SDKs read either.

```
OBSERVE_ENDPOINT=<ingest_url>
OBSERVE_API_KEY=<key>                                     # secret

OTEL_EXPORTER_OTLP_ENDPOINT=<ingest_url>
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=x-api-key=<key>                # secret
OTEL_SERVICE_NAME=<app>
OTEL_RESOURCE_ATTRIBUTES=service.version=<version>,deployment.environment=production
```

The OTLP header has to be `x-api-key`: `/v1/*` does not read `Authorization`. Do not
set a site: the key is bound to one, and the SDKs send none when they hold a key. The
browser SDK and the Sentry shim have no environment to read and are unchanged.

The SDKs send a credential only where it was issued for. An explicit endpoint takes
`OBSERVE_API_KEY` only if `OBSERVE_ENDPOINT` is unset or equal, and
`OTEL_EXPORTER_OTLP_ENDPOINT` is used only with its own `x-api-key` header and no key
in code. An OTLP endpoint with no `x-api-key` is treated as another vendor's collector,
and with nothing else set the SDK refuses to start.
