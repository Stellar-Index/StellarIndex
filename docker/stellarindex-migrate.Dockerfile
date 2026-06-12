# syntax=docker/dockerfile:1.7
# Build + runtime image for stellarindex-migrate.
# See docker/README.md for the shared image-shape rationale.

FROM golang:1.25-alpine AS builder
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
        -X github.com/StellarIndex/stellar-index/internal/version.Version=${VERSION} \
        -X github.com/StellarIndex/stellar-index/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/stellarindex-migrate \
      ./cmd/stellarindex-migrate

FROM gcr.io/distroless/static-debian12:nonroot
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
