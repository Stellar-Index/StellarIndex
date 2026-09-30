---
title: Redis writes blocked — disk full → MISCONF stop-writes
last_verified: 2026-08-29
status: living procedure
severity: P1
---

# Redis writes blocked — disk full → MISCONF stop-writes

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_redis_writes_blocked` (P1, `severity: page`) — the primary detector; fires on Redis's own bgsave status regardless of whether the aggregator is actively writing. `stellarindex_aggregator_cache_write_errors` (page) corroborates when the aggregator is refreshing but CANNOT fire on its own while the aggregator is idle — no attempted write, no counted error. `stellarindex_ratelimit_fail_closed` (page, `api.yml`) is the escalation: once the rate limiter's own Redis calls have failed past the fail-open dwell time, it fails CLOSED and the WHOLE API starts 503ing, not only `/v1/price`. |
| Severity | P1 |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `stellarindex_redis_writes_blocked`: `redis_rdb_last_bgsave_status == 0`, `for: 60s`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. Corroborating: `aggregator.yml`'s `sum(rate(stellarindex_aggregator_vwap_cache_write_errors_total[5m])) > 0`, `for: 2m`; and `api.yml`'s `stellarindex_ratelimit_fail_closed` once the rate limiter itself starts failing closed. |
| Typical MTTR | 5–10 min once root cause is confirmed (free disk space → Redis re-enables writes automatically) |
| Impact | VWAP cache writes fail → `/v1/price` on rewritten or proxy-served pairs starts 404'ing because the cache key was never written — customer-visible from the first write refusal. **If Redis stays unreachable past the rate limiter's fail-open dwell time (30s), the limiter fails CLOSED and the impact widens to the WHOLE API 503ing** on every rate-limited path, not only `/v1/price` — see `ratelimit-fail-open.md`. |

Companion to [`db-disk-full.md`](db-disk-full.md). Different
mechanism, same root cause: when `/` fills up, Redis can't write
RDB snapshots; with the default `stop-writes-on-bgsave-error yes`
it then refuses every subsequent write. The aggregator's VWAP
cache writes all fail, and `/v1/price` on rewritten or
proxy-served pairs starts 404'ing because the cache key was
never written.

This runbook captures the 2026-05-10 incident on r1 and the
recovery sequence, so the same shape recovers in 5 minutes
next time instead of taking diagnosis.

## Signal

Aggregator log filling with WARN at every refresh tick:

```
{"level":"WARN","msg":"refresh failed",
 "pair":"native/fiat:USD","window":300000000000,
 "err":"redis set vwap:native:fiat:USD:300: MISCONF Redis is
 configured to save RDB snapshots, but it's currently unable to
 persist to disk. Commands that may modify the data set are
 disabled, because this instance is configured to report errors
 during writes if RDB snapshotting fails (stop-writes-on-bgsave-error
 option). Please check the Redis logs for details about the RDB
 error."}
```

User-facing symptoms:

- `/v1/price?asset=X&quote=Y` 404s `price-not-found` for any pair
  whose VWAP is served from the Redis cache (rewritten /
  triangulated / stablecoin-proxy paths).
- Pairs that fall through to `prices_1m` directly still serve.
- `flags.stale` does NOT fire (this isn't an aggregator-down
  state; the aggregator is running, it just can't write).

## Triage (1 min)

```sh
# 1. Confirm Redis writes are blocked
ssh root@136.243.90.96 'redis-cli SET test:probe x'  # → MISCONF Redis is configured to save RDB...

# 2. Confirm root FS at 100%
ssh root@136.243.90.96 'df -h /'

# 3. Check Redis log for rdbSaveRio errors
ssh root@136.243.90.96 'tail -20 /var/log/redis/redis-server.log'
# Expect: "Write error saving DB on disk(rdbSaveRio): No space left on device"
```

Once all three confirm, you're in this incident shape.

## Recovery (5 min)

The fix is two steps in order: (a) free disk, (b) trigger a
successful BGSAVE so Redis clears the `stop-writes-on-bgsave-error`
flag.

### 1. Free disk on `/`

The 2026-05-10 incident on r1 found 35 GB of stale logs on a
49 GB root filesystem. The lowest-risk reductions, in order:

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

Target: get `/` to <80% used (10 GB+ free).

### 2. Unblock Redis writes

Once disk has space:

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

### 3. Confirm aggregator recovered

```sh
# Aggregator log: WARN cadence should drop to ~0 within 30s
ssh root@136.243.90.96 'journalctl -u stellarindex-aggregator --since "30 seconds ago" -o cat \
  | grep -c "refresh failed"'
# Expect: 0 (was firing 15+ per 30s pre-fix)

# Probe a previously-broken price endpoint via the public API
curl -sS "https://api.stellarindex.io/v1/price?asset=native&quote=<USDC-classic-asset_id>"
# Expect: 200 with a fresh observed_at within the last few minutes
```

Once both pass, the customer-visible side is restored. The
underlying disk-full state may still need addressing — see
`db-disk-full.md` for postgres-side considerations and
follow-up rotation policy.

## Why this happens

Redis defaults to `stop-writes-on-bgsave-error yes`. This is the
right choice for a primary data store (you don't want to silently
accept writes that will be lost on restart), but Stellar Index uses
Redis as a CACHE — every value in it is reproducible from the
trades hypertable on demand. The conservative default still
applies: a long sustained block protects the operator from
issuing reads against a Redis whose dataset has fallen out of
sync with what aggregator computed.

The trade-off the operator can make:

- Keep `stop-writes-on-bgsave-error yes` (default) — block writes
  on disk-full, surfacing the incident loudly.
- Set `stop-writes-on-bgsave-error no` — accept writes regardless
  of snapshot state. Cache loses durability across restarts but
  the aggregator stays able to serve. Reasonable for our
  Cache-only role.

We've kept the default for now. If repeated disk-full incidents
happen the change is one `CONFIG SET` away.

## Prevention

- ~~Disk-usage alert at 85% on `/`~~ — **CLOSED**: root-FS alerts
  now exist in BOTH trees (`storage.yml`):
  `stellarindex_node_root_disk_warning` (< 20 % avail),
  `stellarindex_node_root_disk_full` (< 10 % avail), and
  `stellarindex_node_root_disk_filling_fast` (predict_linear) —
  the runway signal this item asked for.
- ~~Logrotate retention pass~~ — **CLOSED**: codified in
  `configs/ansible/roles/archival-node/tasks/15-log-discipline.yml`
  (lines 11–95): rsyslog logrotate drop-in caps the live syslog at
  `maxsize 100M` with 7 gzip-compressed rotations, plus a journald
  `SystemMaxUse=500M` cap. The 8.6 GB syslog.1 class (rotate-14,
  no compress) can't recur on an ansible-applied host.
- ~~Dedicated dir for WASM-audit captures~~ — **CLOSED**: the
  wasm-audit procedure
  ([`../wasm-audits/README.md`](../wasm-audits/README.md) §2, "Where
  walk output goes") writes every walk's JSON, stderr and checkpoint
  JSONL under `/var/log/wasm-audit/` and deletes them once the audit
  log records the timeline, so the free-disk step above targets that
  dir's stderr captures (plus any legacy loose
  `/var/log/wasm-history-*.stderr`).

## Related runbooks

- [`db-disk-full.md`](db-disk-full.md) — postgres-side disk pressure.
- [`redis-master-down.md`](redis-master-down.md) — different shape:
  Redis process exited rather than rejecting writes.
- [`redis-memory.md`](redis-memory.md) — memory pressure (eviction
  policies + maxmemory); `stellarindex_redis_write_rejected_oom` is
  that runbook's write-refusal alert (OOM/READONLY/NOREPLICAS) — a
  different mechanism from this one's MISCONF/disk-full, not a
  duplicate.
- [`ratelimit-fail-open.md`](ratelimit-fail-open.md) — where this
  incident escalates to if Redis stays unreachable past the rate
  limiter's own dwell time: `stellarindex_ratelimit_fail_closed`,
  whole-API 503s.
- `internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`
  — the customer-facing post-mortem for the founding incident
  (embedded in the binary; served via `/v1/incidents`).

## Changelog

- 2026-09-30 — closed the wasm-audit log-dir Prevention item: the
  procedure now writes walk captures under `/var/log/wasm-audit/`;
  the free-disk step removes that dir's stderr captures after a
  `pgrep -af` running-walk check.
- 2026-09-27 — corrected "Detected by" / Alert: led with
  `stellarindex_redis_writes_blocked` (fires on Redis's own bgsave
  status) rather than `stellarindex_aggregator_cache_write_errors`
  (cannot fire while the aggregator is idle — no attempted write, no
  counted error); widened Impact to note the whole-API 503 escalation
  via `stellarindex_ratelimit_fail_closed` once Redis stays
  unreachable past the rate limiter's dwell time.
- 2026-08-29 — re-verified against HEAD (Wave I). Detected-by
  made dual-tree with the r1 overlay primary and the real expr
  quoted with its `sum()` (`for: 2m`, `severity: page`); commands
  moved to r1 ssh shapes; Prevention updated — the logrotate item
  is CLOSED (15-log-discipline.yml caps syslog at 100M/7 gzip
  rotations + journald 500M) and the root-FS alert item is CLOSED
  (`stellarindex_node_root_disk_{warning,full,filling_fast}` in
  both storage.yml trees); wasm-audit log-dir item remains open.
  Added `severity: P1` frontmatter and the embedded post-mortem
  cross-link.
