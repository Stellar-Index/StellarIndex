# Q5 — Test-coverage map + concurrency (race)

**Date:** 2026-06-15 · **Method:** `go test -race -coverprofile -covermode=atomic ./...`
across the whole tree (gap-closure #52 + #53).

## Concurrency (#52)

`go test -race ./...` — **0 data races** across every package. (Notable because
the audit's direct-to-main commits bypassed the PR-only race job; this run
exercised them.) `go test` exit 0.

## Coverage (#53)

**Total: 49.0% of statements** (unit tests only — `go test ./...`; the
`integration`-tagged suites are NOT counted here, so the storage/decoder layer's
*real* coverage is meaningfully higher than the unit numbers below).

Lowest-coverage packages (unit) and the honest read on each:

| pkg | unit cov | read |
|---|---|---|
| cmd/stellarindex-migrate | 0.0% | CLI glue / main() — covered by the integration migration round-trip, not unit |
| internal/dispatcher/statsflush | 0.0% | small; worth a unit test |
| internal/platform/postgresstore | 0.4% | security-relevant (Postgres key/account store) — **integration-tested** (testcontainers), but thin on unit; a few pure-logic unit tests would help |
| cmd/* (api/indexer/aggregator/ops) | 1.9–13% | mostly main() wiring — integration/e2e territory, low unit value |
| internal/storage/{clickhouse,timescale} | 10% | **heavily integration-tested** (the round-trip + storage_test); unit number is misleading |
| internal/sources/forex | 11.8% | decode/scale logic IS unit-testable — genuine gap, good target |
| internal/pipeline, internal/projector | 28% | partly integration; projector's new panic-isolation path is unit-tested (this session) |
| internal/xdrjson | 33.8% | the new explorer decoder — field-decoded op types are tested; the helpers + not-decoded fallback could use more |

## Disposition

The coverage MAP is the deliverable (gaps named above). Rather than chase a
headline %, every fix shipped in this remediation pass came with a regression
test: sep41 shape matrix, explorer composite cursor, login throttle, projector
`processEventSafely` panic isolation, SDK Pagination round-trip, S3-cred-env
guard. **Recommended targeted test-adds** (backlog, by value): `sources/forex`
scale/decode, `dispatcher/statsflush`, a few `postgresstore` pure-logic unit
tests, and more `xdrjson` op-body coverage. None are launch-blocking; the
critical paths are covered by the integration suite (which the unit `-cover`
number excludes).
