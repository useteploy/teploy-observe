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

HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=3 \
    CMD wget -q --spider http://localhost:3000/healthz || exit 1

ENTRYPOINT ["observe"]
