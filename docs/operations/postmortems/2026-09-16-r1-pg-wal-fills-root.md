---
title: Post-mortem — r1 Postgres outage from max_wal_size sized against the wrong filesystem
date: 2026-09-16
status: resolved
severity: P1 (2h53m total API outage via Redis MISCONF -> rate-limiter fail-closed, 2,034,194 503s; ~1.78M usage-counter increments lost; 18 units failed; no ledger/backup data loss — archiving caught up, newest off-site backup 5h old)
author: ops
---

# r1 `pg_wal` filled root and Postgres could not recover (2026-09-16)

## Summary

On 2026-09-15 19:02 CEST `max_wal_size` on r1 was raised from **2GB to 16GB** to stop checkpoint thrash. The measurement behind it was
sound; the volume it assumed was not. `pg_wal` is a **symlink** (`/var/lib/postgresql/15/main/pg_wal` to `/pgwal/15-main/pg_wal` on `/dev/md1`)
onto the **49 GB root filesystem**, not the 3.4 TB pool holding the data directory. At **05:15:56 CEST** root hit 0 bytes; Postgres crashed and crash recovery
failed (`FATAL: could not write to file "pg_wal/xlogtemp.3049031": No space left on device`). It shut down rather than come up inconsistent: an outage, not damage.

The full root also broke Redis persistence: the background save failed and Redis entered MISCONF (`stop-writes-on-bgsave-error`), refusing all writes. The
API rate limiter fails **closed** on a Redis write error, so from **03:26 to 06:19 UTC (2h53m)** the **whole API returned 503**: **2,034,194** responses, zero
successful `/v1/price` requests. 18 systemd units failed (every Postgres-dependent job). `stellarindex-api`, indexer, aggregator, ClickHouse and Caddy stayed up;
the fail-closed limiter turned a capacity problem into total downtime.

**No ledger, price or backup data was lost.** WAL archiving was caught up (624 segments in `archive_status`, 0 `.ready`); newest off-site backup 5 h old.
About **1.78M** in-process usage-counter increments were dropped for the outage's duration.

## Root cause

One unchecked sentence justified the change: "pg_wal sits on a dataset with 2.7TB free." True of the data directory, false of `pg_wal`. The volume was inferred from
where the data lives, not measured with `df` on the path WAL is written to; every other number was measured (14,282 requested vs 2,630 timed checkpoints over 71 days,
8x full-page-image multiplier, 12.7 GB of WAL from one 1.5 GB chunk decompression). Root at failure: `/swap_f1209` 16.0 GB (24 MB in use on a 188 GB box), `/pgwal` 9.8 GB
(~2 GB before), `/var` 7.9 GB, `/usr` 7.6 GB, `/root` 4.9 GB, available 0.

`max_wal_size` is SIGHUP: a bad value reaches a running database on reload and surfaces hours later as an outage.

## Timeline (CEST)

| when | what |
|---|---|
| 09-15 19:02 | ansible applies `max_wal_size = 16GB`; SIGHUP, no restart |
| 09-16 05:15:56 | root at 0 bytes; Postgres crashes, fails recovery |
| 05:26 (03:26 UTC) | Redis MISCONF; limiter fails closed; whole API 503 |
| 05:27 (03:27 UTC) | page fires; `chat-page` is Discord-only, nobody sees it |
| 08:05 | v0.83.0 deploy fails at migration (`connection refused` on 5432): how the outage was found |
| 08:14 | diagnosis: `/pgwal` on root, root 100% full |
| 08:17 | 1.7 GB reclaimed (rotated logs older than `.1`, journal vacuumed to 200 MB); `max_wal_size` reverted to 2GB |
| 08:19 (06:19 UTC) | Postgres up, recovery completes, caggs resume, Redis leaves MISCONF, API 200 |
| 08:20 / 08:24 | 18 failed units reset; `pg_wal` self-trimmed 9.8 to 2.1 GB, root 9.5 GB free (confirms cause: 7.7 GB back with nothing else changed) |

## Detection worked, delivery did not

Three root-fs alerts existed; the page-tier ones fired long before the crash:

| alert | threshold | severity | fired |
|---|---|---|---|
| `stellarindex_node_root_disk_full` | < 10% free | page | 2026-09-15 23:46 UTC (01:46 CEST) |
| `stellarindex_node_root_disk_filling_fast` | `predict_linear` to zero in 30 min | page | 03:27 UTC (05:27 CEST), as 503s began |
| `stellarindex_node_root_disk_warning` | < 20% free | ticket | earlier |

The page fired 3h29m before Postgres died. `chat-page` in `/etc/prometheus/alertmanager.yml` carries only `discord_configs` (no PagerDuty, OpsGenie or Pushover), as
the alerts catalog states ("nothing wakes anyone up"). All three alerts point at `redis-write-blocked-disk-full.md`, which describes the MISCONF mechanism that made the
outage total; the runbook was right and nobody was paged to read it. This also proved the negative for launch-plan row 1.4 (SEV drill). The v0.83.0 config-apply gate
correctly refused to apply that tag's config, which still carried `16GB`.

## What changed

1. `max_wal_size` back to 2GB on the host and in `roles/archival-node/templates/postgresql.conf.j2`.
2. Pre-flight guard in `roles/archival-node/tasks/05-postgres.yml`: **resolves the symlink** before measuring, counts existing WAL toward the budget, and requires **2x**
   `max_wal_size` (it is a checkpoint target, not a hard cap; a lagging checkpoint or stalled archiver overshoots). On r1's numbers it refuses 16GB (needs 32,768 MB vs 11,724 MB
   available to WAL) and passes 2GB (needs 4,096 MB).
3. The config comment now quotes the sentence that caused the outage.

## Action items and status

- [ ] **`pg_wal` is on the wrong volume.** The checkpoint problem is real and unfixed; move `pg_wal` onto `data/postgres` (2.8 TB free), which fixes the class.
- [ ] **Nothing wakes anyone.** `chat-page` is Discord-only; a phone-paging receiver is an open decision (owner: maintainer). Same gap as launch-plan row 1.4.
- [x] **No signal on rate-limiter fail-closed / Redis refusing writes — shipped 2026-09-27.** `stellarindex_ratelimit_fail_closed_total` counts the transition and
  `stellarindex_ratelimit_fail_closed` (`api.yml`, page) fires on it; `stellarindex_redis_command_errors_total{class}` counts Redis failures by class.
  `stellarindex_redis_write_rejected_oom` (`cache.yml`) widened to `OOM|READONLY|NOREPLICAS`, deliberately **not** MISCONF (`stellarindex_redis_writes_blocked` already pages on
  it; a second rule would double-page) nor EXECABORT (client-side MULTI error). All three have promtool rule tests in `deploy/monitoring/rule-tests/`, including a fixture
  pinning MISCONF as non-paging for the widened rule.
- [x] **Usage-counter loss was invisible — shipped 2026-09-27.** `stellarindex_usage_units_dropped_total{counter}` counts failed counter writes;
  `stellarindex_usage_write_failing` (ticket) alerts on the billable class.
- [x] **16 GB swapfile** (24 MB used on a 188 GB host): resized to 4 GB on 2026-09-29; root usage 70% to 40%.

## Lesson

A path is not a volume. `df` the path the write actually lands on, not the directory beside it, especially across a symlink.
