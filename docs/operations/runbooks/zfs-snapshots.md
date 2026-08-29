---
title: Runbook — zfs-snapshots
last_verified: 2026-08-29
status: draft
severity: P2
---

# Runbook — `stellarindex_zfs_pool_free_low` / `stellarindex_zfs_pool_free_critical` / `stellarindex_zfs_snapshot_stale` / `stellarindex_zfs_snapshot_pool_free_unreadable`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_zfs_pool_free_low` (P3, ticket) · `stellarindex_zfs_pool_free_critical` (P1, page) · `stellarindex_zfs_snapshot_stale` (P3, ticket) · `stellarindex_zfs_snapshot_pool_free_unreadable` (P3, ticket) |
| Severity | P1 (pool < 1.5 TiB) / P3 (pool < 2.5 TiB; snapshot > 36 h old) |
| Detected by | `deploy/monitoring/rules/zfs-snapshots.yml` (+ the identical `configs/prometheus/rules.r1/zfs-snapshots.yml`) over textfile gauges written by `zfs-snapshot.sh` |
| Typical MTTR | 10–30 min (free space / restart timer); a table restore from a snapshot is 15–90 min depending on size |
| Impact | While a snapshot is stale or skipped, a logical fault in ClickHouse or Postgres (bad migration, `DROP`, bad re-derive) costs a multi-hour pgBackRest restore or a multi-day lake re-derive instead of a clone-and-copy. Below 1.5 TiB the pool itself is at risk for every writer. |

## Why this exists

Decision 2026-08-29. pgBackRest (ADR-0043) protects Postgres off-host
with PITR and the ClickHouse lake is re-derivable from the Galexie
archive — both are hours-to-days recovery paths. Nothing covered the
fast, common failure: a wrong `DROP`, an `ALTER … DELETE` with the
wrong predicate, a migration that rewrote a table badly. A daily ZFS
snapshot of `data/clickhouse` (3 d retention, ~250 GB/day of merged
parts pinned per retained day) and `data/postgres` (7 d, small churn)
turns that into a `zfs clone` + copy-a-table-back, or a `zfs rollback`.

