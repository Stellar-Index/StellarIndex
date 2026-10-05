---
title: Restore-drill evidence log
last_verified: 2026-10-05
status: snapshot of the on-box log
---

# Restore-drill evidence log

> **This file is a snapshot, not the live log.** The evidence lives on r1 at
> `/var/lib/stellarindex/restore-drills/restore-drills.md`. Since BDR-03 (2026-08-14)
> `scripts/ops/restore-drill.sh` appends each run to `$RESTORE_DRILL_LOG_DIR/restore-drills.md`
> (default that path), not to this file. If the two disagree, the on-box file is right.

ADR-0043 §3; CS-110: "a backup that has never been restored is a hope, not a backup".

## Reading the on-box log

One block per run, in the shape `record_evidence()` writes:

```text
## YYYY-MM-DD restore drill (repoN)
- restore: <s>s; tip lag <n> ledgers; hash-chain breaks: <n>; trades window match: <restored>=<live>
- CH re-derive (dry-run, fetch+decode only): <window> ledgers in <s>s from <bucket>; lake rows in window: <n>   (only with DRILL_CH_WINDOW)
- note: <DRILL_LOG_NOTE>   (only when DRILL_LOG_NOTE is set)
- failures: <n>
```

`repo1` is the local pgBackRest copy, `repo2` the encrypted off-site copy. A run that stopped early writes
`- ABORTED at <phase>` instead of the restore line. `failures: 0` is a pass; the two checks that catch a corrupt or
partial copy are `hash-chain breaks: 0` and an exact `trades window match`.

```bash
ssh root@136.243.90.96 'tail -n 30 /var/lib/stellarindex/restore-drills/restore-drills.md'
ssh root@136.243.90.96 'grep -c "^## " /var/lib/stellarindex/restore-drills/restore-drills.md'   # runs recorded
```

Schedule: `restore-drill.timer` (repo1, first Saturday of the month, 04:00 UTC) and `restore-drill-offsite.timer`
(`DRILL_REPO=2`, the 15th, 04:00 UTC). Freshness is alerted, not read from this file:
[restore-drill-stale](../runbooks/restore-drill-stale.md), [restore-drill-offsite-stale](../runbooks/restore-drill-offsite-stale.md),
[restore-drill-failed](../runbooks/restore-drill-failed.md).

## Procedures this log is evidence for

| Layer | What restores it | Runbook |
| ----- | ---------------- | ------- |
| Postgres (served tier) | `pgbackrest restore` into a scratch datadir, `scripts/ops/restore-drill.sh` phases 1-3 | `runbooks/backup-failed.md` |
| ClickHouse **schema + state** | replay `schema.sql` from the daily §2.1 snapshot, then re-derive the data | `runbooks/ch-schema-restore.md` |
| ClickHouse **data** | `ch-full-backfill.sh` against `galexie-archive`, bounded by `ch-backfill-done-windows.txt` from the same snapshot | `runbooks/ch-schema-restore.md` §"Restore path" |

Schema comes first and from the daily snapshot (`scripts/ops/ch-schema-snapshot.sh`, ADR-0043 §2.1), not from
`deploy/clickhouse/tier1_schema.sql` (founding DDL, outgrown by indexes, MVs and compression policies).

### CH re-derive stage (phase 4) and its limit

`DRILL_CH_WINDOW=100000` adds the ADR-0043 §2.2 sample: `ch-backfill -dry-run` fetches and fully decodes every galexie object
(`clickhouse.ExtractLedger`) and writes nothing (`clickhouse.Open` pins the `stellar` database, and re-deriving into the live lake
would add ReplacingMergeTree duplicates). The RTO figure it records is therefore **fetch+decode throughput**, the multi-week part of
a rebuild; the INSERT path is exercised by live ingest. It also reconciles the window against the live lake (`ch_lake_window_complete`).
Before 2026-07-25 it had never run (it passed `-database drill_scratch`, a flag `ch-backfill` never declared). The drill now preflights
its own invocation against the binary and exits 2 on drift; `scripts/ops/restore-drill-test.sh` pins that in CI and
`scripts/ops/restore-drill-run-test.sh` drives the abort paths (capacity refusal, restore failure, recovery failure).

## First drill series, 2026-07-03 (repo1)

