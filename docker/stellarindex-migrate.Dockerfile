# syntax=docker/dockerfile:1.7
# Build + runtime image for stellarindex-migrate.
# See docker/README.md for the shared image-shape rationale.

# Base image pinned by immutable digest (supply-chain, DEP-low); the tag stays
# in the reference for readability only — the digest is what the build pulls.
# Resolved 2026-09-18 with `docker buildx imagetools inspect golang:1.27-alpine`
# (the multi-platform index digest — linux/amd64 + linux/arm64 — resolving to
# 1.27.1-alpine3.24). Dependabot's docker ecosystem bumps it; to refresh by
# hand re-run the inspect and update every Dockerfile in this directory.
FROM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS builder
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
