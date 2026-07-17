# syntax=docker/dockerfile:1.7
# Build + runtime image for stellarindex-indexer.
# See docker/README.md for the shared image-shape rationale.

# Base image pinned by TAG, not digest. TODO(supply-chain, DEP-low): pin by
# immutable digest — FROM golang:1.26-alpine@sha256:<digest> AS builder.
# Digest NOT inlined: unresolvable offline in this worktree (no registry
# access) and must not be fabricated. Resolve with
# `docker buildx imagetools inspect golang:1.26-alpine` and pin in the same PR.
FROM golang:1.26-alpine AS builder
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

# Base image pinned by TAG, not digest. TODO(supply-chain, DEP-low): pin by
# immutable digest — FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest>.
# Resolve the (offline-unavailable) digest via `docker buildx imagetools
# inspect gcr.io/distroless/static-debian12:nonroot`; pin in the same PR (do NOT fabricate).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/stellarindex-indexer /usr/local/bin/stellarindex-indexer
USER nonroot:nonroot
EXPOSE 9464
ENTRYPOINT ["/usr/local/bin/stellarindex-indexer"]