Five runs, each failing one layer deeper; every mode is now encoded in the script:

1. Production-sized config: restored `postgresql.auto.conf` carries live sizing (tens-of-GB `shared_buffers`) a second instance cannot allocate. Fix: scratch overrides.
2. Debian layout: `postgresql.conf`/`pg_hba.conf` live under `/etc/postgresql`, not PGDATA, so `pg_ctl` dies pre-recovery. Fix: synthesized minimal config + loopback-trust hba.
3. WAL replay needs real time: `pg_ctl -w -t 600` timed out during healthy replay (~21 h of WAL via `archive-get`). Fix: `PG_START_TIMEOUT` default 2 h.
4. With `hot_standby=on`, recovery aborts unless `max_connections`, `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions`, `max_locks_per_transaction` are >= the primary's (downsizing had cut `max_connections` 200 to 20). Fix: read all five from the live primary and mirror.

Runs 1-4: `pg_restore` OK each time (848-888 s for the ~273 GB set); `pg_start` failed per the modes above.
**Run 5 PASS, 0 failures:** `pg_restore` 871 s; 4/4 core tables; restored tip 63,302,295 vs live 63,302,535 (240 ledgers, ~20 min of
unarchived WAL); 0 chain breaks in the 100k tail; trades[63202295,63252295] 5,770,426 = 5,770,426. RTO evidence: ~15 min restore + WAL
replay (scales with time since the last differential).

## All runs since 2026-08-16 (copied from the on-box log; last copy 2026-09-17)

`restore` is `pg_restore` wall-clock; the CH column is the phase-4 sample, requested only by the local Saturday drill.

| date | repo | restore | tip lag | hash-chain breaks | trades window (restored = live) | CH re-derive (dry-run) | failures |
|---|---|---|---|---|---|---|---|
| 2026-08-16 | repo1 | 496 s | 12,527 ledgers | 0 | 2,891,421 = 2,891,421 | — | 1 |
| 2026-08-19 | repo1 | 532 s | 13,392 ledgers | 0 | 5,416,797 = 5,416,797 | — | 1 |
| 2026-08-19 | repo1 | 539 s | 206 ledgers | 0 | 5,905,148 = 5,905,148 | — | **0** |
| 2026-08-24 | repo1 | 535 s | 17 ledgers | 0 | 4,601,258 = 4,601,258 | — | **0** |
| 2026-09-03 | repo2 | 2,813 s (47 min) | 17 ledgers | 0 | 2,726,303 = 2,726,303 | — | **0** |
| 2026-09-05 | repo1 | 607 s | 3 ledgers | 0 | 2,888,781 = 2,888,781 | 100,000 ledgers in 1,893 s (~53 ledgers/s) from `galexie-archive`; 100,000 lake rows in window | **0** |
| 2026-09-15 | repo2 | 2,925 s (49 min) | 68 ledgers | 0 | 5,225,239 = 5,225,239 | — | **0** |

- The two failures (08-16, 08-19) had zero chain breaks and exact trades matches; the large tip lag (12.5k and 13.4k ledgers) was WAL not yet archived, not a bad backup.
- **Local RTO:** ~9-10 min (496-607 s over five scheduled runs; manual 2026-07-03 run 871 s).
- **Off-site RTO (the site-gone case): ~47-49 min of restore plus WAL replay**, reproducible (2,813 s then 2,925 s). Cause: 269 GB of compressed
  repository (242.6 GB full + 26.6 GB differential, 788 GB on disk) pulled from S3 `eu-central-1` to Hetzner; ~$24 AWS egress per drill.
  Never quote the ~15 min repo1 figure for a disaster where the local repository is gone.
- 2026-09-03 repo2 run was the first off-site restore; manual, under `run-heavy-job.sh` (singleton lock, 20 GB memory cap, deprioritised CPU/IO), non-destructive,
  on a dedicated ZFS dataset (4.9 TB free vs 1,038 GB required).

## Still outstanding

- This file goes stale by construction: the drill writes only on the box, so the in-repo copy has been twice reconciled by hand (2026-09-03, 2026-09-17).
  Either the drill writes evidence back to the repo or this file stays an explicit snapshot (current choice). Open inventory item INV-0987.
- The ClickHouse leg is not in the monthly off-site drill; blocked on a CH lake backup target (INV-0987).
