---
title: ClickHouse destructive DDL — the size guard and the planned-drop procedure
last_verified: 2026-08-29
status: living procedure
---

# ClickHouse destructive DDL — the size guard and the planned-drop procedure

`DROP TABLE`, `TRUNCATE`, `DROP PARTITION` and `REPLACE PARTITION` on the
lake are irreversible: ClickHouse has no undo, and the lake tables are the
certified source the projections are re-derived from
([ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)). The only things
between a mistyped statement and losing `account_movements` (582 GiB) are
ClickHouse's own size guard and a ZFS snapshot. This page is the rule for
both.

## The guard

ClickHouse refuses any of those statements on a table / partition bigger
than `max_table_size_to_drop` / `max_partition_size_to_drop` unless the
file `/var/lib/clickhouse/flags/force_drop_table` exists. The error is
explicit (`Table or Partition in stellar.X was not dropped. Reason: 1. Size
(...) is greater than max_[table/partition]_size_to_drop (...)`) and tells
you the flag path.

Both limits are pinned to ClickHouse's default, **50 GB (53687091200
bytes)**, by ansible on every host:
`configs/ansible/roles/archival-node/tasks/21-clickhouse-drop-guard.yml`
writes `/etc/clickhouse-server/config.d/si-drop-guard.xml` from
`clickhouse_max_table_size_to_drop` / `clickhouse_max_partition_size_to_drop`
(`defaults/main.yml`) and then asserts the LIVE value from
`system.server_settings`. The settings hot-reload — applying the tag does
not restart `clickhouse-server`.

Why this page exists (2026-08-29 audit): r1 measured **1 TiB for both**.
They had been raised by hand for D2's `REPLACE PARTITION`
([d2-ordinal-reproject.md](d2-ordinal-reproject.md)) in 2026-07 and never
lowered or codified — so for a month every lake table was droppable in one
statement with no second step. A raised limit is the wrong tool for a
planned drop; the flag is.

Check the effective value at any time:

```sh
clickhouse-client --port 9300 -q "SELECT name, value FROM system.server_settings
  WHERE name IN ('max_table_size_to_drop', 'max_partition_size_to_drop')"
```

Do not set either to `0` (unlimited) — the ansible task refuses it — and
do not raise them in an inventory to get a job through. Raise-and-forget is
exactly how r1 ended up at 1 TiB.

## The rule for a planned destructive statement on > 50 GB

1. **Fresh ZFS snapshot of `data/clickhouse` first.** No exceptions — the
   snapshot is the only undo. Snapshot tooling is its own PR; until it
   lands, take it by hand and record the snapshot name in the job's log
   or the incident/plan doc:
   ```sh
   sudo zfs snapshot data/clickhouse@pre-drop-$(date -u +%Y%m%dT%H%M%SZ)
   zfs list -t snapshot -r data/clickhouse | tail -3
   ```
   Confirm the pool has headroom for the snapshot to hold the freed space
   (a dropped 582 GiB table frees nothing while its snapshot exists).
2. **Arm the flag for exactly one statement, then disarm it.**
   ```sh
   sudo touch /var/lib/clickhouse/flags/force_drop_table
   clickhouse-client --port 9300 -q "DROP TABLE stellar.some_table SYNC"
   sudo rm -f /var/lib/clickhouse/flags/force_drop_table
   ```
   The server deletes the flag itself when it is consumed by an OVERSIZE
   drop, but leaves it in place if the statement turned out to be under
   the limit — an unconsumed flag silently permits the next big drop by
   anyone. Always `rm -f` after, and confirm it is gone.
3. **Never leave the flag armed across a multi-statement job.** Scripts
   that do guarded DDL (`scripts/ops/d2-ordinal-reproject.sh`,
   `scripts/ops/d3-lecur-v2-rebuild.sh`) wrap each guarded statement in
   `guarded_ddl` — touch, run, remove — and refuse to start without an
   explicit acknowledgement (`D2_FORCE_DROP=yes`, `D3_FORCE_DROP_OLD=yes`,
   `D3_FORCE_DROP_V2=yes`). New scripts that drop or replace lake data
   follow the same shape: an explicit env/flag acknowledgement, the flag
   armed per statement, never a raised limit.

`REPLACE PARTITION` counts: it drops the target partition, and the guard
applies to the partition size. `DROP TABLE IF EXISTS` on a table that
does not exist is not guarded (nothing to size).

## Restore path

Roll the dataset back to the snapshot from step 1 (`zfs rollback` after
stopping `clickhouse-server`), or clone it and `ATTACH` the table's parts
from the clone for a single-table restore. The schema half is covered by
[runbooks/ch-schema-restore.md](runbooks/ch-schema-restore.md).

## Related

- [clickhouse-ops-batch-profile.md](clickhouse-ops-batch-profile.md) —
  the identity heavy ops jobs run as.
- [deploy-config-apply.md](deploy-config-apply.md) — how config.d changes
  land on a host (this one via `--tags clickhouse-drop-guard`).

## Before the first apply on a host: find the hand-written override

A later-sorting `config.d` file (or an element in `config.xml`) that still
carries a larger value silently wins over `si-drop-guard.xml`; the role's live
verify task then fails loudly. Locate and remove it first:

```sh
clickhouse-client --port 9300 -q "SELECT name,value FROM system.server_settings WHERE name LIKE 'max_%size_to_drop'"
sudo grep -rn 'size_to_drop' /etc/clickhouse-server/config.xml /etc/clickhouse-server/config.d/
ls -la /var/lib/clickhouse/flags/   # no stale force_drop_table
```
