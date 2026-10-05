---
adr: 0015
title: API rates served from last-closed bucket, never in-progress
status: Accepted
date: 2026-04-27
supersedes: []
superseded_by: null
---

# ADR-0015: API rates served from last-closed bucket, never in-progress

## Context

Regions are never byte-identical at one instant, so a rate computed from "all trades up to now" can differ between regions and flicker for a client hitting rotated DNS.
A closed aggregation window has a fixed identity once it closes, so serving only closed windows makes every region agree.

## Decision

API rate endpoints serve the most recent closed aggregate bucket and never the in-progress one.

- `/v1/price` serves the aggregator's cached VWAP (below) and falls back to the last closed 1-minute bucket in the Timescale `prices_1m` continuous aggregate; with neither it returns 404. There is no client-selectable `window`, and the response carries `observed_at` and `window_seconds`.
- `/v1/ohlc` and the chart/history reads return only closed rows. `/v1/vwap` and `/v1/twap` compute on query from `trades` and clamp `to` to the last closed boundary (`clamped: true`).
- The aggregator's cached VWAP is a rolling window `[bucketEnd - W, bucketEnd)` with `bucketEnd` truncated to the last closed minute (`closedBucket`, `internal/aggregate/orchestrator/orchestrator.go`), default 5 minutes. It lives in Redis only.
- The aggregator writes no CAGG rows. `prices_1m` and its rollups (`prices_15m` through `prices_1mo`) are materialised by TimescaleDB refresh policies over `trades`, declared in `migrations/`.
- The guard is `bucket <= now() - INTERVAL '<granularity>'` (`HistoryGranularity.closedBucketInterval`, `internal/storage/timescale/aggregates.go`); a CAGG row carries only its start, `bucket`.
- The one exception is `/v1/price/tip`, which serves `[now - N, now)` by design (ADR-0018).
- Rejected: syncing replication everywhere, a shared cross-region Redis, and serving the open bucket flagged `provisional`. Per-region independent ingest was rejected here and later adopted by ADR-0050, where this rule is what makes regions agree.

## Invariant

- No served price, OHLC or windowed rate value comes from a bucket that is still filling, apart from `/v1/price/tip`.
- Rate reads clamp their time range to the last closed boundary; `internal/api/v1/closed_bucket_internal_test.go` pins the parameter clamp and `internal/storage/timescale/closed_bucket_guard_test.go` pins the query guard.
- A closed bucket's value never changes after it closes, so any region serves identical bytes for it.

## Consequences

Served rates are up to one bucket (1 minute) old, and fast moves show only when the bucket closes; sub-bucket data comes from the tip and SSE surfaces.
Any region can answer any query, so routing needs no primary affinity.
This rule is the whole consistency mechanism of independent per-region ingest (ADR-0050), which is deferred past v1.0; today one region, R1, is deployed, with no Postgres replication.
The closed-bucket guard is still spelled several ways and not every read goes through one chokepoint (tracked as #689).

## Evidence

`internal/storage/timescale/aggregates.go`, `internal/api/v1/closed_bucket_internal_test.go`, `internal/storage/timescale/closed_bucket_guard_test.go`, `internal/aggregate/orchestrator/closed_bucket_test.go`, and `migrations/0002_create_price_aggregates.up.sql`.
