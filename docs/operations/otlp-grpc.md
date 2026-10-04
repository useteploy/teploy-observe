# OTLP/gRPC receiver

Observe accepts OTLP over gRPC for traces, metrics and logs in addition to
OTLP/HTTP (`/v1/traces`, `/v1/metrics`, `/v1/logs`), which is unchanged. The
gRPC listener is **off by default**.

| Variable | Default | Meaning |
|---|---|---|
| `OBSERVE_OTLP_GRPC_ADDR` | empty (disabled) | Listen address, e.g. `127.0.0.1:4317`. |
| `OBSERVE_OTLP_GRPC_TLS_CERT` | empty | PEM certificate. Set together with the key, or not at all. |
| `OBSERVE_OTLP_GRPC_TLS_KEY` | empty | PEM private key. |

Without a certificate the listener is plaintext HTTP/2. Keep it on localhost, a
tailnet, or behind a proxy that terminates TLS (Caddy, nginx, an L4 load
balancer); do not publish a plaintext port to the internet. A half-configured
TLS pair or an unbindable address fails startup.

## Authentication

The same API key as the HTTP receivers, sent as gRPC metadata:

    x-api-key: obs_...
    authorization: Bearer obs_...

The key must carry the `telemetry` scope; the site comes from the key, never
from the request. Example exporter settings:

    OTEL_EXPORTER_OTLP_PROTOCOL=grpc
    OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com:4317
    OTEL_EXPORTER_OTLP_HEADERS=x-api-key=obs_...

## Status codes

| Condition | Code | Exporter behaviour |
|---|---|---|
| Missing / unknown / revoked key | `UNAUTHENTICATED` | not retried |
| Key without `telemetry` scope | `PERMISSION_DENIED` | not retried |
| Auth or storage outage | `UNAVAILABLE` + RetryInfo (5s) | retried |
| Per-site rate limit | `RESOURCE_EXHAUSTED` + RetryInfo (1s) | retried after delay |
| Message over 10 MiB (after decompression) | `RESOURCE_EXHAUSTED`, no RetryInfo | not retried |
| Undecodable payload, metrics cardinality refusal | `INVALID_ARGUMENT` | not retried |

Logs report rejected records via `partial_success.rejected_log_records`, as on
HTTP. Traces and metrics are all-or-nothing. gzip compression is supported.

## Limits

10 MiB max message, 256 concurrent streams per connection, keepalive pings no
more often than every 30s, idle connections closed after 10 minutes. Shutdown
is graceful (in-flight exports finish) and bounded by the app shutdown
deadline; the receiver stops before the ingest buffers it feeds.
