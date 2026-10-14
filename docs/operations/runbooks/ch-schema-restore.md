---
title: Runbook — ClickHouse schema + state snapshot (ADR-0043 §2.1)
last_verified: 2026-09-09
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_schema_snapshot_stale` / `_offsite_stale`

## At a glance

P3. Rules: `deploy/monitoring/rules/storage.yml`. Producer: `scripts/ops/ch-schema-snapshot.sh` via `ch-schema-snapshot.timer` (daily 03:40 UTC).

- `stellarindex_ch_schema_snapshot_stale`: older than 36 h, or `last_success_unix` absent for 36 h (never ran / every run failed).
- `stellarindex_ch_schema_snapshot_offsite_stale`: no off-site push in 72 h on a host with a local snapshot, including a host that never had a target. Per host (`instance` label).
- `stellarindex_ch_schema_snapshot_unit_failed`: `ch-schema-snapshot.service` exited non-zero for 5 min; usually fires first and names the host.

No customer impact, but while stale a lake rebuild would use a schema that no longer matches production. [ADR-0043](../../adr/0043-backup-and-restore-strategy.md) §2 rejects a full ClickHouse backup (the lake is derived from the Galexie archive, ADR-0027); what is not recoverable is the lake's own DDL, which `deploy/clickhouse/tier1_schema.sql` (founding DDL only) does not carry.

## What the snapshot contains

Under `/var/lib/stellarindex/ch-schema-snapshot/<YYYY-MM-DD>/` (90 days retained): `schema.sql` (`SHOW CREATE` for every object in `stellar`), `tables.tsv`, `partitions.tsv` (active 1M-ledger partitions), `settings-changed.tsv`, `server.tsv`, `ch-backfill-done-windows.txt` (`ch-full-backfill.sh` resume state), `ingestion-cursors.tsv`, `MANIFEST`, `SHA256SUMS`.

## Restore path: snapshot to CREATE

```sh
SNAP=/var/lib/stellarindex/ch-schema-snapshot/2026-07-25   # or the offsite copy

# 0) Integrity first.
( cd "$SNAP" && sha256sum -c SHA256SUMS )

# 1) Review before replaying; this is DDL.
less "$SNAP/schema.sql"

# 2) Replay onto an empty/new server (no conflicting objects).
clickhouse-client --port 9300 --multiquery < "$SNAP/schema.sql"

# 3) Verify the restored shape matches what was captured.
clickhouse-client --port 9300 --query \
  "SELECT name, engine, sorting_key FROM system.tables
    WHERE database='stellar' ORDER BY name FORMAT TabSeparated" \
  | diff - <(tail -n +2 "$SNAP/tables.tsv" | cut -f1,2,4)

# 4) Re-derive the data. Historic ranges MUST read the archive bucket;
#    ch-backfill's default is the trimmed live bucket.
BUCKET=galexie-archive bash scripts/ops/ch-full-backfill.sh

# 5) For a drill, not an incident (ADR-0043 §2.2; ch-backfill -dry-run, fetch+decode only):
DRILL_CH_WINDOW=100000 bash scripts/ops/restore-drill.sh
```

If a lake data backup chain exists, prefer [ch-lake-backup](ch-lake-backup.md#restore).

## stellarindex_ch_schema_snapshot_stale

Check:

```sh
systemctl status ch-schema-snapshot.service
journalctl -u ch-schema-snapshot.service -n 50 --no-pager
/usr/local/bin/ch-schema-snapshot.sh          # run it by hand; it is seconds
```

The script fails closed: it refuses to snapshot a database with zero tables and does not stamp `last_success_unix` if any `SHOW CREATE` failed. "Never ran" and "every run failed" appear as an absent series; when the `absent_over_time(...[36h])` arm fires, the alert has no `instance` label, so check every host that should run the timer.

| Cause | Fix |
| ----- | --- |
| ClickHouse unreachable on `$CH_HTTP` | Fix ClickHouse first |
| A `SHOW CREATE` errored (broken view, dictionary source down) | The journal names the table; repair or drop it |
| Timer disabled / not installed | `systemctl enable --now ch-schema-snapshot.timer` |
| Disk full under `OUT_DIR` | Free space (ZFS pool alerts own that) |
| Host rebuilt / textfile dir wiped | `systemctl start ch-schema-snapshot.service`; `ls /var/lib/node_exporter/textfile_collector/ch_schema_snapshot.prom` |

## stellarindex_ch_schema_snapshot_offsite_stale

A local snapshot exists but no copy reached off-site storage in 72 h, so the only copy of the DDL sits on the pool it protects. Split the cases with the gauge (emitted every run):

```sh
curl -s http://127.0.0.1:9100/metrics | grep ch_schema_snapshot_offsite
# in Prometheus: stellarindex_ch_schema_snapshot_offsite_configured{instance="<host>:9100"}
```

| `offsite_configured` | Meaning | Fix |
| --- | --- | --- |
| `0` or absent | No off-site target was ever configured. | Set `ch_schema_snapshot_mc_target` (an `mc` alias/prefix) in the host inventory, configure the matching `mc` alias for root, then `ansible-playbook -i inventory/<host>.yml playbooks/archival-node.yml --diff --tags backup`. On r1 this is [v1-launch-plan](../v1-launch-plan.md) row 1.12. |
| `1` | A target exists and the push fails (the stamp goes absent, not old). | `journalctl -u ch-schema-snapshot.service -n 50` shows `OFFSITE PUSH FAILED`; check the `mc` alias and credentials behind `SNAPSHOT_MC_TARGET` (as root: `mc alias list`, `mc ls <alias>/...`; do not paste its output, it prints keys), that `mc` is installed, and remote quota. Run `/usr/local/bin/ch-schema-snapshot.sh` by hand: exit `2` = snapshot written, push failed. |

The alert clears on the first successful push. The archival-node role (`18-pgbackrest-backup.yml`) refuses the backup surface on a pubnet host without a target; `ch_schema_snapshot_offsite_ack` is accepted only on test nets and does not satisfy this alert.

Interim off-box copy while a host has no off-site store (does NOT clear the alert; run after any DDL change and at least monthly):

```sh
# from a checkout, with access to the ClickHouse HTTP port
OUT_DIR=deploy/clickhouse/schema-snapshot TEXTFILE_DIR=/dev/null \
  scripts/ops/ch-schema-snapshot.sh
