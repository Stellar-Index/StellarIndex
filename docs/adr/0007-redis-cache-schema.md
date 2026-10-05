---
adr: 0007
title: Redis as hot-path cache + rate-limit + ephemeral state
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0007: Redis as hot-path cache + rate-limit + ephemeral state

## Context

The p95 <= 200 ms API SLA is out of reach if every `/v1/price` read hits a Postgres continuous
aggregate (5-20 ms each; saturates the primary at 2,000 rps). The API also needs expiring per-key rate
counters, SEP-1 and asset-metadata caches, and an SSE subscriber registry, all key-value with TTL.

## Decision

One Redis serves every cache, rate-limit and ephemeral-state workload; no second Redis, Memcached or
KV. HA is Sentinel, not Cluster (ADR-0024). Key schema:

| Key | Purpose | TTL | Writer |
| --- | --- | --- | --- |
| `price:<asset_id>` | latest price, `/v1/price` hot path | 60 s | aggregator |
| `vwap:<base>:<quote>:<window>` | precomputed VWAP | the window | aggregator |
| `ohlc:<base>:<quote>:<granularity>:<bucket>` | one candle | 1 h open; none once closed | aggregator |
| `rl:<api_key>:<min>`, `rl:<ip>:<min>` | rate-limit counters | 120 s | api |
| `toml:<domain>` | parsed stellar.toml | 15 min | api, lazy |
| `meta:<asset_id>` | asset metadata | 5 min | api lazy, indexer invalidates |
| `sub:<channel>:<subscriber_id>` | SSE liveness | 60 s, heartbeat-renewed | api |
| `div:<asset_id>` | divergence result | 5 min | divergence worker |
| `health:<source>` | per-source freshness | 60 s | indexer |

The instance is also the store of record for three TTL-less families: `apikey:` credentials (plaintext
is unrecoverable, so an evicted record is a lost key), `apikey-index:` and `signup:email:`. Persistence
is AOF every second plus a nightly 03:00 UTC RDB shipped to MinIO. Max-memory policy is `volatile-lru`
(applied to running instances by the redis-sentinel role; r1 runs `noeviction`), so only TTL-bearing
keys are eviction candidates. The `usage:<subject>:<day>` meter has a 35-day TTL and so is evictable;
the monthly quota reads each day as the larger of that key and the `usage_daily` rollup (ok + 4xx).

Failure modes: a miss falls back to Timescale and repopulates (cold p95 <= 50 ms); a full outage serves
Timescale directly with `stale_flag=true`; a wiped Redis is re-warmed from Timescale in about 2 min and
rate counters reset; a Sentinel failover takes 15-30 s, with affected keys falling back to Timescale
and `stale_flag=true`.

## Invariant

Every cache family carries a TTL; a family written without one is a store-of-record decision.
Callers build keys through the typed helpers in `internal/cachekeys`, never raw strings. Rate counters
increment atomically (Lua `INCR`+`EXPIRE`).

## Consequences

The hot path meets the SLA with one Redis to monitor, atomic rate limiting and CDN-pinned closed
candles. Costs: possible stale reads during failover (bounded by 60 s TTLs), a small abuse window when
rate counters reset on a wipe, and Redis 7+. In-process LRU, Memcached, Postgres UNLOGGED tables,
DragonflyDB/KeyDB and no cache at all were rejected.

## Evidence

HA topology in [docs/architecture/ha-plan.md](../architecture/ha-plan.md); the redis-sentinel Ansible
role applies the eviction policy.
