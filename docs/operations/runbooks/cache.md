---
title: Runbook — cache (Redis) alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — Redis cache

Rules: `configs/prometheus/rules.r1/cache.yml` (what r1 loads) and `deploy/monitoring/rules/cache.yml` (multi-host twin), group `stellarindex.cache`, `component: cache`. The two trees are identical except where a section says otherwise.

**r1 posture.** ONE Debian-packaged `redis-server` on the archival host: no Sentinel, no replicas, no `cache-01..03`. The `redis-sentinel` ansible role is NOT applied to archival nodes (comment in `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml`). Everything labelled "multi-host" below is the future ADR-0024 / `docs/architecture/ha-plan.md` §3.4 shape (1 primary + 2 replicas on `cache-01..03`, `redis-server.service` + `redis-sentinel.service`).

r1 first look:

```sh
ssh root@136.243.90.96 "systemctl status redis-server --no-pager | head -15"
ssh root@136.243.90.96 "journalctl -u redis-server -n 100 --no-pager"
```

Redis loss costs cache warmth, not data: `/v1/price` entries re-derive from Timescale; rate-limit counters (~1 min TTL) reset; API keys and SEP-10 sessions are backed by Timescale (`internal/auth/`, `internal/platform/`). Related: `redis-write-blocked-disk-full.md` (`stellarindex_redis_writes_blocked`, MISCONF bgsave failure), `api-latency.md`, `docs/adr/0007-redis-cache-schema.md`.

## At a glance

