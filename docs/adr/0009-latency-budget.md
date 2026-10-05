---
adr: 0009
title: API latency budget
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0009: API latency budget

## Context

The API commits to p95 <= 200 ms and p99 <= 500 ms end to end (TLS at HAProxy to client) on the public
REST endpoints. Without a per-component allocation those numbers are aspirational and drift is found
only after it lands. Cold storage is never on the request path (ADR-0008).

## Decision

The hot-path endpoints `/v1/price`, `/v1/vwap`, `/v1/twap` and `/v1/ohlc` are budgeted per component.

Cache-warm path, p95 (total 65 ms, leaving 135 ms headroom against 200 ms): TLS plus HAProxy 5 ms,
ingress middleware 5 ms, auth (apikey Redis HGET or SEP-10 verify) 10 ms, handler validation 5 ms,
Redis lookup of `price:<asset_id>` (ADR-0007) 20 ms, marshalling plus envelope 5 ms, network egress
15 ms.

Cache-cold path, p99 (total 125 ms, leaving 375 ms headroom against 500 ms): the warm-path components
45 ms, the Timescale `LatestClosedVWAP1mForPair` query 60 ms, non-blocking cache write-back 20 ms.

Aggregation, indexer ingest, cross-region replication, Hubble cross-checks, WASM audits and divergence
monitoring are asynchronous and contribute zero; a handler calling any of them synchronously violates
the budget and ADR-0008.

Other endpoints get their own budgets, recorded in handler tests as `// budget: pXX = N ms`:

| Endpoint family | p95 | p99 |
| --- | --- | --- |
| `/v1/price`, `/v1/vwap`, `/v1/twap`, `/v1/ohlc`, `/v1/oracle/lastprice` | 200 ms | 500 ms |
| `/v1/price/batch` (up to 100 assets) | 500 ms | 1000 ms |
| `/v1/history` (recent ranges) | 500 ms | 1500 ms |
| `/v1/history/since-inception` | 5 s | 15 s |
| `/v1/healthz`, `/readyz`, `/version` (no DB) | 5 ms | 20 ms |

## Invariant

The hot path gains no new middleware or remote round-trip without fitting inside the 65 ms warm
budget or recording an explicit extension; caches below the Redis layer are allowed.

Latency alerts fire above the budget so only sustained slowdowns page: `stellarindex_api_latency_p95_high`
at p95 > 500 ms and `stellarindex_api_latency_p99_high` at p99 > 2 s, each sustained 10 min, on
`http_request_duration_seconds_bucket` per route. Budget and alert thresholds change together.

The SLA load test at 2,000 rps on cache-served endpoints blocks a release that misses 200/500 ms.
Handlers on novel paths should assert latency in their unit test.

## Consequences

Every PR has a yardstick, and the 135 ms headroom is the budget for the degradation tax (envelope
flags, source list, `as_of`). The budget is deliberately conservative (65 ms steady state against a
200 ms promise) and auth costs 10 ms unconditionally; both are open to review with real traffic.
Alert rules and `docs/operations/alerts-catalog.md` stay in lockstep (doc-lint enforces the symmetry).
A stricter 100 ms target and a budget-free global limit were rejected.

## Evidence

`internal/obs/metrics.go` histograms; alerts in
[docs/operations/alerts-catalog.md](../operations/alerts-catalog.md); launch-checklist load test in
[docs/architecture/ha-plan.md](../architecture/ha-plan.md).
