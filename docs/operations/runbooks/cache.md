---
title: Runbook — cache (Redis) alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — Redis cache

Rules: `configs/prometheus/rules.r1/cache.yml` (r1) and `deploy/monitoring/rules/cache.yml` (multi-host twin), group `stellarindex.cache`.

**r1 posture.** ONE Debian `redis-server` on the archival host: no Sentinel, no replicas. Multi-host sections below are the future ADR-0024 shape ([ha-plan](../../architecture/ha-plan.md) section 3.4: 1 primary + 2 replicas on `cache-01..03`).

```sh
ssh root@136.243.90.96 "systemctl status redis-server --no-pager | head -15"
ssh root@136.243.90.96 "journalctl -u redis-server -n 100 --no-pager"
```

Redis loss costs cache warmth, not data: `/v1/price` re-derives from Timescale, rate-limit counters reset, API keys and sessions live in Timescale. Related: `api.md#stellarindex_api_latency_p95_high`, `docs/adr/0007-redis-cache-schema.md`.

## At a glance

Redis master, memory, replication and sentinel alerts; the page alerts are P1/P2 per section, see [sev-playbook](../sev-playbook.md).

## stellarindex_redis_master_down

**Symptom.** `redis_up == 0`, `for: 30s`, page (SEV-1). Price cache gone, rate limiter fails open (`stellarindex_ratelimit_fail_open_total` jumps), clients served from Timescale with `stale=true`.

**Check.** The two commands above; check `redis_exporter` first (if only the exporter died this is a phantom). Multi-host:

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

**Likely causes.**

- Sentinel mid-failover (`for: 30s` can page as it resolves).
- OOM-kill of Redis (`dmesg | grep -i oom`).
- Persistence stall: `redis-cli info persistence` shows `rdb_last_bgsave_status:err` or an `aof_rewrite` over 60 s.
- Partition between API and master.

**Fix.**

1. r1: `ssh root@136.243.90.96 systemctl restart redis-server`.
2. Multi-host: hold during failover (15-30 s, no operator action). Current primary: `redis-cli -h cache-01 -p 26379 -a "$REDIS_PASSWORD" SENTINEL get-master-addr-by-name stellarindex-r1-cache`.
3. Force only if failover has not completed in 60 s or `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL ckquorum stellarindex-r1-cache` reports fewer than 2 sentinels: `redis-cli -p 26379 -a "$REDIS_PASSWORD" SENTINEL failover stellarindex-r1-cache` (forcing on a blip can split-brain).
4. Dead primary host: restore it as a replica with `ansible-playbook --tags redis --limit cache-X`.
5. Verify `redis-cli info replication` followers `lag` 0-1 and `redis_up == 1`; `ratelimit_fail_open_total` rate returns to 0.

**Escalate:** SEV-1 per [sev-playbook](../sev-playbook.md); silence for a planned rolling restart.

## stellarindex_redis_memory_saturated

**Symptom.** `(redis_memory_used_bytes / redis_memory_max_bytes) * 100 > 90`, `for: 5m`, ticket (P2). r1 pins `maxmemory-policy noeviction` (`redis_maxmemory_policy`, `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml`; protects TTL-less `apikey:` records): at the cap writes fail. Multi-host default `volatile-lru` evicts TTL keys, so hit rate drops and p95 rises.

**Check.**

```sh
ssh root@136.243.90.96 'redis-cli config get maxmemory-policy'
ssh root@136.243.90.96 "redis-cli info memory | grep -E 'used_memory_human|maxmemory_human|maxmemory_policy'"
ssh root@136.243.90.96 'redis-cli --bigkeys'    # samples one key per type
ssh root@136.243.90.96 'redis-cli info stats | grep -E "evicted_keys|keyspace_misses|keyspace_hits"'
ssh root@136.243.90.96 'redis-cli --memkeys'    # redis-cli 6.0+; one big key or many small?
```

**Likely causes.**

- Legitimate growth over days or weeks.
- Key explosion: `info keyspace` count far above assets x 2; a handler writes keys without TTL.
- One list or stream dominating `--memkeys`.
- Rate-limit keys of ~KB: the limiter is a fixed-window INCR+EXPIRE counter (`internal/ratelimit/`), so someone replaced it.

**Fix.**

