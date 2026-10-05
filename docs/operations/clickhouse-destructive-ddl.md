---
title: ClickHouse destructive DDL — the size guard and the planned-drop procedure
last_verified: 2026-08-29
status: living procedure
---

# ClickHouse destructive DDL — the size guard and the planned-drop procedure

`DROP TABLE`, `TRUNCATE`, `DROP PARTITION` and `REPLACE PARTITION` on the
lake are irreversible, and the lake is the certified source projections are
re-derived from ([ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)).
Only ClickHouse's size guard and a ZFS snapshot stand between a mistyped
statement and losing `account_movements` (582 GiB).

## The guard

ClickHouse refuses those statements on a table / partition bigger than
`max_table_size_to_drop` / `max_partition_size_to_drop` unless
`/var/lib/clickhouse/flags/force_drop_table` exists. The error names the flag
path (`Table or Partition in stellar.X was not dropped. Reason: 1. Size
(...) is greater than max_[table/partition]_size_to_drop (...)`).

Both limits are pinned to **50 GB (53687091200 bytes)** by ansible on every
host: `configs/ansible/roles/archival-node/tasks/21-clickhouse-drop-guard.yml`
writes `/etc/clickhouse-server/config.d/si-drop-guard.xml` from
`clickhouse_max_table_size_to_drop` / `clickhouse_max_partition_size_to_drop`
(`defaults/main.yml`), then asserts the LIVE value from
`system.server_settings`. The settings hot-reload; applying the tag does not
restart `clickhouse-server`. Between applies, the hourly
`config-assertions.sh` check `ch_drop_guard_live` fails when either is `0` or
above 50 GB (`stellarindex_config_assertion_failed`,
[runbook](runbooks/config-assertion-failed.md)).

A raised limit is the wrong tool for a planned drop; the flag is. r1 once ran
at 1 TiB for both after a hand raise for D2's `REPLACE PARTITION`
([d2-ordinal-reproject.md](d2-ordinal-reproject.md)), leaving every lake table
droppable in one statement.

Check the effective value:

```sh
clickhouse-client --port 9300 -q "SELECT name, value FROM system.server_settings
  WHERE name IN ('max_table_size_to_drop', 'max_partition_size_to_drop')"
```

Never set either to `0` (unlimited; the ansible task refuses it) and never
raise them in an inventory to get a job through.

## The rule for a planned destructive statement on > 50 GB

1. **Fresh ZFS snapshot of `data/clickhouse` first.** No exceptions: the
   snapshot is the only undo. Take it by hand and record the name in the
   job's log or the incident/plan doc (or use `scripts/ops/zfs-snapshot-now.sh`,
   [runbooks/zfs-snapshots.md](runbooks/zfs-snapshots.md)):
   ```sh
   sudo zfs snapshot data/clickhouse@pre-drop-$(date -u +%Y%m%dT%H%M%SZ)
   zfs list -t snapshot -r data/clickhouse | tail -3
   ```
   Confirm pool headroom: a dropped 582 GiB table frees nothing while its
   snapshot exists.
2. **Arm the flag for exactly one statement, then disarm it.**
   ```sh
   sudo touch /var/lib/clickhouse/flags/force_drop_table
   clickhouse-client --port 9300 -q "DROP TABLE stellar.some_table SYNC"
   sudo rm -f /var/lib/clickhouse/flags/force_drop_table
   ```
   The server deletes the flag when an OVERSIZE drop consumes it, but leaves
   it if the statement was under the limit, and an unconsumed flag silently
   permits the next big drop by anyone. Always `rm -f` after and confirm it
   is gone.
3. **Never leave the flag armed across a multi-statement job.**
   `scripts/ops/d3-lecur-v2-rebuild.sh` wraps each guarded statement in
   `guarded_ddl` (touch, run, remove) and refuses to start without an explicit
   acknowledgement (`D3_FORCE_DROP_OLD=yes`, `D3_FORCE_DROP_V2=yes`); the
   retired `scripts/ops/d2-ordinal-reproject.sh` required `D2_FORCE_DROP=yes`
   and now refuses to run at all. New scripts that drop or
   replace lake data follow the same shape: explicit env/flag acknowledgement,
   flag armed per statement, never a raised limit.

`REPLACE PARTITION` counts: it drops the target partition, and the guard
applies to the partition size. `DROP TABLE IF EXISTS` on a missing table is
not guarded.

## Restore path

Roll the dataset back to the step-1 snapshot (`zfs rollback` after stopping
`clickhouse-server`), or clone it and `ATTACH` the table's parts from the
clone for a single-table restore. Schema:
[runbooks/ch-schema-restore.md](runbooks/ch-schema-restore.md).

A single-table restore can bring `stellar.ledgers` back before
`stellar.contract_events`. `compute-completeness -ch` compares each
Soroban-era `contract_events` partition's row count with the
`soroban_event_count` its `ledgers` rows record; a short partition fails
`substrate_ok` and `recognition_ok` for every event-reading source whose range
it touches. It reads active-part row counts, so it catches a missing or short
partition, but not a partial loss masked by as many unmerged duplicates.

## Related

- [clickhouse-ops-batch-profile.md](clickhouse-ops-batch-profile.md): the
  identity heavy ops jobs run as.
- [deploy-config-apply.md](deploy-config-apply.md): how config.d changes land
  on a host (this one via `--tags clickhouse-drop-guard`).

## Before the first apply on a host: find the hand-written override

The role removes r1's known carrier,
`/etc/clickhouse-server/config.d/zz-partition-drop-limit.xml`. Any other
later-sorting `config.d` file (or a `config.xml` element) carrying a larger
value silently wins over `si-drop-guard.xml`, and the role's live verify task
then fails loudly. Locate and remove it first:

```sh
clickhouse-client --port 9300 -q "SELECT name,value FROM system.server_settings WHERE name LIKE 'max_%size_to_drop'"
sudo grep -rn 'size_to_drop' /etc/clickhouse-server/config.xml /etc/clickhouse-server/config.d/
ls -la /var/lib/clickhouse/flags/   # no stale force_drop_table
```
