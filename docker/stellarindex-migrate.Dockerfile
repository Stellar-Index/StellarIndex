# syntax=docker/dockerfile:1.7
# Build + runtime image for stellarindex-migrate.
# See docker/README.md for the shared image-shape rationale.

# Base image pinned by immutable digest (supply-chain, DEP-low). Resolved
# 2026-10-05 with `docker buildx imagetools inspect golang:1.27-alpine`
# (multi-platform index digest).
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder
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
      -o /out/stellarindex-migrate \
      ./cmd/stellarindex-migrate

# Base image pinned by immutable digest (supply-chain, DEP-low). Resolved
# 2026-09-18 with `docker buildx imagetools inspect
# gcr.io/distroless/static-debian12:nonroot` (multi-platform index digest).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/stellarindex-migrate /usr/local/bin/stellarindex-migrate
# F-1227 (codex audit-2026-05-12): the migrate binary defaults
# `-migrations migrations`, so a runtime image that copies only the
# binary cannot apply schema out of the box — `stellarindex-migrate
# up` exits with "open migrations: no such file or directory" before
# touching the DB. Bake the migrations into the image so the default
# subcommand works without a bind-mount + flag. The Ansible role
# already syncs `/usr/local/share/stellarindex/migrations` and passes
# `-migrations` explicitly; that path keeps working in parallel.
COPY migrations/ /migrations/
WORKDIR /
USER nonroot:nonroot
# no listening port
ENTRYPOINT ["/usr/local/bin/stellarindex-migrate"]
