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
parts pinned per retained day) and `data/postgres` (3 d, small churn; pgBackRest PITR covers older faults)
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
   dataset's window (`data/clickhouse` 3 d, `data/postgres` 3 d).
2. **Min-free guard** — if `zpool list -o free data` < floor, destroy
   `auto-*` snapshots oldest-first across both datasets (by creation
   time, not size — predictable, and postgres snapshots are tiny), one
   at a time re-reading free space, stopping at each dataset's newest.
3. **Snapshot** — if at/above the floor, `zfs snapshot -r
   <dataset>@auto-YYYYMMDD-HHMM`; else skip + metric. Snapshot and
   destroy are recursive: a child dataset (`data/postgres/wal`) is
   captured in the same instant as its parent and pruned with it.
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
  cluster data; `pg_wal` is a symlink into the child dataset
  `data/postgres/wal` (`/var/lib/postgresql/wal/15-main/pg_wal`). The job
  snapshots with `-r`, so data and WAL are captured in one atomic
  instant, which is exactly the state a crash would leave: Postgres
  replays WAL from the last checkpoint on start and reaches a consistent
  state. Committed transactions are present; in-flight ones roll back.
  It only works because data and WAL come from the same instant.
  **Never** restore a snapshot of the data directory with `pg_wal` from
  a different point in time. Check the layout before trusting a
  snapshot: `readlink -f /var/lib/postgresql/15/main/pg_wal` must be
  under `/var/lib/postgresql/wal`, and `zfs list -r data/postgres` must
  show both datasets.
  - **From 2026-05-17 until the v0.92.1 WAL move, pg_wal was on the root
    ext4 filesystem (`/pgwal`)**, outside every snapshot. A
    `data/postgres@…` snapshot from that period has no WAL for its
    instant: it cannot be started without `pg_resetwal`, which gives no
    consistency guarantee. Use pgBackRest for any point in that period.
  - The cluster's configuration is **not** in the dataset: it lives in
    `/etc/postgresql/15/main/` (`postgresql.conf` sets `data_directory`,
    `external_pid_file`, `hba_file`, `log_directory`). Starting a clone
    with the packaged config points it at the LIVE cluster; see Option C.
  - Snapshots pin only the WAL present at each snapshot instant (about
    `max_wal_size` plus any unarchived backlog), not the WAL written
    between snapshots.
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
clickhouse-client --port 9300 -q "SELECT data_paths FROM system.tables WHERE database='tier1' AND name='trades'"
#    → ['/var/lib/clickhouse/store/3fa/3fa2…/']  ⇒ same relative path under /mnt/ch-restore/

# 4. Create an empty table with the same DDL (name it *_restore), then
#    copy the parts into its detached/ dir and ATTACH them:
clickhouse-client --port 9300 -q "CREATE TABLE tier1.trades_restore AS tier1.trades"
RESTORE_DIR=$(clickhouse-client --port 9300 -q "SELECT data_paths[1] FROM system.tables WHERE database='tier1' AND name='trades_restore'")
cp -a /mnt/ch-restore/store/3fa/3fa2…/*_*_*_* "${RESTORE_DIR}/detached/"     # part dirs only, not tmp_*/format_version.txt
chown -R clickhouse:clickhouse "${RESTORE_DIR}/detached"
clickhouse-client --port 9300 -q "ALTER TABLE tier1.trades_restore ATTACH PART '<part>'"   # per part, or loop over detached/
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

#### Option B for Postgres

`zfs rollback` is per dataset (its `-r` destroys newer snapshots, it
does not descend), so roll back **both** `data/postgres` and
`data/postgres/wal` to the **same** snapshot name. Three consequences
that make this different from the ClickHouse case:

1. **Every Postgres write after the snapshot is gone**, and ClickHouse is
   not rolled back with it. `ingestion_cursors` and the projector cursors
   rewind to the snapshot, so the indexer and projector re-process the
   ledgers since then; the lake receives that range again. Check it for
   duplicates afterwards (`docs/operations/lake-dedup-2026-07.md`), and
   treat any served-tier row written after the snapshot as needing
   re-derivation (`projector-replay`).
2. **The timeline must move.** A rolled-back cluster that simply starts
   continues on the same timeline and re-generates WAL segment names
   pgBackRest already archived from the discarded future; `archive-push`
   then refuses them ("already exists … with a different checksum"),
   archiving stalls and `pg_wal` grows until it fills. Starting once in
   archive recovery ends on a new timeline instead.
3. **Take a new full backup** once archiving is healthy on the new
   timeline; until then PITR into the post-rollback period has no base.