The pool had 5.0 TB free of 18.3 TB when this landed. Snapshots pin
churn, so the job carries a **hard min-free guard** (default 2 TiB,
`zfs_snapshot_min_free_bytes`): below it the job prunes its oldest
`auto-*` snapshots (never a dataset's newest one), and if still below
it **skips** the snapshot and reports
`stellarindex_zfs_snapshot_guard_skipped{dataset}=1`. The guard is the
safety property; the alerts here are its early warning.

## What the job does

`zfs-snapshot.timer` → `zfs-snapshot.service` → `/usr/local/bin/zfs-snapshot.sh rotate`
daily at 01:45 UTC (+ ≤ 5 min jitter), config in `/etc/default/zfs-snapshot`:

1. **Retention** — destroy `auto-YYYYMMDD-HHMM` snapshots older than the
   dataset's window (`data/clickhouse` 3 d, `data/postgres` 7 d).
2. **Min-free guard** — if `zpool list -o free data` < floor, destroy
   `auto-*` snapshots oldest-first across both datasets (by creation
   time, not size — predictable, and postgres snapshots are tiny), one
   at a time re-reading free space, stopping at each dataset's newest.
3. **Snapshot** — if at/above the floor, `zfs snapshot
   <dataset>@auto-YYYYMMDD-HHMM`; else skip + metric.
4. **Metrics** — write `/var/lib/node_exporter/textfile_collector/zfs_snapshot.prom`.

Invariants (pinned by `scripts/ci/zfs-snapshot-test.sh` with a stubbed
`zfs`): only `auto-YYYYMMDD-HHMM` names are ever destroyed; only the
configured datasets are touched; the guard never prunes a dataset's
newest auto snapshot; every `zfs` call is idempotent. **A `manual-*`
or any operator-named snapshot is never destroyed by automation** —
you own its deletion, and a forgotten one pins churn forever (see
`stellarindex_zfs_snapshot_used_bytes`).

## Symptoms

- `stellarindex_zfs_pool_free_low`: `zpool` free < 2.5 TiB for 15 min.
  The guard will begin pruning within days at ~250 GB/day.
- `stellarindex_zfs_pool_free_critical`: free < 1.5 TiB for 5 min. The
  guard has already pruned everything it may and is skipping snapshots.
- `stellarindex_zfs_snapshot_stale`: newest auto snapshot of a dataset
  > 36 h old, or the metric has never been reported (absent branch,
  clickhouse only).
- `stellarindex_zfs_snapshot_pool_free_unreadable`: the last run could
  not read pool free space and refused to act (nothing pruned, nothing
  snapshotted, unit failed).

## Quick diagnosis (≤ 5 min)

```sh
systemctl status zfs-snapshot.timer zfs-snapshot.service
journalctl -u zfs-snapshot.service -n 50 --no-pager      # "SKIPPED", "destroyed", "created"
zpool list -o name,size,alloc,free data
zfs list -t snapshot -o name,creation,used,refer -s creation -r data/clickhouse data/postgres
zfs get -Hp usedbysnapshots data/clickhouse data/postgres
cat /var/lib/node_exporter/textfile_collector/zfs_snapshot.prom
```

- Skip logged + `guard_skipped=1` → capacity problem, not a job problem.
- Timer inactive / unit failed → job problem (see Mitigation).
- No `.prom` file at all → job never ran on this host, or the textfile
  dir moved (check `--collector.textfile.directory` on node_exporter).
- `zfs_snapshot_error.prom` present (`stellarindex_zfs_snapshot_pool_free_unreadable=1`)
  → `zpool list -Hp -o free data` failed or returned a non-number. The
  job is **fail-closed**: it destroyed nothing, snapshotted nothing, and
  the unit exited non-zero. Fix `zpool` (pool imported? `zpool status`),
  then `systemctl start zfs-snapshot.service`; a good run removes the
  error file.

## Mitigation (≤ 15 min)

Pool free low / critical:

- [ ] Find what is pinning space: `zfs list -t snapshot -o name,used -s used`
      — a `manual-*` or hand-named snapshot from a past incident is
      the usual culprit; `zfs destroy data/<ds>@manual-<label>` once
      it is genuinely no longer needed (confirm with the person who
      took it; automation will never do this for you).
- [ ] Otherwise treat as the pool-capacity runbook
      ([zfs-pool-full](zfs-pool-full.md)): archive → S3, galexie trim.
      **Never propose a drive upgrade for r1** — capacity is software-only.
- [ ] If the pool is healthy but the floor is wrong for this host, change
      `zfs_snapshot_min_free_bytes` in the inventory and re-apply
      `--tags zfs-snapshots`; do not hand-edit `/etc/default/zfs-snapshot`.
- [ ] Verification: `stellarindex_zfs_pool_free_bytes` above threshold;
      next run (or `systemctl start zfs-snapshot.service`) logs `created`.

Snapshot stale:

- [ ] `systemctl enable --now zfs-snapshot.timer` if inactive; read the
      journal for the failing `zfs` command otherwise.
- [ ] `systemctl start zfs-snapshot.service` to take today's snapshot
      now (it is a metadata operation — seconds, no service impact).
- [ ] Verification: `stellarindex_zfs_snapshot_latest_unix{dataset}`
      within the last hour; the alert clears within 30 min.

## Taking a snapshot before a destructive change

The ClickHouse destructive-DDL runbook (`docs/operations/clickhouse-destructive-ddl.md`)
requires a **fresh** snapshot before any `DROP` / irreversible `ALTER`;
do the same before a Postgres migration you are nervous about.

```sh
sudo scripts/ops/zfs-snapshot-now.sh data/clickhouse                 # auto-YYYYMMDD-HHMM: rides the 3 d retention
sudo scripts/ops/zfs-snapshot-now.sh data/postgres --keep pre-0123   # manual-pre-0123: never auto-pruned — you delete it
```

The wrapper sources `/etc/default/zfs-snapshot` so it uses the same
dataset list and floor as the timer, and the guard applies: on a pool
below the floor it prunes older auto snapshots first and **refuses
(exit 1)** if that is not enough. Do not work around it; free space is
the fix. Prefer the default (auto) form: a `--keep` snapshot taken
"just in case" and forgotten pins ~250 GB/day of ClickHouse churn.

## Recovery from a snapshot

### What a ZFS snapshot is — honest semantics

A ZFS snapshot is an **atomic, point-in-time image of the dataset**,
taken at a transaction-group boundary. Everything `write(2)`-ed before
the snapshot is in it (ZFS syncs the txg; it does not depend on the
application having called `fsync`). It is therefore **crash-consistent
at the filesystem level** — exactly what the on-disk state would be
after a `kill -9` of the process, *not* a clean shutdown. It is not an
application-level consistent backup:

- **ClickHouse (MergeTree, single node, no replication):** every part
  is written to a `tmp_*` directory and renamed into place atomically,
  and merges produce a new part before the old ones are marked
  inactive, so a snapshot contains only whole parts plus possibly some
  `tmp_*` leftovers, which ClickHouse discards on startup. That makes
  a crash-consistent snapshot **recoverable** the way a crash is. What
  is *lost*: rows still in memory — `async_insert` buffers, Buffer-engine
  tables, in-flight inserts whose part had not been renamed yet — i.e.
  up to the last few seconds of ingest. Nothing here needs to be, or
  is, quiesced: there is **no `SYSTEM SYNC` step** (`SYSTEM SYNC
  REPLICA` is for replicated tables, which this lake does not use) and
  **`ALTER TABLE … FREEZE` is not used** — FREEZE hard-links a
  table's parts into `shadow/` for a per-table consistent copy, which is
  the right tool for a table-level *backup*; a dataset snapshot already
  captures every table at one instant and costs nothing to take. If
  you ever need a *specific* table's parts to be exactly consistent
  with a query you just ran, `SYSTEM STOP MERGES <table>` before
  `zfs-snapshot-now.sh` and `SYSTEM START MERGES` after — but be aware
  this is belt-and-braces, not a requirement.
- **Postgres:** `data/postgres` (`/var/lib/postgresql`) holds the
  cluster data **and** `pg_wal` in the same dataset, so a snapshot is
  exactly the state a crash would leave: Postgres replays WAL from the
  last checkpoint on start and reaches a consistent state. Committed
  transactions are present; in-flight ones roll back. This is the
  same guarantee a `pg_basebackup` without `pg_start_backup` would
  *not* give you — it only works because data and WAL are captured in
  one atomic snapshot. **Never** restore a snapshot of the data
  directory with `pg_wal` from a different point in time.
- **vs pgBackRest PITR (ADR-0043):** pgBackRest restores to *any*
  point in time (WAL archive), lives *off-host* (repo2 on Storage Box)
  and survives losing the pool. A ZFS snapshot restores only to the
  snapshot instant, lives *in the same pool* and dies with it. They are
  complementary: ZFS is the minutes-scale answer to a logical fault;
  pgBackRest is the DR answer. A snapshot is **not** a backup.

### Option A — clone and copy back one ClickHouse table (preferred)

Non-destructive: the live lake keeps running; you attach the old parts
into a fresh table and copy rows across (or `ATTACH PART`).

```sh
# 1. Pick the snapshot (creation time = the point you restore to).
zfs list -t snapshot -o name,creation -s creation data/clickhouse

# 2. Clone it read-write somewhere ClickHouse can read.
zfs clone -o mountpoint=/mnt/ch-restore data/clickhouse@auto-20260829-0145 data/ch-restore

# 3. Locate the table's parts. Atomic databases store data under
#    store/<uuid-prefix>/<uuid>/ — get the path from the LIVE server
#    (the uuid is the same in the snapshot):
clickhouse-client -q "SELECT data_paths FROM system.tables WHERE database='tier1' AND name='trades'"
#    → ['/var/lib/clickhouse/store/3fa/3fa2…/']  ⇒ same relative path under /mnt/ch-restore/

# 4. Create an empty table with the same DDL (name it *_restore), then
#    copy the parts into its detached/ dir and ATTACH them:
clickhouse-client -q "CREATE TABLE tier1.trades_restore AS tier1.trades"
RESTORE_DIR=$(clickhouse-client -q "SELECT data_paths[1] FROM system.tables WHERE database='tier1' AND name='trades_restore'")
cp -a /mnt/ch-restore/store/3fa/3fa2…/*_*_*_* "${RESTORE_DIR}/detached/"     # part dirs only, not tmp_*/format_version.txt
chown -R clickhouse:clickhouse "${RESTORE_DIR}/detached"
clickhouse-client -q "ALTER TABLE tier1.trades_restore ATTACH PART '<part>'"   # per part, or loop over detached/
#    5. Verify counts / ranges against the damaged table, then INSERT … SELECT
#       the missing rows (or swap: EXCHANGE TABLES tier1.trades AND tier1.trades_restore).

# 6. Tear down the clone (required before the origin snapshot can ever be destroyed).
zfs destroy data/ch-restore
```

Follow the ClickHouse destructive-DDL runbook for any `EXCHANGE`/`DROP`
step in (5) — it is itself a destructive change.

### Option B — roll the whole dataset back (last resort)

Destructive: **every change since the snapshot on that dataset is
gone**, and every snapshot newer than the target is destroyed (`-r`).
For `data/clickhouse` that means the lake loses the ledgers ingested
since the snapshot (re-catch-up from Galexie afterwards, see
[backfill-with-live-ingest](../backfill-with-live-ingest.md)); for
`data/postgres` it means every write since — only sane when pgBackRest
PITR is *also* unavailable or slower than the loss is worth.

```sh
systemctl stop stellarindex-indexer stellarindex-aggregator   # writers first
systemctl stop clickhouse-server                              # or postgresql@15-main
zfs rollback -r data/clickhouse@auto-20260829-0145
systemctl start clickhouse-server
# ClickHouse: check system.parts / system.detached_parts for tmp_ leftovers (normal).
# Postgres: expect "database system was not properly shut down; automatic recovery in progress" — normal.
systemctl start stellarindex-indexer stellarindex-aggregator
```

Take a `--keep` snapshot of the *broken* state first if a postmortem
will need it — rollback destroys it.

### Option C — Postgres: clone + throwaway instance

Same shape as the restore-drill (`scripts/ops/restore-drill.sh`): clone
`data/postgres@…` to a mountpoint, start a disposable Postgres on a
non-5432 port against it (`pg_ctl -D /mnt/pg-restore/15/main -o "-p 5499"`),
`pg_dump -t <table>` from there and restore into the live cluster. WAL
replay on first start is expected.

## Root cause analysis

- `journalctl -u zfs-snapshot.service` for the run history (every
  create/destroy/skip is logged, with the reason).
- `zfs list -t snapshot -o name,creation,used,refer -s creation` — what
  is pinning space and since when.
- Grafana: `stellarindex_zfs_pool_free_bytes` vs
  `stellarindex_zfs_snapshot_used_bytes` over a week shows whether
  churn or a forgotten snapshot ate the headroom.

## Known false-positive patterns

- **First 36 h after the role apply on a host that never had the
  job**: the absent branch fires until the first run reports. Start
  the service once (`systemctl start zfs-snapshot.service`) rather
  than waiting.
- **A `now` snapshot in the same minute as the timer's**: logged as
  "already exists (no-op)" — not an error.
- `stellarindex_zfs_snapshot_used_bytes` includes *all* snapshots on
  the dataset (it is `usedbysnapshots`), including manual ones —
  a large value with a small `stellarindex_zfs_snapshot_count` means a
  manual snapshot, not a job problem.

## Related

- Script: `scripts/ops/zfs-snapshot.sh` (installed to
  `/usr/local/bin/`), wrapper `scripts/ops/zfs-snapshot-now.sh`, test
  `scripts/ci/zfs-snapshot-test.sh`.
- Role: `configs/ansible/roles/archival-node/tasks/21-zfs-snapshots.yml`,
  tag `zfs-snapshots`; vars `zfs_snapshot_*` in `defaults/main.yml`.
- Companion runbooks: [zfs-pool-full](zfs-pool-full.md) (pool
  capacity, the percentage-based alert), [backup-failed](backup-failed.md)
  / [restore-drill-stale](restore-drill-stale.md) (pgBackRest, the
  off-host path), [ch-schema-restore](ch-schema-restore.md) (schema,
  not data).
- `docs/operations/clickhouse-destructive-ddl.md` — the DROP/ALTER
  procedure that requires a fresh snapshot (added by
  `fix/clickhouse-drop-size-guard`).
- ADR-0043 backup and restore strategy.

## Changelog

- 2026-08-29 — initial draft with the rolling-snapshot job (decision
  2026-08-29: 3 d clickhouse / 7 d postgres, 2 TiB min-free guard).
