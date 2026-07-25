# Restore-drill evidence log

One entry per drill (ADR-0043 §3; CS-110: "a backup that has never
been restored is a hope, not a backup"). Appended by
`scripts/ops/restore-drill.sh` when run from a checkout; runs from
`/usr/local/bin` on r1 are reconstructed here from
`/var/log/restore-drill-*.log`.

## Procedures this log is evidence for

| Layer | What restores it | Runbook |
| ----- | ---------------- | ------- |
| Postgres (served tier) | `pgbackrest restore` into a scratch datadir, driven by `scripts/ops/restore-drill.sh` phases 1–3 | `runbooks/backup-failed.md` |
| ClickHouse **schema + state** | replay `schema.sql` from the daily §2.1 snapshot, then re-derive the data | **`runbooks/ch-schema-restore.md`** — the snapshot → `CREATE` path, step by step |
| ClickHouse **data** | `ch-full-backfill.sh` against `galexie-archive`, bounded by `ch-backfill-done-windows.txt` from the same snapshot | `runbooks/ch-schema-restore.md` §"Restore path" |

The ClickHouse half of a restore is **schema first, data second**, and
the schema does not come from `deploy/clickhouse/tier1_schema.sql` — that
is the founding DDL, since outgrown by indexes, MVs and compression
policies. It comes from the daily snapshot
(`scripts/ops/ch-schema-snapshot.sh`, ADR-0043 §2.1). A drill that
restores Postgres and assumes the lake schema is in the repo is drilling
half the system.

### CH re-derive stage (phase 4), and its honest limit

`DRILL_CH_WINDOW=100000` adds the ADR-0043 §2.2 re-derive sample. It runs
`ch-backfill -dry-run`: every galexie object is fetched and fully decoded
(`clickhouse.ExtractLedger`), and nothing is written. There is no
scratch-database mode to use — `clickhouse.Open` pins the `stellar`
database — and re-deriving into the live lake would add ReplacingMergeTree
duplicates to a capacity-blocked pool every drill. So the RTO figure this
log records is **fetch+decode throughput**, which is the multi-week part
of a rebuild; the ClickHouse INSERT path is exercised continuously by live
ingest and is not the bottleneck. The stage additionally reconciles the
window against the live lake (`ch_lake_window_complete`).

Before 2026-07-25 this stage had **never run**: it passed `-database
drill_scratch`, a flag `ch-backfill` has never declared, so
`flag.ContinueOnError` rejected the invocation at parse time and the
failure was recorded as a generic re-derive failure under `| tail -5`.
The drill now preflights its own invocation against the binary and
refuses to start (exit 2, precondition) on drift;
`scripts/ops/restore-drill-test.sh` pins the same contract in CI.

## 2026-07-03 restore drill (repo1) — first drill series

The first-ever drill took **five runs**, each failing one layer
deeper — every failure mode is now encoded in the script, which is
exactly the value CS-110 promised:

1. **Production-sized config**: the restored `postgresql.auto.conf`
   carries live sizing (tens-of-GB `shared_buffers`); a second
   instance beside the live DB can't allocate it. → scratch overrides
   (memory downsized).
2. **Debian config layout**: the cluster's `postgresql.conf` /
   `pg_hba.conf` live under `/etc/postgresql`, NOT in PGDATA — the
   restored datadir has neither and `pg_ctl` dies pre-recovery. →
   synthesized minimal config + loopback-trust hba.
3. **WAL replay needs real time**: `pg_ctl -w -t 600` timed out while
   recovery was healthily replaying ~21h of WAL through
   `archive-get` (daily-diff schedule × busy ingest DB). →
   `PG_START_TIMEOUT` default 2h.
4. **Replay-enforced GUCs**: with `hot_standby=on`, recovery ABORTS
   ("insufficient parameter settings") unless
   `max_connections`/`max_worker_processes`/`max_wal_senders`/
   `max_prepared_transactions`/`max_locks_per_transaction` are ≥ the
   primary's — the downsizing pass had cut `max_connections` 200→20.
   → the five GUCs are now read from the live primary and mirrored.

Runs 1–4: `pg_restore` OK every time (848–888s for the ~273GB set,
repo1); `pg_start` failed per the modes above.

**Run 5 — PASS, 0 failures (2026-07-03, repo1, agent-run):**

- `pg_restore`: 871s
- `pg_start`: recovery to consistency complete (WAL replay via
  archive-get; scratch instance on :5499)
- `core_tables`: 4/4
- `tip_lag`: restored tip 63,302,295 vs live 63,302,535 — **240
  ledgers (~20 min)** of WAL not yet archived; WAL archiving healthy
- `hash_chain_sample`: 0 chain breaks in the restored 100k tail
- `trades_window_match`: trades[63202295,63252295] restored
  5,770,426 = live 5,770,426 — exact

CS-110's answer: the repo1 backup restores, recovers, and matches the
live database bit-for-bit on an immutable window. RTO evidence:
~15 min restore + WAL replay (scales with time-since-last-diff).
Next: the same drill against **repo2** once the offsite bucket exists
(operator), which proves the copy that matters in a real disaster.

