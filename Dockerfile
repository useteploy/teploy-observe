# Stage 1: Build the Neutron TS dashboard
FROM node:22-alpine AS ui-builder
RUN corepack enable && corepack prepare pnpm@9 --activate
WORKDIR /ui

# Copy the Neutron TS monorepo (framework packages + observe app)
# In production, this would reference published packages instead
COPY tystack/typescript/package.json tystack/typescript/pnpm-workspace.yaml tystack/typescript/pnpm-lock.yaml ./
COPY tystack/typescript/packages/ ./packages/
COPY tystack/typescript/apps/observe/ ./apps/observe/
RUN pnpm install --frozen-lockfile
RUN pnpm -C apps/observe build

# Stage 2: Build the Go binary
FROM golang:1.24-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src

# Copy Go modules first for layer caching
COPY teploy-observe/go.mod teploy-observe/go.sum ./
COPY tystack/go/ /tystack/go/

# Fix the replace directive to point to the right location
RUN sed -i 's|../../tystack/go|/tystack/go|' go.mod
RUN go mod download

# Copy source code
COPY teploy-observe/ ./

# Copy built UI from stage 1
COPY --from=ui-builder /ui/apps/observe/dist/ ./cmd/observe/ui/dist/

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /observe ./cmd/observe/

# Stage 3: Minimal runtime image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go-builder /observe /usr/local/bin/observe

EXPOSE 3000
ENV OBSERVE_ADDR=:3000
ENV OBSERVE_NUCLEUS_URL=postgres://nucleus:5432/observe

ENTRYPOINT ["observe"]
