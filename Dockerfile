# Single-stage build. The Go module vendors neutron-go, so no external
# fetch is needed. The dashboard SPA is committed under
# cmd/observe/ui/dist/ and embedded by `//go:embed all:ui/dist`.
# 1.25, not 1.24: go.opentelemetry.io/proto/otlp (the OTLP protobuf types
# behind /v1/traces, /v1/metrics and /v1/logs) declares go 1.25.0, as do the
# golang.org/x/* versions it pulls in. On 1.24 the vendored build fails.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates
WORKDIR /src

COPY . .

# Vendor mode is automatic when vendor/ is present and go.mod's go
# directive is >= 1.14.  -trimpath strips local paths from the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -mod=vendor \
        -trimpath \
        -ldflags="-s -w" \
        -o /observe \
        ./cmd/observe

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /observe /usr/local/bin/observe

EXPOSE 3000
ENV OBSERVE_ADDR=:3000
ENV OBSERVE_NUCLEUS_URL=postgres://nucleus:5432/observe
ENV OBSERVE_DATA_DIR=/var/lib/observe

VOLUME ["/var/lib/observe"]

# /healthz is not a static 200: it runs `SELECT 1` against Nucleus, which is
# the point (a process that cannot reach its database is not healthy). But that
# means a probe arriving when the pool has no live connection pays the full
# connect, and a cold connect to Nucleus was measured at ~7.8s against 0.00s
# for every pooled hit after it. At a 3s timeout the container flapped to
# `unhealthy` on the first probe after start and again whenever the pool had
# gone idle, while the app was serving normally throughout.
#
# The timeout is sized for the cold-connect case rather than the warm one; it
# stays under the interval, and retries=3 still requires three consecutive
# failures before the container is called unhealthy.
HEALTHCHECK --interval=15s --timeout=12s --start-period=30s --retries=3 \
    CMD wget -q --spider http://localhost:3000/healthz || exit 1

ENTRYPOINT ["observe"]
