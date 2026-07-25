---
title: Runbook — ClickHouse schema + state snapshot (ADR-0043 §2.1)
last_verified: 2026-07-25
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_schema_snapshot_stale` / `_offsite_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_ch_schema_snapshot_stale` (>36 h), `stellarindex_ch_schema_snapshot_offsite_stale` (>72 h) |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` |
| Producer | `scripts/ops/ch-schema-snapshot.sh` via `ch-schema-snapshot.timer` (daily, 03:40 UTC) |
| Typical MTTR | 15 min |
| Impact | No immediate customer impact. While stale, ClickHouse DDL changes are unprotected: a lake rebuild would use a schema that no longer matches production. |

## Why this backup is the whole backup

ADR-0043 §2 **rejects** a full ClickHouse backup as the primary
strategy. The lake is a structural decode of the Galexie ledger
archive, and that ground truth exists in two independent places (the
local MinIO `galexie-archive` bucket and the public
`aws-public-blockchain` cold tier, ADR-0027) — so paying object storage
for a multi-TiB third copy of *derived* data is poor spend. The
question the ADR asks is not "can we recover" but "how long".

What is **not** recoverable from either archive is the lake's own
definition. §2.1:

> Schema + state backup (tiny, daily): `SHOW CREATE` DDL for every
> table + the ch-live-catchup/backfill cursor state, pushed to repo2
> alongside pgBackRest. Losing DDL/config is what turns a re-derive
> from "run the script" into archaeology.

`deploy/clickhouse/tier1_schema.sql` is the **founding** DDL, not the
live one — indexes, materialized views, compression policies and
serving-profile changes have landed on top of it since. Rebuilding a
60M-ledger lake from a stale hand-written schema silently changes sort
orders and codecs. That is what this snapshot prevents.

## What the snapshot contains

Under `/var/lib/stellarindex/ch-schema-snapshot/<YYYY-MM-DD>/`:

| File | Contents |
| ---- | -------- |
| `schema.sql` | `CREATE DATABASE` + `SHOW CREATE` for every table, view and dictionary in `stellar`, ready to replay |
| `tables.tsv` | engine / partition key / sorting key / row + byte counts per table |
| `partitions.tsv` | which 1M-ledger partitions were active and how big — the difference between "rebuild everything" and "rebuild these twelve" |
| `settings-changed.tsv` | non-default server settings |
| `server.tsv` | ClickHouse version + uptime |
| `ch-backfill-done-windows.txt` | `ch-full-backfill.sh`'s resume state — the record of which windows have been backfilled |
| `ingestion-cursors.tsv` | Postgres `ingestion_cursors` (also inside the pgBackRest stanza; copied here so a CH rebuild does not need a full Postgres restore to learn where live ingest was) |
| `MANIFEST`, `SHA256SUMS` | provenance + integrity |

Retention 90 days, because part of the value is the **diff history**
(`diff` two days to see exactly when an `ORDER BY` changed).

## Restore path: snapshot → CREATE

This is the procedure the ADR's re-derive assumes exists.

```sh
SNAP=/var/lib/stellarindex/ch-schema-snapshot/2026-07-25   # or the offsite copy

# 0) Integrity first — a corrupted schema is worse than a missing one.
( cd "$SNAP" && sha256sum -c SHA256SUMS )

# 1) Review before replaying. This is DDL against a database that may
#    still hold data; read it, do not paste it blind.
less "$SNAP/schema.sql"

# 2) Replay onto an empty/new server. Every statement is
#    CREATE ... (SHOW CREATE output), so run it against a database that
#    does NOT already have conflicting objects.
clickhouse-client --port 9300 --multiquery < "$SNAP/schema.sql"

# 3) Verify the restored shape matches what was captured.
clickhouse-client --port 9300 --query \
  "SELECT name, engine, sorting_key FROM system.tables
    WHERE database='stellar' ORDER BY name FORMAT TabSeparated" \
  | diff - <(tail -n +2 "$SNAP/tables.tsv" | cut -f1,2,4)

# 4) Re-derive the data. partitions.tsv tells you which 1M-ledger
#    partitions existed; ch-backfill-done-windows.txt tells you what the
#    previous backfill had already banked.
#    Historic ranges MUST read the archive bucket — ch-backfill's
#    no-seam default is the trimmed live bucket, which cannot hold them.
BUCKET=galexie-archive bash scripts/ops/ch-full-backfill.sh

# 5) Prove the re-derive path first if this is a drill, not an incident:
DRILL_CH_WINDOW=100000 bash scripts/ops/restore-drill.sh
```

