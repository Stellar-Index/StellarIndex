---
adr: 0024
title: Redis HA via Sentinel (not Cluster)
status: Accepted
date: 2026-04-30
supersedes: []
superseded_by: null
---

# ADR-0024: Redis HA via Sentinel (not Cluster)

## Context

The hot-data set (price cache, VWAP precompute, rate-limit counters, SEP-1 and asset metadata, SSE registry) fits in one primary's RAM. Redis Cluster shards that set for no capacity gain and taxes every multi-key feature.

## Decision

Redis HA is Sentinel: one primary and two replicas on three cache hosts, one Sentinel per host, `quorum=2`. No sharding; each replica holds a full async copy. Persistence is AOF `everysec` plus RDB snapshots. Failover is automatic (target 15-30 s). Clients connect through go-redis `FailoverClient`, which asks Sentinel for the primary; no HAProxy or VIP sits in front. One vault secret authenticates the data plane and Sentinel: `requirepass`, `masterauth` and Sentinel `auth-pass` all share it.

## Invariant

Redis is never sharded: the keyspace lives on a single primary. Config validation requires `redis_master_name` whenever `redis_sentinel_addrs` is set (`internal/config/validate.go`).

## Consequences

- Capacity ceiling is one primary's `maxmemory`; moving to Cluster later is a one-time migration. `stellarindex_redis_memory_saturated` (`deploy/monitoring/rules/cache.yml`) fires above 90% for 5 minutes.
- All writes go to the primary; acceptable because the aggregator's bulk refresh tolerates throughput limits.
- A 2-1 Sentinel partition lets only the larger side promote; `docs/operations/runbooks/cache.md#stellarindex_redis_master_down` covers it.

## Evidence

- Client: `internal/storage/redisclient/redisclient.go` (`NewFailoverClient`).
- Deployment: `configs/ansible/roles/redis-sentinel/defaults/main.yml` (quorum, AOF, RDB).
- Topology: `docs/architecture/ha-plan.md` §3.4.
