---
title: Post-mortem — r1 Postgres outage from max_wal_size sized against the wrong filesystem
date: 2026-09-16
status: resolved
severity: P1 (2h53m total API outage via Redis MISCONF -> rate-limiter fail-closed, 2,034,194 503s; ~1.78M usage-counter increments lost; 18 units failed; no ledger/backup data loss — archiving caught up, newest off-site backup 5h old)
author: ops
---

# r1 `pg_wal` filled root and Postgres could not recover (2026-09-16)

## Summary

On 2026-09-15 19:02 CEST I raised `max_wal_size` from **2GB to 16GB** on r1 to
stop checkpoint thrash. The measurement behind it was sound. The volume it
assumed was not: `pg_wal` is a **symlink** to a path on the **49 GB root
filesystem**, not the 3.4 TB pool the data directory sits on.

Ten hours later, at **2026-09-16 05:15:56 CEST**, root reached 0 bytes
available. Postgres crashed, and crash recovery could not complete:

```
redo done at 240F/2EFFFFA0
FATAL: could not write to file "pg_wal/xlogtemp.3049031": No space left on device
LOG:  startup process (PID 3049031) exited with exit code 1
LOG:  shutting down due to startup process failure
```

It shut down rather than come up inconsistent. That is correct behaviour and is
the reason this was an outage rather than damage.

Impact was not limited to Postgres-backed routes. The same full root
filesystem that starved `pg_wal` also broke Redis's own persistence: its
background save failed against that disk, tripping `stop-writes-on-bgsave-error`
(MISCONF), which made every Redis write fail. The rate limiter fails **closed**
on a Redis write error, so from that point every request was rejected, not
only the ones touching Postgres. From **03:26 to 06:19 UTC** (05:26–08:19
CEST) — **2h53m** — the **whole API returned 503**: **2,034,194** 503
responses, and zero successful `/v1/price` requests anywhere in the window.
Behind that, **18 systemd units** failed (every Postgres-dependent job:
rollups, gap-fills, completeness, directory sync, the archive verifiers).
`stellarindex-api`, the indexer, the aggregator, ClickHouse and Caddy all stayed
running the whole time — it was the rate limiter's fail-closed behaviour, not
a crash in any of them, that turned a Postgres-and-Redis capacity problem into
total API downtime.

**No ledger, price or backup data was lost.** WAL archiving was fully caught
up at the moment of the crash (624 segments in `archive_status`, **0
`.ready`**), and the newest off-site backup was 5 hours old. The exception is
usage accounting: about **1.78M** usage-counter increments were lost in the
window — in-process counters, not queued, dropped for the outage's duration.

## Root cause

One unchecked sentence, in the comment justifying the change:

> "The disk cost is nothing — pg_wal sits on a dataset with 2.7TB free."

That is true of `/var/lib/postgresql/15/main` and false of
`/var/lib/postgresql/15/main/pg_wal`, which is a symlink to
`/pgwal/15-main/pg_wal` on `/dev/md1` — the 49 GB root filesystem, shared with
`/usr`, `/var/log`, `/root` and a 16 GB swapfile.

The volume was **inferred from where the data lives** rather than measured with
`df` against the path WAL is actually written to. Every other number in the
change was measured: 14,282 requested checkpoints against 2,630 timed over 71
days, the 8x full-page-image multiplier, the 12.7 GB of WAL from a single
1.5 GB chunk decompression. The one quantity nobody measured is the one that
mattered.

Root, at the moment of failure:

| | |
|---|---:|
| `/swap_f1209` | 16.0 GB (24 MB in use, on a 188 GB box) |
| `/pgwal` | 9.8 GB (was ~2 GB before the change) |
| `/var` | 7.9 GB |
| `/usr` | 7.6 GB |
| `/root` | 4.9 GB |
| **available** | **0** |

## Timeline (CEST)

| when | what |
|---|---|
| 2026-09-15 19:02 | ansible applies `max_wal_size = 16GB`; SIGHUP, no restart |
| 2026-09-16 05:15:56 | root hits 0 bytes; Postgres crashes and fails recovery |
| 05:26 (03:26 UTC) | Redis's own background save fails against the same full root fs; it enters MISCONF (`stop-writes-on-bgsave-error`) and refuses writes; the rate limiter fails closed on the resulting Redis errors and the **whole API** starts returning 503 |
| 05:27 (03:27 UTC) | the page fires; `chat-page` routes every severity to Discord only, no phone/paging leg — nobody sees it |
| 08:05 | v0.83.0 deploy fails at the migration step — `connection refused` on 5432. This is how the outage was found |
| 08:14 | diagnosis: `/pgwal` is on root, root is 100% full |
| 08:17 | 1.7 GB reclaimed (rotated logs older than `.1`, journal vacuumed to 200 MB); `max_wal_size` reverted to 2GB |
| 08:19 (06:19 UTC) | Postgres started; recovery completes; continuous aggregates resume; Redis exits MISCONF; API returns 200 — 2,034,194 503s served over 2h53m, zero successful `/v1/price` requests in the window |
| 08:20 | 18 failed units reset |
| 08:24 | `pg_wal` has trimmed 9.8 GB → 2.1 GB on its own; root back to 9.5 GB free |

The self-trim is the clearest confirmation of cause available: lowering the
setting returned **7.7 GB** of root within minutes, with nothing else changed.

## What went wrong beyond the setting