git add deploy/clickhouse/schema-snapshot && git commit
```

Escalate: if the snapshot cannot be taken because ClickHouse is down, go to [clickhouse.md](clickhouse.md#stellarindex_clickhouse_server_down).

## The drift check

Tier-1 lake DDL is hand-applied (no migration runner), so nothing else notices an `ORDER BY` changed by hand or a declared table never created; a re-derive against a differing sort order silently writes a mis-sorted table. `configs/ansible/roles/archival-node/files/ch-schema-drift.sh` runs daily (`ch-schema-drift.timer`, 04:10 UTC) and compares, per table in `deploy/clickhouse/tier1_schema.sql`: existence, `ENGINE` including arguments (parsed from `SHOW CREATE`), `PARTITION BY`, `ORDER BY`, and column names in order. Column types are `INFO` only (ClickHouse re-renders them). Tables live but absent from the file are counted (`stellarindex_ch_schema_drift_uncodified`) but do not fail.

Alerts (`deploy/monitoring/rules/storage.yml` and the `configs/prometheus/rules.r1/` mirror): `stellarindex_ch_schema_drift_detected` (`_divergent > 0`), `stellarindex_ch_schema_drift_not_converged` (`_intent_converged == 0`), `stellarindex_ch_schema_drift_stale` (timer stopped; node_exporter would re-serve a frozen verdict).

Modes (every message names the file it read):

| Mode | Selected by | Reads |
| --- | --- | --- |
| live (default) | nothing, or `LIVE=1` | a fresh `SHOW CREATE` sweep via `$CH_HTTP` |
| capture | `LIVE_SCHEMA=<file>` | the file named |
| snapshot | `LIVE=0` | newest `schema.sql` under `$SNAPSHOT_DIR`; refuses (exit `2`) if the intent is newer than the capture |

```sh
ch-schema-drift.sh                      # against live; also checks its intent against the deployed-versions sidecars
LIVE=0 ch-schema-drift.sh               # newest retained capture

# from a checkout, against a snapshot you copied down
INTENT=deploy/clickhouse/tier1_schema.sql \
  LIVE_SCHEMA=/path/to/schema.sql TEXTFILE_DIR=/dev/null \
  configs/ansible/roles/archival-node/files/ch-schema-drift.sh
```

Exit codes: `0` clean, `1` drift, `2` could-not-compare (ClickHouse unreachable, or the intent is not current). `ch-schema-drift.service` is not excluded from `stellarindex_systemd_unit_failed`, so a refusal also tickets.

### NOT CONVERGED

The intent is a file the archival-node role ships, stamped `-- Intent-Version:` and `-- Intent-Schema-Commit:`. The check compares `Intent-Version` with the lowest release tag among the host's release-managed binaries (`deployed-versions` sidecars, `stellarindex-migrate` excluded, as `deploy.yml` computes it; see [deployed-versions](../deployed-versions.md)). An older intent is `NOT CONVERGED`: exit `2`, nothing compared, no `_divergent` gauge. It is a provisioning gap, not schema drift. An unstamped shipped copy refuses the same way; an unstamped intent the operator named compares and is marked `UNVOUCHED`. A release that changes no config surface can read NOT CONVERGED with current DDL; the cost is one idempotent role run. Clear it:

```sh
cd configs/ansible
ansible-playbook -i inventory/<host>.yml playbooks/archival-node.yml \
  --tags ch-schema-drift
```

Then re-run the check; any `DRIFT` now is real.

### Drift

Decide which side is wrong before touching anything: a hand change on the host never codified (update `tier1_schema.sql` in the same change), or repo DDL never applied (apply it). Editing the repo to match live without knowing which codifies the incident.

Metrics: `stellarindex_ch_schema_drift_divergent`, `_tables`, `_uncodified`, `_last_run_unix`, `_live` (`1` = fresh sweep), `_intent_verified` (`0` = the drift verdict is unvouched), `_intent_converged` (absent when unverified), `_intent_info{intent_version,host_version}`.

```promql
# converged and clean
stellarindex_ch_schema_drift_intent_converged == 1
  and stellarindex_ch_schema_drift_divergent == 0

# converged and drifted: decide which side is wrong
stellarindex_ch_schema_drift_intent_converged == 1
  and stellarindex_ch_schema_drift_divergent > 0

# NOT CONVERGED: re-apply the role, then re-read
stellarindex_ch_schema_drift_intent_converged == 0
```

A NOT CONVERGED run rewrites the textfile with only `_last_run_unix` and the convergence series. Self-test: `configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh`.

## Related

- ADR-0043 §2; ADR-0027 (cold tier); `docs/operations/drills/restore-drills.md`.
- `infra.md#stellarindex_timescale_backup_none_24h`: the Postgres half.