- [`stellarindex_redis_master_down`](#stellarindex_redis_master_down)
- [`stellarindex_redis_memory_saturated`](#stellarindex_redis_memory_saturated)
- [`stellarindex_redis_evictions_high`](#stellarindex_redis_evictions_high)
- [`stellarindex_redis_write_rejected_oom`](#stellarindex_redis_write_rejected_oom)
- [`stellarindex_redis_replication_broken`](#stellarindex_redis_replication_broken)
- [`stellarindex_redis_sentinel_textfile_stale`](#stellarindex_redis_sentinel_textfile_stale)

## stellarindex_redis_master_down

- **Trips:** `redis_up == 0`, `for: 30s`, `severity: page` (SEV-1). `redis_exporter` emits no `role` label, so the rule watches bare `redis_up` (exporter could/couldn't reach the server).
- **Impact:** hot-path cache for `/v1/price` gone; rate limiter fails open (no throttling, abuse window); clients served via Timescale with `stale=true` and higher latency. Degraded SLA, not an outage. MTTR 1–15 min (Sentinel failover < 1 min; manual longer).
- **Signals:** API latency up; `stellarindex_ratelimit_fail_open_total` jumps (the deliberate Redis-outage signal, fail-open by design per HA plan §3.4); API logs "connection refused" / "pool exhausted".

**Diagnose (r1):** the two commands in the preamble; restart with `ssh root@136.243.90.96 systemctl restart redis-server`. Check `redis_exporter` health first: if only the exporter died, this pages on a phantom outage.

**Diagnose (multi-host):**

```sh
for h in cache-01 cache-02 cache-03; do
  echo -n "$h: "
  redis-cli -h $h -a "$REDIS_PASSWORD" ping
done
redis-cli -h cache-01 -p 26379 -a "$REDIS_PASSWORD" SENTINEL masters
ssh root@cache-01 "systemctl status redis-server --no-pager | head -15"
ssh root@cache-01 "journalctl -u redis-server -n 100 --no-pager"
ssh root@cache-01 "systemctl status redis-sentinel --no-pager | head -10"
```

Root causes:

1. Sentinel mid-failover (`for: 30s` can page as it resolves). Wait a poll; `SENTINEL masters` shows the new master and the alert clears.
2. OOM-kill of Redis on the host (host memory pressure; `maxmemory` is independent of the kernel's view). `dmesg | grep -i oom`.
3. Persistence stall: AOF rewrite / RDB save blocks `fork()`. `redis-cli info persistence` shows `rdb_last_bgsave_status:err` or an `aof_rewrite` running > 60 s.
4. Partition between API and master (Prometheus in the same partitioned zone sees `redis_up` 0; Sentinel fails over if it sees the master).

**Fix (multi-host).** Sentinel failover is the default: `down-after-milliseconds=5000` + `failover-timeout=60000`, so 15–30 s with no operator action; `go-redis/v9` `FailoverClient` rediscovers the primary, no app restart.

1. Hold while failover is in progress. Current primary: `redis-cli -h cache-01 -p 26379 -a "$REDIS_PASSWORD" SENTINEL get-master-addr-by-name stellarindex-r1-cache`. `stellarindex_redis_sentinel_primary` (multi-host only, absent on r1) sums to 1 across hosts in steady state.
2. Check clients reconnected: API/aggregator log `redis configured` with `mode=sentinel` at startup; no "connection refused" after failover; `ratelimit_fail_open_total` rate returns to 0 (watch the rate; it is cumulative).
3. Manual failover ONLY if automatic failover has not completed within 60 s OR `SENTINEL ckquorum` reports < 2 alive sentinels. Confirm: `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL ckquorum stellarindex-r1-cache` and `SENTINEL master stellarindex-r1-cache` (`last-ok-ping-reply` > 10000 ms or `flags` containing `s_down,o_down`). Force: `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL failover stellarindex-r1-cache`. Forcing on a transient blip can split-brain if Sentinels rejoin and disagree.
4. If the primary host is gone, Sentinel already promoted; restore it as a fresh replica via the host-bringup runbook: `ansible-playbook --tags redis --limit cache-X`.
5. Verify the new primary: `redis-cli info replication` shows every follower `lag` 0–1; `redis_up == 1` on every instance.

RCA: Sentinel log order `+sdown`, `+odown`, `+new-epoch`, `+switch-master`; Redis logs of old and new master; host OOM/load/network; whether a rolling restart respected Sentinel quorum.

False positives: rolling restart of the current primary (apt upgrade via the `redis-sentinel` role with `--limit`; one host at a time, per the role README) trips this ~30 s; muting for planned maintenance is fine.

## stellarindex_redis_memory_saturated

- **Trips:** `(redis_memory_used_bytes / redis_memory_max_bytes) * 100 > 90`, `for: 5m`, `severity: ticket` (P2). Same expr in both trees. MTTR 30 min (scale-up) to hours (cleanup / policy).
- **Impact depends on `maxmemory-policy`.** r1's archival-node role pins `maxmemory-policy` to `noeviction` (`redis_maxmemory_policy`, `15-log-discipline.yml`; protects the TTL-less `apikey:` credential records, GH #1317): at the cap, writes fail (cache writes, rate-limit INCRs) instead of evicting. The multi-host `redis-sentinel` role default is `volatile-lru`: only TTL-bearing keys evicted (`apikey:` never), hot keys can be knocked out, hit rate drops, API falls back to Timescale (higher p95/p99), rate-limit counters evicted early give fresh quotas.

**Diagnose (r1; single instance, no redis-0/1/2 shards):**

```sh
ssh root@136.243.90.96 'redis-cli config get maxmemory-policy'
ssh root@136.243.90.96 "redis-cli info memory | grep -E 'used_memory_human|maxmemory_human|maxmemory_policy'"
ssh root@136.243.90.96 'redis-cli --bigkeys'    # samples one key per type
ssh root@136.243.90.96 'redis-cli info stats | grep -E "evicted_keys|keyspace_misses|keyspace_hits"'
ssh root@136.243.90.96 'redis-cli --memkeys'    # redis-cli 6.0+; one big key or many small?
```

Root causes:

1. **Legitimate growth** (steady climb over days/weeks; evictions grow with it). Raise the cap: r1's is `maxmemory {{ redis_maxmemory | default('1gb') }}` written to `/etc/redis/redis.conf` by `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml` (tag `redis`). Live: `CONFIG SET maxmemory <new>` (zero-downtime with host headroom), then persist by bumping `redis_maxmemory` in the inventory and re-applying the archival-node role's `redis` tag; a hand-only change WILL page on the weekly drift check. The `redis-sentinel` role's `redis.conf.j2` is unapplied on r1; editing it changes nothing there.
2. **Key explosion** (a handler writes per-request keys without TTL or with long TTLs). `info keyspace` shows a `dbN:keys=...` count far above distinct assets × 2 (price per pair + sep1 resolver per issuer + rate-limit counter per API key). Find the writer (recent PR), add TTL + cap, deploy. Do NOT `FLUSHDB` first: the bug refills it.
3. **Redis used as queue/list.** `--memkeys`/`--bigkeys` shows one stream/list dominating. Cap with `MAXLEN ~`, truncate, or move the workload. Delete a single big key with `UNLINK <key>` (non-blocking; `DEL` blocks).
4. **Bloated rate-limit counters.** The shipped limiter is a fixed-window INCR+EXPIRE counter (`internal/ratelimit/`; one small integer per subject per window; not a token bucket or sliding log). Keys of ~KB under the rate-limit prefix mean someone replaced it: revert to fixed-window INCR+EXPIRE.

Verify: memory under 80 %, eviction rate back to baseline (~0 for a right-sized cache). RCA: 30-day keyspace growth curve; which prefix grew (`SCAN 0 MATCH <prefix>:*` per namespace; `--bigkeys` is a snapshot); git blame on the writer; is 90 % still the right threshold or is the box underprovisioned.

False positives: post-deploy spike when a new cache namespace warms (should not outlast `for: 5m`); a `SCAN + DEL` cleanup sweep causes a brief bounded spike.

## stellarindex_redis_evictions_high

- **Trips:** `rate(redis_evicted_keys_total[5m]) > 100`, `for: 5m`, `severity: ticket` (P2). Both trees.
- **Means:** LRU pressure sustained; API hit rate drops, Timescale fallback load rises; latency alerts may follow if a popular asset's keys are evicted. Under r1's `noeviction` this alert cannot fire; see `stellarindex_redis_write_rejected_oom`.
- **Diagnose / fix:** same commands, root causes and verification as `stellarindex_redis_memory_saturated`.

## stellarindex_redis_write_rejected_oom

- **Trips:** `rate(redis_errors_total{err=~"OOM|READONLY|NOREPLICAS"}[5m]) > 0`, `for: 2m`, `severity: page`. `redis_errors_total{err=...}` is the prometheus-redis-exporter counter fed by INFO ERRORSTATS. Both trees (comments differ; expr identical).
- **Means:** Redis refuses writes outright (not degraded); application writes are failing now. This is the loud counterpart to evictions under `noeviction`.
  - `OOM`: at `maxmemory`, nothing left to evict (`noeviction`, or on volatile-lru only TTL-less keys remain) or cannot evict fast enough. Go to `stellarindex_redis_memory_saturated` diagnosis and causes 1–4.
  - `READONLY`: instance stopped accepting writes. Single-node r1: config drift (`replica-read-only`) or a script/transaction against a stale role. Multi-host: stale primary handoff; `redis-cli info replication` for `role:` and confirm Sentinel agrees.
  - `NOREPLICAS`: `min-replicas-to-write` refusing because too few replicas are connected (ADR-0024 1-primary-2-replica). Check `redis_connected_slaves`; if low, this is the `stellarindex_redis_replication_broken` event seen from the write side.
- Deliberately NOT widened to `MISCONF` (`stellarindex_redis_writes_blocked`, `redis-write-blocked-disk-full.md`, already pages it; a second rule would double-page) nor `EXECABORT` (client-side MULTI queueing error, not a server refusing writes).

## stellarindex_redis_replication_broken

- **Trips (the trees DIFFER by design):** `deploy/monitoring/rules/cache.yml`: `redis_connected_slaves < 2`, `for: 2m`, `severity: ticket` (P2), against the ADR-0024 1-primary-2-replica topology. `configs/prometheus/rules.r1/cache.yml`: `redis_connected_slaves < on(instance) redis_expected_slaves`, `for: 2m`, `severity: ticket`. `redis_expected_slaves` has no producer anywhere (F-1329), so the r1 form never fires: **the alert is INERT on r1** (single node, no replicas). Drop the r1 form when r1 grows replicas. Everything below is the multi-host shape.
- **Impact:** none immediate (reads/writes continue on the master), but Sentinel needs a healthy replica to promote; without one a master failure becomes a full cache outage (`stellarindex_redis_master_down`). MTTR 15–45 min.
- **Signals:** `connected_slaves:` below configured in `redis-cli info replication` on the master; Sentinel logs `+sdown slave ...`.

Diagnose (cache-01..03; run on the master and each replica host):

```sh
redis-cli -h <master> info replication
redis-cli -h <replica> info replication | grep -E 'role|master_link_status|slave_read_only'
redis-cli -h <replica> info replication | grep -E 'master_sync_in_progress|master_sync_total_bytes|master_sync_left_bytes'
redis-cli -h cache-01 -p 26379 sentinel replicas stellarindex-r1-cache
```

Causes and fixes:

1. Replica process died (hardware/OOM/crash): `ssh root@cache-NN "systemctl restart redis-server"`; Sentinel re-adds it on its next discovery cycle.
2. Replica behind the repl-backlog, doing a full sync (counted connected but `master_link_status:down` can flap; `master_sync_in_progress:1`). Wait; ETA = `master_sync_left_bytes` / network bandwidth. If it never completes, bump both `repl-backlog-size` and `client-output-buffer-limit replica`.
3. Network flapping: master log shows repeated `Connecting to MASTER ... / Partial resynchronization not possible`. Diagnose MTU, packet loss, firewall between zones.
4. Auth drift after secret rotation (replica log `NOAUTH Authentication required`; `requirepass` / `masterauth` not updated): restart the replica with the correct secret.

Verify: master `connected_slaves` back to the expected count; Sentinel's replica list all healthy. RCA: master, replica and Sentinel logs (sdown/odown) across the window; any network/firewall change.

False positives: rolling `systemctl restart redis-server` across cache hosts (one at a time via the `redis-sentinel` role with `--limit`) can trip `for: 2m` on a slow rollout; adding a third replica gives `connected_slaves` one short during initial sync.

## stellarindex_redis_sentinel_textfile_stale

- **Trips:** `time() - node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/redis_sentinel.prom"} > 600`, `for: 5m`, `severity: ticket` (P3). Both trees. 10 m tolerates ~20 missed 30 s runs. Multi-host only (the producer belongs to the `redis-sentinel` role). MTTR 5–15 min.
- **Means:** `redis-sentinel-textfile-scraper.timer` (`OnUnitActiveSec=30s`, `configs/ansible/roles/redis-sentinel/tasks/07-monitoring.yml`; runs `/usr/local/bin/redis-sentinel-textfile-scraper`) stopped rewriting `redis_sentinel.prom`. node_exporter re-serves a textfile's last values regardless, so `stellarindex_redis_sentinel_primary` alone can never detect the scraper's death; it is frozen, and a primary re-election during the outage is invisible. Same mechanism as `stellarindex_config_assertions_stale` ([config-assertion-failed](config-assertion-failed.md)).

```sh
systemctl status redis-sentinel-textfile-scraper.timer
systemctl status redis-sentinel-textfile-scraper.service
journalctl -u redis-sentinel-textfile-scraper.service -n 50
/usr/local/bin/redis-sentinel-textfile-scraper   # run by hand, inspect the error
```

Common causes: `REDIS_PASSWORD` missing from `/etc/default/redis_exporter` (`EnvironmentFile=-`, so the unit starts but the scraper fails auth), or `redis-cli SENTINEL get-master-addr-by-name` failing because Sentinel itself is down.

## Related

- [Alerts catalogue](../alerts-catalog.md)