1. Growth: raise the cap. r1's is `maxmemory {{ redis_maxmemory | default('1gb') }}` in `/etc/redis/redis.conf` (archival-node role, tag `redis`). Live `CONFIG SET maxmemory <new>`, then bump `redis_maxmemory` in inventory and re-apply the `redis` tag (a hand-only change pages on the weekly drift check).
2. Key explosion: find the writer (recent PR), add TTL and cap, deploy. Do NOT `FLUSHDB` first: the bug refills it.
3. Big key: `UNLINK <key>` (non-blocking; `DEL` blocks); cap lists with `MAXLEN ~`.
4. Revert any non-fixed-window limiter.
5. Verify memory under 80 % and evictions back to ~0.

**Escalate:** none (ticket).

## stellarindex_redis_evictions_high

**Symptom.** `rate(redis_evicted_keys_total[5m]) > 100`, `for: 5m`, ticket (P2). Hit rate drops, Timescale load rises. Cannot fire under r1's `noeviction`; see [write_rejected_oom](#stellarindex_redis_write_rejected_oom). Check, causes and fix: as [memory_saturated](#stellarindex_redis_memory_saturated).

## stellarindex_redis_write_rejected_oom

**Symptom.** `rate(redis_errors_total{err=~"OOM|READONLY|NOREPLICAS"}[5m]) > 0`, `for: 2m`, page. Redis refuses writes now. `MISCONF` is deliberately excluded ([writes_blocked](#stellarindex_redis_writes_blocked) pages it).

**Likely causes.**

- `OOM`: at `maxmemory` with nothing evictable; follow [memory_saturated](#stellarindex_redis_memory_saturated).
- `READONLY`: r1 config drift (`replica-read-only`); multi-host stale primary handoff (`redis-cli info replication`, confirm Sentinel agrees).
- `NOREPLICAS`: `min-replicas-to-write` with too few replicas (`redis_connected_slaves`); see [replication_broken](#stellarindex_redis_replication_broken).

**Escalate:** page; SEV per [sev-playbook](../sev-playbook.md).

## stellarindex_redis_replication_broken

**Symptom.** Multi-host: `redis_connected_slaves < 2`, `for: 2m`, ticket. The r1 rule compares against `redis_expected_slaves`, which has no producer, so it is INERT on r1 (`KNOWN_INERT` in `scripts/ci/lint-metric-refs.sh`). Without a healthy replica a master failure becomes a full cache outage ([master_down](#stellarindex_redis_master_down)).

**Check** (master and each replica).

```sh
redis-cli -h <master> info replication
redis-cli -h <replica> info replication | grep -E 'role|master_link_status|slave_read_only'
redis-cli -h <replica> info replication | grep -E 'master_sync_in_progress|master_sync_total_bytes|master_sync_left_bytes'
redis-cli -h cache-01 -p 26379 sentinel replicas stellarindex-r1-cache
```

**Likely causes.**

- Replica process died.
- Full sync in progress (`master_sync_in_progress:1`); if it never completes raise `repl-backlog-size` and `client-output-buffer-limit replica`.
- Network flapping (`Partial resynchronization not possible` in the master log).
- Auth drift after rotation (`NOAUTH`; `requirepass` / `masterauth`).

**Fix.**

1. `ssh root@cache-NN "systemctl restart redis-server"`; Sentinel re-adds it.
2. Auth drift: restart the replica with the correct secret.
3. Verify `connected_slaves` is back to the expected count.

**Escalate:** none (ticket).

## stellarindex_redis_sentinel_textfile_stale

**Symptom.** `time() - node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/redis_sentinel.prom"} > 600`, `for: 5m`, ticket (P3), multi-host only. `redis-sentinel-textfile-scraper.timer` (30 s, `configs/ansible/roles/redis-sentinel/tasks/07-monitoring.yml`) stopped rewriting the file; `stellarindex_redis_sentinel_primary` is frozen, so a re-election is invisible (same mechanism as `stellarindex_config_assertions_stale`, [config-assertion-failed](config-assertion-failed.md)).

**Check.**

```sh
systemctl status redis-sentinel-textfile-scraper.timer
systemctl status redis-sentinel-textfile-scraper.service
journalctl -u redis-sentinel-textfile-scraper.service -n 50
/usr/local/bin/redis-sentinel-textfile-scraper   # run by hand, inspect the error
```

**Likely causes.** `REDIS_PASSWORD` missing from `/etc/default/redis_exporter` (the unit starts, the scraper fails auth); Sentinel itself down.

**Fix.** Restore the password or Sentinel, run the scraper once by hand, confirm the mtime advances. **Escalate:** none.

## stellarindex_redis_writes_blocked

**Symptom.** `redis_rdb_last_bgsave_status == 0`, `for: 60s`, page (P1; `configs/prometheus/rules.r1/storage.yml`). Root FS full, so RDB saves fail and `stop-writes-on-bgsave-error yes` refuses every write (`MISCONF`). VWAP cache writes fail and `/v1/price` on rewritten or proxy-served pairs 404s `price-not-found` (pairs served from `prices_1m` still work; `flags.stale` does not fire). Corroborated by `stellarindex_aggregator_cache_write_errors`; if Redis stays unreachable past the 30 s dwell the limiter fails closed and the whole API 503s ([`api.md#stellarindex_ratelimit_fail_closed`](api.md#stellarindex_ratelimit_fail_closed)). Post-mortem: `internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`.

**Check.**

```sh
# 1. Confirm Redis writes are blocked
ssh root@136.243.90.96 'redis-cli SET test:probe x'  # → MISCONF Redis is configured to save RDB...

# 2. Confirm root FS at 100%
ssh root@136.243.90.96 'df -h /'

# 3. Check Redis log for rdbSaveRio errors
ssh root@136.243.90.96 'tail -20 /var/log/redis/redis-server.log'
# Expect: "Write error saving DB on disk(rdbSaveRio): No space left on device"
```

**Likely causes.** Stale logs, syslog archives or WASM-audit stderr captures filling `/`. Postgres-side: [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full).

**Fix.**

1. Free disk on `/` (on r1, lowest risk first; target under 80 % used):

   ```sh
   # All commands in this step run ON r1 (ssh root@136.243.90.96).

   # Vacuum systemd journal — keep recent 200 MB
   journalctl --vacuum-size=200M

   # Truncate rotated syslog archives
   truncate -s 0 /var/log/syslog.1
   # (also syslog.2.gz, .3.gz etc if present)

   # Remove WASM-audit stderr captures (multi-GB per walk). The walk JSON and
   # checkpoint JSONL are small and may not be committed yet, so they stay.
   # -f is required: the kernel comm is truncated to 15 chars, so a bare
   # `pgrep stellarindex-ops` never matches. Empty output = no walk running.
   pgrep -af 'stellarindex-ops wasm-history'
   rm -f /var/log/wasm-audit/*.stderr /var/log/wasm-history-*.stderr

   # Postgres logs — only truncate if confirmed safe (not actively in use)
   # Prefer `logrotate -f /etc/logrotate.d/postgresql-common` first
   ls -la /var/log/postgresql/

   # Confirm space is back
   df -h /
   ```
2. Unblock writes (a successful BGSAVE clears the flag):

   ```sh
   # Trigger a manual BGSAVE; on success it clears the
   # stop-writes-on-bgsave-error flag automatically
   ssh root@136.243.90.96 'redis-cli BGSAVE'
   # → Background saving started

   # Confirm it succeeded (LASTSAVE timestamp moves to ~now)
   ssh root@136.243.90.96 'redis-cli LASTSAVE'

   # Probe write
   ssh root@136.243.90.96 'redis-cli SET test:probe ok && redis-cli GET test:probe && redis-cli DEL test:probe'
   # → OK / ok / 1
   ```
3. Confirm the aggregator recovered:

   ```sh
   # Aggregator log: WARN cadence should drop to ~0 within 30s
   ssh root@136.243.90.96 'journalctl -u stellarindex-aggregator --since "30 seconds ago" -o cat \
     | grep -c "refresh failed"'
   # Expect: 0 (was firing 15+ per 30s pre-fix)

   # Probe a previously-broken price endpoint via the public API
   curl -sS "https://api.stellarindex.io/v1/price?asset=native&quote=<USDC-classic-asset_id>"
   # Expect: 200 with a fresh observed_at within the last few minutes
   ```
4. `stop-writes-on-bgsave-error no` is one `CONFIG SET` away if disk-full incidents repeat; the default is kept so the block is loud.

**Escalate:** page; SEV per [sev-playbook](../sev-playbook.md). Prevention already codified: `stellarindex_node_root_disk_{warning,full,filling_fast}` in `storage.yml`; logrotate and journald caps in `15-log-discipline.yml`; WASM-audit captures under `/var/log/wasm-audit/` ([`../wasm-audits/README.md`](../wasm-audits/README.md)).

## Related

- [Alerts catalogue](../alerts-catalog.md)
