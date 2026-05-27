# Dependency Inventory

Generated 2026-05-26T21:43:30Z.

## Direct Go deps (from `go.mod`)

```
	github.com/BurntSushi/toml v1.6.0 // TOML parser for config/ + metadata/sep1.go
	github.com/alicebob/miniredis/v2 v2.37.0 // In-memory Redis for ratelimit/ tests (test-only)
	github.com/golang-migrate/migrate/v4 v4.19.1 // Schema migrations; cmd/ratesengine-migrate (ADR-0006)
	github.com/lib/pq v1.12.3 // Postgres driver (ADR-0006)
	github.com/prometheus/client_golang v1.23.2 // /metrics + counters/gauges in internal/obs
	github.com/redis/go-redis/v9 v9.19.0 // Redis client (ADR-0007) — rate-limit + SEP-1 cache
	github.com/testcontainers/testcontainers-go v0.42.0 // Integration-test Postgres container
	github.com/testcontainers/testcontainers-go/modules/postgres v0.42.0 // Timescale-flavoured container helper
	golang.org/x/sync v0.20.0 // singleflight for metadata/cache.go
	cloud.google.com/go/bigquery v1.77.0
	github.com/aws/aws-sdk-go-v2 v1.36.5
	github.com/aws/aws-sdk-go-v2/config v1.29.17
	github.com/aws/aws-sdk-go-v2/credentials v1.17.70
	github.com/aws/aws-sdk-go-v2/service/s3 v1.83.0
	github.com/coder/websocket v1.8.14
	github.com/google/uuid v1.6.0
	github.com/prometheus/client_model v0.6.2
	golang.org/x/crypto v0.50.0
	google.golang.org/api v0.278.0
	gopkg.in/yaml.v3 v3.0.1
	cel.dev/expr v0.25.1 // indirect
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.20.0 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.2.8 // indirect
	cloud.google.com/go/compute/metadata v0.9.0 // indirect
	cloud.google.com/go/iam v1.7.0 // indirect
	cloud.google.com/go/monitoring v1.24.3 // indirect
	cloud.google.com/go/storage v1.62.0 // indirect
	dario.cat/mergo v1.0.2 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp v1.31.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric v0.55.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping v0.55.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/apache/arrow/go/v15 v15.0.2 // indirect
	github.com/aws/aws-sdk-go v1.49.6 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.6.11 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.32 // indirect
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.17.83 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.36 // indirect
...
```

## Frontend deps (per package.json)

### `web/dashboard`

```
  "dependencies": {
  "devDependencies": {
```
### `web/explorer`

```
  "dependencies": {
  "devDependencies": {
```
### `web/status`

```
  "dependencies": {
  "devDependencies": {
```
