# syntax=docker/dockerfile:1.7
# Build + runtime image for stellarindex-indexer.
# See docker/README.md for the shared image-shape rationale.

# Base image pinned by immutable digest (supply-chain, DEP-low). Resolved
# 2026-09-30 with `docker buildx imagetools inspect golang:1.26-alpine`
# (multi-platform index digest).
FROM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /src
# Cache modules separately so source-only edits don't invalidate
# the dependency layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -buildvcs=true \
      -ldflags="-s -w \
        -X github.com/Stellar-Index/StellarIndex/internal/version.Version=${VERSION} \
        -X github.com/Stellar-Index/StellarIndex/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/stellarindex-indexer \
      ./cmd/stellarindex-indexer

# Base image pinned by immutable digest (supply-chain, DEP-low). Resolved
# 2026-09-18 with `docker buildx imagetools inspect
# gcr.io/distroless/static-debian12:nonroot` (multi-platform index digest).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/stellarindex-indexer /usr/local/bin/stellarindex-indexer
USER nonroot:nonroot
EXPOSE 9464
ENTRYPOINT ["/usr/local/bin/stellarindex-indexer"]