```sh
# 0. Stop every Postgres writer, then Postgres.
systemctl stop stellarindex-api stellarindex-aggregator stellarindex-indexer
systemctl list-units 'stellarindex-*' 'cap67-*' --state=active   # stop anything else writing
systemctl stop postgresql@15-main

# 0b. Check the archive's timeline before rolling back anything. A prior
#     rollback rehearsal or the offsite restore-drill can already have
#     pushed a later timeline into this same repo; step 2 below promotes
#     onto "current + 1", and restore_command=/bin/false there means
#     Postgres cannot see the archive's .history files to detect a
#     collision itself — it would only surface once archive-push runs.
sudo -u postgres pgbackrest --stanza=stellarindex info | grep -E 'timeline|wal archive min/max'
#     If a timeline at or above the one this promotion is about to create
#     is already archived, pin an explicit, confirmed-unused
#     recovery_target_timeline in step 2 instead of letting Postgres take
#     the implicit next number.

# 1. Roll back data AND WAL to the same snapshot.
S=pre-v0921   # the snapshot name, e.g. manual-pre-v0921 or auto-YYYYMMDD-HHMM
zfs rollback -r "data/postgres@$S"
zfs rollback -r "data/postgres/wal@$S"
readlink -f /var/lib/postgresql/15/main/pg_wal     # must be under /var/lib/postgresql/wal

# 2. Recover to the end of the LOCAL WAL and promote onto a new timeline,
#    in a one-off start outside systemd. restore_command must fetch
#    nothing: pgBackRest's archive holds the discarded future, and
#    replaying it would re-apply what you rolled back. No recovery_target:
#    'immediate' stops at the first consistent point, which on a crash
#    image is the checkpoint redo point and would drop commits the
#    snapshot holds (and leave pages newer than the WAL replayed).
D=/var/lib/postgresql/15/main
rm -f "$D/postmaster.pid"
sudo -u postgres touch "$D/recovery.signal"
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$D" -w -t 7200 start -o "\
  -c config_file=/etc/postgresql/15/main/postgresql.conf \
  -c restore_command=/bin/false"
sudo -u postgres psql -Atc "SELECT pg_is_in_recovery(), pg_walfile_name(pg_current_wal_lsn())"
#    → f, and the file name's first 8 hex digits (the timeline) one higher
#    than the newest segment in pg_wal before the start; recovery.signal is
#    gone and pg_wal holds a new 0000000N.history.
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$D" -m fast stop
systemctl start postgresql@15-main

# 3. Archiving healthy on the new timeline, then a new full backup.
sudo -u postgres pgbackrest --stanza=stellarindex check
sudo -u postgres psql -Atc "SELECT last_archived_wal, last_failed_wal FROM pg_stat_archiver"
sudo -u postgres pgbackrest --stanza=stellarindex --type=full backup   # hours; run in tmux

# 4. Writers back, then re-derive (item 1 above).
systemctl start stellarindex-indexer stellarindex-aggregator stellarindex-api
```

Rehearse steps 1–2 on a clone (Option C) before relying on them: the
timeline and the archive behaviour are the parts that fail silently.

### Option C — Postgres: clone + throwaway instance

Clone both datasets from one recursive snapshot, repoint the clone's
`pg_wal`, and start a disposable Postgres that shares **nothing** with
the live cluster: not its config, pid file, log directory, port or WAL
archive. `pg_dump -t <table>` from it and restore into the live cluster.
WAL replay on first start is expected.

The order matters. The snapshot carries the live cluster's
`postmaster.pid` and a `pg_wal` symlink that points at the LIVE WAL
directory: repoint and verify the symlink **before** removing the pid
file, so nothing can ever start against the live WAL.

```sh
S=auto-20260829-0145                   # any recursive snapshot of data/postgres
zfs list -t snapshot "data/postgres@$S" "data/postgres/wal@$S"   # both must exist
# data has mountpoint=none, so the clone needs an explicit one.
zfs clone -o mountpoint=/mnt/pg-restore     "data/postgres@$S"     data/pg-restore
zfs clone -o mountpoint=/mnt/pg-restore-wal "data/postgres/wal@$S" data/pg-restore-wal
C=/mnt/pg-restore/15/main

# 1. Repoint pg_wal at the WAL clone and prove it, BEFORE touching the pid file.
ln -sfn /mnt/pg-restore-wal/15-main/pg_wal "$C/pg_wal"
readlink -f "$C/pg_wal"                # must be /mnt/pg-restore-wal/15-main/pg_wal
test "$(readlink -f "$C/pg_wal")" = /mnt/pg-restore-wal/15-main/pg_wal || exit 1
rm -f "$C/postmaster.pid"
install -d -o postgres -g postgres /mnt/pg-restore-log

# 2. Start with every path and side effect overridden on the command line.
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$C" -l /mnt/pg-restore-log/startup.log -w -t 3600 start -o "\
  -c config_file=/etc/postgresql/15/main/postgresql.conf \
  -c data_directory=$C \
  -c hba_file=/etc/postgresql/15/main/pg_hba.conf \
  -c ident_file=/etc/postgresql/15/main/pg_ident.conf \
  -c external_pid_file= \
  -c port=5433 -c listen_addresses=127.0.0.1 \
  -c unix_socket_directories=/mnt/pg-restore-log \
  -c log_directory=/mnt/pg-restore-log \
  -c archive_mode=off -c archive_command=/bin/true \
  -c timescaledb.max_background_workers=0 \
  -c timescaledb.telemetry_level=off \
  -c shared_buffers=8GB"
# Connect over the clone's own socket: the live pg_hba.conf may require a
# password on TCP.
sudo -u postgres psql -h /mnt/pg-restore-log -p 5433 -d stellarindex -Atc "SELECT pg_is_in_recovery(), current_setting('data_directory')"

# 3. Tear down.
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$C" -m fast stop
zfs destroy data/pg-restore && zfs destroy data/pg-restore-wal
```

`archive_mode=off` is the one that matters most: a clone that archives
pushes its WAL into the live pgBackRest stanza.

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
- v0.92.1 — snapshots are recursive; pg_wal moves to `data/postgres/wal`;
  the Postgres semantics were wrong from 2026-05-17 (pg_wal on root) and
  are corrected; Postgres rollback and clone procedures added.