**A one-hop cascade turned a Postgres capacity problem into a total outage.**
The full root filesystem did not just starve `pg_wal` — it also broke Redis's
own persistence: Redis's background save failed against the same disk and it
entered MISCONF (`stop-writes-on-bgsave-error`), refusing all writes. The
API's rate limiter fails **closed** on a Redis write error, so every request
was rejected from that point, not only the ones reading Postgres. That is why
the outage was total (2,034,194 503s, zero `/v1/price` 200s) rather than
scoped to the handful of Postgres-backed routes.

**Detection worked. Delivery did not.** This was written first as "nothing
watched root", and that was wrong — checked properly, three root-filesystem
alerts already existed, and the page-tier one fired long before the crash:

| alert | threshold | severity | fired |
|---|---|---|---|
| `stellarindex_node_root_disk_full` | < 10% free | **page** | **2026-09-15 23:46 UTC (01:46 CEST)** |
| `stellarindex_node_root_disk_filling_fast` | `predict_linear` to zero in 30 min | **page** | **03:27 UTC (05:27 CEST)**, as the API's 503s began |
| `stellarindex_node_root_disk_warning` | < 20% free | ticket | earlier still |

The page fired **3 h 29 min before Postgres died** and kept firing through the
outage. The predictive alert did exactly what it was built for.

What failed is the leg after that. `chat-page` in
`/etc/prometheus/alertmanager.yml` carries only `discord_configs` — no
PagerDuty, no OpsGenie, no Pushover — which the alerts catalog already states
in as many words: *"there is no PagerDuty leg ... so nothing wakes anyone up"*.
Pages fired into a chat channel at 01:46 and again at 03:27, as the API itself
went down.

So the ten hours are not a detection gap to be closed with another rule. They
are the known, documented state of the paging path, demonstrated in production
against a real P1. That is what launch-plan row 1.4's SEV drill exists to
prove, and this incident proved the negative for free.

**The runbook the page pointed at was the right one.** All three root-disk
alerts carry `runbook_url: .../redis-write-blocked-disk-full.md` — a
procedure about Redis MISCONF stop-writes on a full disk. That is exactly the
mechanism that made the outage total: Redis's failed background save is what
tripped the rate limiter's fail-closed path. Anyone who opened that document
on the 01:46 or 03:27 page would have been pointed straight at the actual
user-facing failure mode. The catalog's per-alert runbooks were correct;
nobody read one because nobody was paged.

**`max_wal_size` is SIGHUP.** A bad value reaches a running database on reload,
with no restart to think twice about, and surfaces hours later as an outage
rather than immediately as a failed start.

**The gate that did work.** The deploy's config-apply gate refused to apply
v0.83.0's config surfaces afterwards — which was correct and protective: the
v0.83.0 tag still carried `16GB`, so applying it would have repeated the
outage. The revert landed after the tag.

## What changed

1. **`max_wal_size` back to 2GB**, on the host and in
   `roles/archival-node/templates/postgresql.conf.j2`.
2. **A pre-flight guard** in `roles/archival-node/tasks/05-postgres.yml`. It
   **resolves the symlink** before measuring — that substitution is the whole
   defect — and refuses to template a config whose `max_wal_size` does not fit
   the volume it lands on. Existing WAL counts toward the budget, because a
   directory already holding segments is not short by what it holds; it is
   spent on the thing being sized. It asks for **2x** the value, since Postgres
   treats `max_wal_size` as a target for the checkpoint interval rather than a
   hard cap on the directory, and both a lagging checkpoint and a stalled
   archiver overshoot it.

   Run against r1's real numbers it refuses 16GB (needs 32,768 MB against
   11,724 MB available to WAL) and passes 2GB (needs 4,096 MB). It would have
   stopped this at the apply.

3. **The comment now states the correction**, quoting the sentence that caused
   the outage, so the next person to size this reads why the obvious reasoning
   was wrong rather than repeating it.

## Still open

- **`pg_wal` is on the wrong volume.** The checkpoint problem that motivated
  the original change is real and unfixed, and the answer is not a bigger
  number while WAL lives on a 49 GB volume shared with the OS. Moving `pg_wal`
  onto `data/postgres` (2.8 TB free) fixes the class rather than the instance,
  and would have made the original change harmless.
- **Nothing wakes anyone.** `chat-page` is Discord-only. Detection is fine and
  a real page proved it — twice, at 01:46 and again at 03:27 as the API itself
  went down; the delivery path is the gap, and it is the same gap launch-plan
  row 1.4 (SEV drill) is meant to exercise. A page nobody receives is
  indistinguishable from no page at all, which is exactly how this was first
  written up. A phone-paging receiver is still an open decision (owner:
  maintainer).
- **Nothing paged on the failure mode that actually caused the outage.** There
  was no signal on the rate limiter failing closed or on Redis refusing
  writes — those metrics didn't exist. In progress, on branches, not yet
  shipped: `stellarindex_ratelimit_fail_closed_total` with a page rule on it,
  `stellarindex_redis_command_errors_total{class}`, and a widened Redis
  OOM/READONLY alert that also catches MISCONF.
- **Usage-counter loss during an outage was invisible.** The ~1.78M increments
  dropped in this window weren't counted anywhere.
  `stellarindex_usage_units_dropped_total` is in progress, on a branch, not
  yet shipped.
- **The 16 GB swapfile** holds 24 MB on a 188 GB host with 131 GB available. It
  is a third of the root filesystem doing nothing.

## The lesson, stated so it transfers

A path is not a volume. `df` the path the write actually lands on, not the
directory beside it — especially where a symlink separates them, which is
exactly where the assumption feels safest to make.
