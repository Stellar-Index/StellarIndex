---
title: Runbook — zfs-snapshots
last_verified: 2026-08-29
status: draft
severity: P2
---

# Runbook — `stellarindex_zfs_pool_free_low` / `stellarindex_zfs_pool_free_critical` / `stellarindex_zfs_snapshot_stale` / `stellarindex_zfs_snapshot_pool_free_unreadable`

## At a glance

Rules: `deploy/monitoring/rules/zfs-snapshots.yml` (identical `configs/prometheus/rules.r1/zfs-snapshots.yml`) over textfile gauges from `zfs-snapshot.sh`.

- `stellarindex_zfs_pool_free_low` (P3): `zpool` free under 2.5 TiB for 15 min.
- `stellarindex_zfs_pool_free_critical` (P1, page): free under 1.5 TiB for 5 min; the guard has pruned all it may and is skipping snapshots.
- `stellarindex_zfs_snapshot_stale` (P3): newest auto snapshot of a dataset older than 36 h, or never reported (absent branch, clickhouse only).
- `stellarindex_zfs_snapshot_pool_free_unreadable` (P3): the last run could not read pool free space and refused to act (nothing pruned or snapshotted, unit failed).

While stale, a logical fault (bad `DROP`, bad migration) costs a multi-hour pgBackRest restore or multi-day re-derive. A snapshot is NOT a backup: it lives in the same pool; pgBackRest ([ADR-0043](../../adr/0043-backup-and-restore-strategy.md)) is the off-host DR path.

## What the job does

`zfs-snapshot.timer` -> `zfs-snapshot.service` -> `/usr/local/bin/zfs-snapshot.sh rotate`, daily 01:45 UTC (+5 min jitter), config `/etc/default/zfs-snapshot`:

1. Retention: destroy `auto-YYYYMMDD-HHMM` snapshots older than 3 d (`data/clickhouse`, `data/postgres`).
2. Min-free guard (`zfs_snapshot_min_free_bytes`, default 2 TiB): below it, destroy `auto-*` snapshots oldest-first across both datasets, never a dataset's newest.
3. Snapshot `zfs snapshot -r <dataset>@auto-YYYYMMDD-HHMM` if at/above the floor; else skip and set `stellarindex_zfs_snapshot_guard_skipped{dataset}=1`. Recursive, so `data/postgres/wal` is captured with its parent.
4. Write `/var/lib/node_exporter/textfile_collector/zfs_snapshot.prom`.

Only `auto-*` names are ever destroyed. A `manual-*` or operator-named snapshot is never deleted by automation; a forgotten one pins churn (~250 GB/day on ClickHouse) forever (see `stellarindex_zfs_snapshot_used_bytes`).

## Check

```sh
systemctl status zfs-snapshot.timer zfs-snapshot.service
journalctl -u zfs-snapshot.service -n 50 --no-pager      # "SKIPPED", "destroyed", "created"
zpool list -o name,size,alloc,free data
zfs list -t snapshot -o name,creation,used,refer -s creation -r data/clickhouse data/postgres
zfs get -Hp usedbysnapshots data/clickhouse data/postgres
cat /var/lib/node_exporter/textfile_collector/zfs_snapshot.prom
```

Likely causes:

- Skip logged and `guard_skipped=1`: capacity, not the job.
- Timer inactive or unit failed: the job.
- No `.prom` file: the job never ran here, or the textfile dir moved (`--collector.textfile.directory`).
- `zfs_snapshot_error.prom` present: `zpool list -Hp -o free data` failed. The job is fail-closed; fix `zpool` (`zpool status`), then `systemctl start zfs-snapshot.service`.

## Fix

Pool free low / critical:

1. Find what pins space: `zfs list -t snapshot -o name,used -s used`. A `manual-*` snapshot from a past incident is the usual culprit; `zfs destroy data/<ds>@manual-<label>` once its owner confirms.
2. Otherwise use [zfs-pool-full](infra.md#stellarindex_zfs_pool_low_space): archive to S3, galexie trim. Never propose a drive upgrade for r1.
3. If the floor is wrong for this host, change `zfs_snapshot_min_free_bytes` in the inventory and re-apply `--tags zfs-snapshots`; do not hand-edit `/etc/default/zfs-snapshot`.
4. Verify `stellarindex_zfs_pool_free_bytes` is above threshold and the next run logs `created`.

Snapshot stale:

1. `systemctl enable --now zfs-snapshot.timer` if inactive; otherwise read the journal for the failing `zfs` command.
2. `systemctl start zfs-snapshot.service` (metadata only, seconds).
3. Verify `stellarindex_zfs_snapshot_latest_unix{dataset}` is within the hour; the alert clears in 30 min.

False positives: the absent branch fires up to 36 h after first role apply (start the service once); a large `used_bytes` with a small `stellarindex_zfs_snapshot_count` means a manual snapshot.

Escalate: if the pool stays under 1.5 TiB after removing manual snapshots, page the storage owner.

## Taking a snapshot before a destructive change

`docs/operations/clickhouse-destructive-ddl.md` requires a fresh snapshot before any `DROP` / irreversible `ALTER`; do the same before a risky Postgres migration.

```sh
sudo scripts/ops/zfs-snapshot-now.sh data/clickhouse                 # auto-YYYYMMDD-HHMM: rides the 3 d retention
sudo scripts/ops/zfs-snapshot-now.sh data/postgres --keep pre-0123   # manual-pre-0123: never auto-pruned; you delete it
```

The wrapper uses the same config and guard as the timer and refuses (exit 1) if pruning cannot reach the floor; free space instead of working around it. Prefer the auto form.

## Recovery from a snapshot

- ClickHouse: only whole parts plus discarded `tmp_*` leftovers; recoverable like a crash. Rows still in memory (async-insert buffers, in-flight inserts), up to a few seconds of ingest, are lost. No `SYSTEM SYNC` or `ALTER TABLE ... FREEZE` is needed.
- Postgres: `pg_wal` is a symlink into the child dataset `data/postgres/wal`; the job snapshots with `-r`, so data and WAL come from one instant and Postgres replays WAL on start. NEVER restore the data directory with a `pg_wal` from another point in time. Verify the layout: `readlink -f /var/lib/postgresql/15/main/pg_wal` must be under `/var/lib/postgresql/wal`, and `zfs list -r data/postgres` must show both datasets. Snapshots from before the WAL move (pg_wal on root ext4 `/pgwal`) have no WAL and cannot start without `pg_resetwal`; use pgBackRest for that period.
- Postgres config is not in the dataset (`/etc/postgresql/15/main/`); a clone started with the packaged config points at the LIVE cluster, hence Option C.

### Option A - clone and copy back one ClickHouse table (preferred)

```sh
# 1. Pick the snapshot (creation time = the point you restore to).
zfs list -t snapshot -o name,creation -s creation data/clickhouse

# 2. Clone it read-write somewhere ClickHouse can read.
zfs clone -o mountpoint=/mnt/ch-restore data/clickhouse@auto-20260829-0145 data/ch-restore

# 3. Locate the table's parts (same uuid path in the snapshot):
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

Any `EXCHANGE`/`DROP` in step 5 is itself destructive: follow `docs/operations/clickhouse-destructive-ddl.md`.

### Option B - roll the whole dataset back (last resort)

Destructive: every change since the snapshot is gone and every newer snapshot is destroyed (`-r`). ClickHouse then needs re-catch-up from Galexie ([backfill-procedure](../backfill-procedure.md#running-alongside-live-ingest)). For Postgres use it only when pgBackRest PITR is unavailable or slower than the loss is worth. Take a `--keep` snapshot of the broken state first if a postmortem needs it.

```sh
systemctl stop stellarindex-indexer stellarindex-aggregator   # writers first
systemctl stop clickhouse-server                              # or postgresql@15-main
zfs rollback -r data/clickhouse@auto-20260829-0145
systemctl start clickhouse-server
# ClickHouse: check system.parts / system.detached_parts for tmp_ leftovers (normal).
# Postgres: expect "database system was not properly shut down; automatic recovery in progress" — normal.
systemctl start stellarindex-indexer stellarindex-aggregator
```

#### Option B for Postgres

Roll back BOTH `data/postgres` and `data/postgres/wal` to the SAME snapshot name (`-r` does not descend). Consequences:

1. Every Postgres write after the snapshot is gone; ClickHouse is not rolled back. `ingestion_cursors` and projector cursors rewind, so the lake receives that range again: check for duplicates (`docs/operations/lake-dedup-2026-07.md`) and re-derive served-tier rows written after the snapshot (`projector-replay`).
2. The timeline must move. A cluster that simply restarts re-generates WAL names pgBackRest already archived from the discarded future; `archive-push` refuses them and `pg_wal` fills. Starting once in archive recovery ends on a new timeline.
3. Take a new full backup once archiving is healthy.

```sh
# 0. Stop every Postgres writer, then Postgres.
systemctl stop stellarindex-api stellarindex-aggregator stellarindex-indexer
systemctl list-units 'stellarindex-*' 'cap67-*' --state=active   # stop anything else writing
systemctl stop postgresql@15-main

# 0b. Check the archive's timeline first. If a timeline at or above the one this
#     promotion creates is already archived (a rehearsal or offsite restore-drill),
#     pin an explicit, confirmed-unused recovery_target_timeline in step 2.
sudo -u postgres pgbackrest --stanza=stellarindex info | grep -E 'timeline|wal archive min/max'

# 1. Roll back data AND WAL to the same snapshot.
S=pre-v0921   # the snapshot name, e.g. manual-pre-v0921 or auto-YYYYMMDD-HHMM
zfs rollback -r "data/postgres@$S"
zfs rollback -r "data/postgres/wal@$S"
readlink -f /var/lib/postgresql/15/main/pg_wal     # must be under /var/lib/postgresql/wal

# 2. Recover to the end of the LOCAL WAL and promote onto a new timeline, in a
#    one-off start outside systemd. restore_command must fetch nothing (the
#    archive holds the discarded future). No recovery_target: 'immediate' would
#    drop commits the snapshot holds.
D=/var/lib/postgresql/15/main
rm -f "$D/postmaster.pid"
sudo -u postgres touch "$D/recovery.signal"
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$D" -w -t 7200 start -o "\
  -c config_file=/etc/postgresql/15/main/postgresql.conf \
  -c restore_command=/bin/false"
sudo -u postgres psql -Atc "SELECT pg_is_in_recovery(), pg_walfile_name(pg_current_wal_lsn())"
#    → f, first 8 hex digits (timeline) one higher than before; recovery.signal gone,
#    pg_wal holds a new 0000000N.history.
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$D" -m fast stop
systemctl start postgresql@15-main

# 3. Archiving healthy on the new timeline, then a new full backup.
sudo -u postgres pgbackrest --stanza=stellarindex check
sudo -u postgres psql -Atc "SELECT last_archived_wal, last_failed_wal FROM pg_stat_archiver"
sudo -u postgres pgbackrest --stanza=stellarindex --type=full backup   # hours; run in tmux

# 4. Writers back, then re-derive (item 1 above).
systemctl start stellarindex-indexer stellarindex-aggregator stellarindex-api
```

Rehearse steps 1-2 on a clone (Option C) first: timeline and archive behaviour fail silently.

### Option C - Postgres: clone + throwaway instance

Start a disposable Postgres sharing nothing with the live cluster (config, pid file, log dir, port, WAL archive), then `pg_dump -t <table>` into the live cluster. The snapshot carries the live `postmaster.pid` and a `pg_wal` symlink to the LIVE WAL: repoint and verify the symlink BEFORE removing the pid file.

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
# Connect over the clone's own socket (the live pg_hba.conf may require a password on TCP).
sudo -u postgres psql -h /mnt/pg-restore-log -p 5433 -d stellarindex -Atc "SELECT pg_is_in_recovery(), current_setting('data_directory')"

# 3. Tear down.
sudo -u postgres /usr/lib/postgresql/15/bin/pg_ctl -D "$C" -m fast stop
zfs destroy data/pg-restore && zfs destroy data/pg-restore-wal
```

`archive_mode=off` matters most: a clone that archives pushes WAL into the live pgBackRest stanza.

## Related

- `scripts/ops/zfs-snapshot.sh`, `scripts/ops/zfs-snapshot-now.sh`; role `configs/ansible/roles/archival-node/tasks/21-zfs-snapshots.yml`.
- [pool space](infra.md#stellarindex_zfs_pool_low_space), [backup](infra.md#stellarindex_timescale_backup_none_24h), [restore drill](restore-drill.md#stellarindex_restore_drill_stale), [ch-schema-restore](ch-schema-restore.md).
