---
title: Runbook — redis-memory
last_verified: 2026-08-29
status: current
severity: P2
---

# Runbook — `stellarindex_redis_memory_saturated` / `_evictions_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_redis_memory_saturated` (> 90 % memory), `stellarindex_redis_evictions_high` (> 100/s) |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/cache.yml` (group `stellarindex.cache`, both alerts `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/cache.yml`. |
| Typical MTTR | 30 min (scale-up) – hours (cleanup / policy change) |
| Impact | Depends on `maxmemory-policy` (see Quick diagnosis step 0): under `allkeys-lru`, eviction — hot keys may get knocked out; cache hit-rate drops; API falls back to Timescale more often → elevated p95/p99 latency; rate-limit counters can get evicted early → some clients get fresh quotas. Under `noeviction`, WRITE ERRORS instead of evictions — cache writes and rate-limit INCRs start failing. |

## Symptoms

- `redis_memory_used_bytes / redis_memory_max_bytes > 0.90` for ≥ 5 min, **or**
- `rate(redis_evicted_keys_total[5m]) > 100` for ≥ 5 min.
- API panel: `stellarindex_sep1_cache_ops_total{result="miss"}`
  rate climbs; hit-rate drops.
- Latency alert may follow if the miss storm hits a popular asset.

## Quick diagnosis (≤ 5 min)

```sh
# Step 0 — what does hitting the cap actually DO here?
# r1's ansible only codifies maxmemory (see below), NOT
# maxmemory-policy — only the (unapplied) multi-host
# redis-sentinel role sets allkeys-lru. If this reports
# `noeviction` (the Redis default), the failure mode at the cap
# is WRITE ERRORS, not evictions.
ssh root@136.243.90.96 'redis-cli config get maxmemory-policy'

# Current memory usage + maxmemory policy
ssh root@136.243.90.96 "redis-cli info memory | grep -E 'used_memory_human|maxmemory_human|maxmemory_policy'"

# What's filling it up? `--bigkeys` samples one key per type.
ssh root@136.243.90.96 'redis-cli --bigkeys'

# Eviction / hit-rate stats (r1 is a single instance — there are
# no redis-0/1/2 shards)
ssh root@136.243.90.96 'redis-cli info stats | grep -E "evicted_keys|keyspace_misses|keyspace_hits"'

# Is it one oversized key or many small ones?
ssh root@136.243.90.96 'redis-cli --memkeys'   # requires redis-cli 6.0+
```

## Typical root causes

1. **Legitimate growth — more assets cached than the box is sized
   for.** Symptom: steady climb over days/weeks, eviction rate
   grew alongside.
   - Mitigation: scale up `maxmemory` (if host has headroom) or
     scale out (shard). On r1 the codified cap is
     `maxmemory {{ redis_maxmemory | default('1gb') }}` written
     into `/etc/redis/redis.conf` by
     `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml`
     (tag `redis`) — bump `redis_maxmemory` in the inventory and
     re-apply that role. (The `redis-sentinel` role's
     `redis.conf.j2` is the UNAPPLIED multi-host shape — editing
     it changes nothing on r1.)

2. **Key-explosion bug.** A handler writes per-request cache keys
   without TTL, or with overly long TTLs, and the key-space
   balloons.
   - Signal: `info keyspace` shows a `dbN:keys=...` count way
     larger than distinct assets × 2 (the rough theoretical
     ceiling: price per pair + sep1 resolver per issuer + rate-
     limit counter per API key).
   - Mitigation: identify the bad writer (probably a recent PR),
     add a TTL + cap, deploy, then `FLUSHDB` or let LRU handle it.

3. **Someone used it as a queue / list.** Redis's `LPUSH` /
   streams patterns without bounds can grow unbounded.
   - Signal: `memkeys` / `--bigkeys` shows a single key (stream /
     list) dominating memory.
   - Mitigation: cap with `MAXLEN ~` on streams, or truncate the
     list manually, or move that workload elsewhere. Redis isn't
     our queue.

4. **Very large rate-limit window counters** (sliding-window log
   impl that stores one element per request per key). The SHIPPED
   limiter is a fixed-window INCR+EXPIRE counter
   (`internal/ratelimit/` — one small integer per subject per
   window; NOT a token bucket, NOT a sliding log). If keys
   matching the rate-limit prefix are ~KB each, something replaced
   that implementation with a sliding-log variant.
   - Signal: keys matching the rate-limit prefix are ~KB each.
   - Mitigation: revert to the fixed-window INCR+EXPIRE
     implementation in `internal/ratelimit/`.

## Mitigation

- [ ] Step 1 — figure out which pattern is growing (diagnosis above).
- [ ] Step 2 — if key-explosion: rollback / fix the writer. Don't
      just `FLUSHDB` to "reset" — the bug will refill it. Fix the
      source first.
- [ ] Step 3 — if legitimate growth: scale up. `maxmemory` bumps
      are zero-downtime if Redis has host headroom
      (`CONFIG SET maxmemory <new>` for the live instance, then
      persist by bumping `redis_maxmemory` in the ansible
      inventory and re-applying the archival-node role's `redis`
      tag — r1 config is ansible-managed; a hand-only change WILL
      page on the weekly drift check).
- [ ] Step 4 — if a single big key: delete it or cap it
      (`UNLINK <key>` is non-blocking; `DEL` blocks).
- [ ] Verification: memory drops under 80 %, eviction rate back to
      baseline (roughly 0 during normal ops for a right-sized cache).

## Root cause analysis

- Keyspace growth curve over 30 days (dashboard).
- Which key prefix(es) grew — `--bigkeys` is a snapshot; ideally
  run `SCAN 0 MATCH <prefix>:*` across prefixes to count per
  namespace.
- Who shipped the growth pattern (git blame on the writer).
- Is the alert threshold (90 %) still right or is the box simply
  underprovisioned?

## Known false-positive patterns

- **Just after a deploy** that adds a new cache namespace: brief
  spike as the cache warms; subsides once LRU prunes cold entries.
  Should not cross the 5-min `for:` threshold.
- **Cleanup job side-effect**: large `SCAN + DEL` sweeps create
  brief memory spikes (the returned keys list). Bounded — ignore.

## Related

- `redis-master-down.md` — OOM-kill is a common cause of this
  escalating into a full master outage.
- `api-latency.md` — downstream effect when eviction hits popular
  keys.
- ADR-0007 (key schema + TTL conventions).

## Changelog

- 2026-04-23 — initial draft.
- 2026-08-29 — re-verified against HEAD: token-bucket rollback
  fiction fixed (the shipped limiter is fixed-window INCR+EXPIRE),
  "persist to the ConfigMap" replaced with the real r1 cap
  (`redis_maxmemory` via the archival-node role's
  15-log-discipline.yml, tag `redis`), maxmemory-policy check
  added as step 0 (NOT codified on r1 — `noeviction` means write
  errors, not evictions), fictional redis-0/1/2 shards replaced
  with the single r1 instance, dual-tree Detected-by. Status
  draft → current.
