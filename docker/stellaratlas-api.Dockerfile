# syntax=docker/dockerfile:1.7
# Build + runtime image for stellaratlas-api.
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
        -X github.com/StellarAtlas/stellar-atlas/internal/version.Version=${VERSION} \
        -X github.com/StellarAtlas/stellar-atlas/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/stellaratlas-api \
      ./cmd/stellaratlas-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/stellaratlas-api /usr/local/bin/stellaratlas-api
USER nonroot:nonroot
EXPOSE 3000
ENTRYPOINT ["/usr/local/bin/stellaratlas-api"]
