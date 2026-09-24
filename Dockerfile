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
# tini is PID 1 (see ENTRYPOINT): it reaps orphaned processes, which observe
# itself never does.
RUN apk add --no-cache ca-certificates tzdata tini
COPY --from=builder /observe /usr/local/bin/observe

# Audit F50: run as a dedicated unprivileged identity instead of the image's
# default root. Only the data directory (WAL queue, backup temp files) must
# be writable; existing volumes chowned by an earlier root-mode deployment
# need a one-time `chown -R 10001:10001 /var/lib/observe` on the host —
# the application deliberately does not chown host paths itself.
RUN addgroup -S -g 10001 observe \
 && adduser -S -D -H -u 10001 -G observe observe \
 && mkdir -p /var/lib/observe \
 && chown observe:observe /var/lib/observe

EXPOSE 3000
ENV OBSERVE_ADDR=:3000
ENV OBSERVE_NUCLEUS_URL=postgres://nucleus:5432/observe
ENV OBSERVE_DATA_DIR=/var/lib/observe

VOLUME ["/var/lib/observe"]

USER 10001:10001

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
#
# wget carries its own -T 10, under Docker's 12s. When Docker's timeout fires
# it kills the probe's shell, not wget; the orphaned wget is re-parented to
# PID 1, and with observe as PID 1 nothing ever reaps it. A live container
# accumulated 1,108 zombie wget processes that way (one per probe that
# outlived the timeout) on its way to PID exhaustion. -T makes wget exit on
# its own first; tini as PID 1 reaps anything that still slips through.
HEALTHCHECK --interval=15s --timeout=12s --start-period=30s --retries=3 \
    CMD wget -q -T 10 --spider http://localhost:3000/healthz || exit 1

ENTRYPOINT ["/sbin/tini", "--", "observe"]