Step 5 is ADR-0043 §2.2. Note its honest limit: the drill runs
`ch-backfill -dry-run`, which fetches and decodes every ledger but
writes nothing — there is no scratch-database mode (`clickhouse.Open`
pins the `stellar` database), and re-deriving into the live lake on a
capacity-constrained host would pay for its own evidence in the
scarcest resource on the box. So the measured figure is fetch+decode
throughput, which is the multi-week part; the ClickHouse INSERT path is
exercised continuously by live ingest.

## Mitigation — `stellarindex_ch_schema_snapshot_stale`

```sh
systemctl status ch-schema-snapshot.service
journalctl -u ch-schema-snapshot.service -n 50 --no-pager
/usr/local/bin/ch-schema-snapshot.sh          # run it by hand; it is seconds
```

The script **fails closed**: it refuses to write a snapshot when the
database reports zero tables, and it does not stamp
`last_success_unix` when any `SHOW CREATE` failed. So a stale metric
means one of:

| Cause | Fix |
| ----- | --- |
| ClickHouse unreachable on `$CH_HTTP` | Fix ClickHouse first; this alert is downstream |
| A `SHOW CREATE` errored (broken view, dictionary source down) | The journal names the table; repair or drop it |
| Timer disabled / not installed | `systemctl enable --now ch-schema-snapshot.timer` |
| Disk full under `OUT_DIR` | Free space; the ZFS pool alerts own that |

## Mitigation — `stellarindex_ch_schema_snapshot_offsite_stale`

The snapshot is being written locally but not reaching offsite storage,
so the only copy of the lake's DDL sits on the pool it protects.
Check the `mc` alias and credentials behind
`ch_schema_snapshot_mc_target`, and the remote's quota. This alert
cannot fire on a host that has no offsite target configured — that
state is declared instead, via `ch_schema_snapshot_offsite_ack` in the
inventory (18-pgbackrest-backup.yml refuses to proceed without one or
the other).

### Interim off-box copy while r1 has no offsite store

r1 has no offsite object storage provisioned yet (same gap as
`pgbackrest_offsite_ack`). Until it does, the repo itself is the
off-box copy — public, versioned, and not on this host:

```sh
# from a checkout, with access to the ClickHouse HTTP port
OUT_DIR=deploy/clickhouse/schema-snapshot TEXTFILE_DIR=/dev/null \
  scripts/ops/ch-schema-snapshot.sh
git add deploy/clickhouse/schema-snapshot && git commit
```

Do this after any DDL change, and at least monthly. It is an operator
action, tracked in the operator register — the timer cannot do it
(no repo credentials on r1, and committing from production is its own
bad idea).

## Related

- ADR-0043 §2 — why a full CH backup is rejected and what replaces it.
- ADR-0027 — the `aws-public-blockchain` cold tier that makes the raw
  ledgers independently recoverable.
- `docs/operations/drills/restore-drills.md` — the drill evidence log.
- `backup-failed.md` — the Postgres half of the same ADR.

## Changelog

- 2026-07-25 — initial version, shipped with the §2.1 implementation
  (script + daily timer + staleness alerts). Before this the ClickHouse
  lake had no backup of any kind and no alert that said so.
