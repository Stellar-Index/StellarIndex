---
title: Runbook — ClickHouse schema + state snapshot (ADR-0043 §2.1)
last_verified: 2026-09-04
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_schema_snapshot_stale` / `_offsite_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_ch_schema_snapshot_stale` (>36 h, or `last_success_unix` absent for 36 h — never / every-run-failed), `stellarindex_ch_schema_snapshot_offsite_stale` (>72 h since the last push, or no push inside 72 h on a host that has a local snapshot — including a host that has never had a target; per host, carries `instance`), `stellarindex_ch_schema_snapshot_unit_failed` (`ch-schema-snapshot.service` exited non-zero, 5 min — the causal signal) |
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
`last_success_unix` when any `SHOW CREATE` failed. Because it rewrites
the textfile on every run (and exits before writing it on a first-run
failure), "every run failed" and "never ran" show up as an **absent**
series rather than an old one — the alert's `absent_over_time(...[36h])`
branch covers that shape (audit 2026-08-28, backup-restore-2; before it,
exactly those cases could never fire). When the absent branch is the one
firing, the alert carries no `instance` label: check every host that
should run the timer. `stellarindex_ch_schema_snapshot_unit_failed`
usually fires first and names the host. So a stale metric means one of:

| Cause | Fix |
| ----- | --- |
| ClickHouse unreachable on `$CH_HTTP` | Fix ClickHouse first; this alert is downstream |
| A `SHOW CREATE` errored (broken view, dictionary source down) | The journal names the table; repair or drop it |
| Timer disabled / not installed | `systemctl enable --now ch-schema-snapshot.timer` |
| Disk full under `OUT_DIR` | Free space; the ZFS pool alerts own that |
| Host rebuilt / textfile dir wiped, timer never fired since | Series absent — `systemctl start ch-schema-snapshot.service` and confirm `ls /var/lib/node_exporter/textfile_collector/ch_schema_snapshot.prom` |

## Mitigation — `stellarindex_ch_schema_snapshot_offsite_stale`

The snapshot exists on this host and no copy of it has reached off-site
storage in the last 72 h, so the only copy of the lake's DDL sits on the
pool it protects. Two properties of the alert decide the triage:

- It is **per host**. The second arm is `stellarindex_ch_schema_snapshot_last_success_unix
  unless on (instance) max_over_time(…offsite_last_success_unix[72h])`:
  every host that has a local snapshot, minus the hosts that pushed
  inside the window. The alert carries the `instance` of the host whose
  copy is local, and one host pushing never hides another that is not.
- It is **not gated** on whether an off-site target was ever configured.
  A host that has never had one is exactly as loud as a host whose push
  is failing, because the two are the same exposure. (Until 2026-09-04
  the absent arm required `offsite_configured == 1`, so a
  never-configured target — r1's state — was the one condition the
  alert could not report.)

Split the two with the gauge the alert's description names — it is
emitted on every run, whether or not a target is set:

```sh
curl -s http://127.0.0.1:9100/metrics | grep ch_schema_snapshot_offsite
# or, in Prometheus, for the instance the alert names:
#   stellarindex_ch_schema_snapshot_offsite_configured{instance="<host>:9100"}
```

| `offsite_configured` | Meaning | Fix |
| --- | --- | --- |
| `0`, or the gauge is absent | No off-site target has ever been configured on this host (an absent gauge is a pre-2026-08-28 script — same verdict). Nothing is failing; nothing has ever been tried. | Set `ch_schema_snapshot_mc_target` (an `mc` alias/prefix on off-site storage) in the host's inventory, configure the matching `mc` alias for root on the host, and apply the backup surface: `ansible-playbook -i inventory/<host>.yml playbooks/archival-node.yml --diff --tags backup`. On r1 this is [v1-launch-plan](../v1-launch-plan.md) row 1.12, which also lists the expected transients. |
| `1` | A target is configured and the push is failing, or has never succeeded since it was set. The stamp is written only on a successful push and the textfile is rewritten every run, so a failing push makes the stamp **absent** on this host, not old. | `journalctl -u ch-schema-snapshot.service -n 50` shows `OFFSITE PUSH FAILED`; check the `mc` alias and credentials behind `SNAPSHOT_MC_TARGET` (`mc alias list`, `mc ls <alias>/…` as root), that `mc` is installed, and the remote's quota. Run `/usr/local/bin/ch-schema-snapshot.sh` by hand — exit `2` means the snapshot was written but the push failed. |

The alert clears on the first successful push: the stamp lands and the
`unless` arm drops the host. It cannot be acknowledged away on pubnet.
The archival-node role (`18-pgbackrest-backup.yml`) refuses the backup
surface (`--tags backup` / `pgbackrest`) on a pubnet host without a
target — ADR-0043's restore is schema-first, so a re-derive has nothing
to replay into if the DDL went down with the pool — and on a full run it
reports the gap by name (`Report a local-only ClickHouse schema snapshot
on pubnet`) and carries on rather than aborting, so the weekly drift
check and everything after step 18 still apply. `ch_schema_snapshot_offsite_ack`
is accepted only on test nets, whose lakes rebuild from
`deploy/clickhouse/*.sql` in minutes; even there it satisfies the role,
not this alert (the test nets run no Prometheus today, so nothing
evaluates it there yet).

### Interim off-box copy while r1 has no offsite store

r1 has no off-site object storage provisioned yet (same gap as
`pgbackrest_offsite_ack`). Until it does, the repo itself is an off-box
copy of the DDL — public, versioned, and not on this host:

```sh
# from a checkout, with access to the ClickHouse HTTP port
OUT_DIR=deploy/clickhouse/schema-snapshot TEXTFILE_DIR=/dev/null \
  scripts/ops/ch-schema-snapshot.sh
git add deploy/clickhouse/schema-snapshot && git commit
```

Do this after any DDL change, and at least monthly. It is an operator
action, tracked in the operator register — the timer cannot do it
(no repo credentials on r1, and committing from production is its own
bad idea). It does **not** clear the alert, and should not: nothing on
the host verifies the copy, and it carries none of the cursor state. It
narrows the exposure while the target is provisioned; it is not the
target.

## The drift check — the other half of the snapshot

The snapshot answers *what IS the live schema*. It cannot answer *is
the live schema what the repository says it should be* — and until
2026-07-25 nothing did. Tier-1 lake DDL is **hand-applied**: there is
no ClickHouse migration runner the way `migrations/` + golang-migrate
cover Postgres, so an `ORDER BY` changed by hand during an incident, a
column added and never codified, or a table the repo declares that was
never created, produced no signal anywhere. That matters more than it
sounds: a 60M-ledger re-derive against a sort order that differs from
the one the repo assumes does not error — it silently writes a
differently-ordered table.

`configs/ansible/roles/archival-node/files/ch-schema-drift.sh` closes
that (it lives in the role's `files/` dir because that is where this
repo keeps textfile-collector emitters, and where
`scripts/ci/lint-metric-refs.sh` looks for a metric's producer). It runs daily
(`ch-schema-drift.timer`, 04:10 UTC — 30 minutes after the snapshot, so
it always reads that day's complete capture) and compares, per table
declared in `deploy/clickhouse/tier1_schema.sql`:

| Compared | Why |
| --- | --- |
| existence | Declared in the repo, missing live. |
| `ENGINE` incl. arguments | A `ReplacingMergeTree` whose version column changed dedupes differently. `system.tables.engine` does **not** carry the arguments — this is why the check parses `SHOW CREATE`, not the inventory TSV. |
| `PARTITION BY` | Re-shapes every part. |
| `ORDER BY` | The one that silently corrupts a rebuild rather than failing it. |
| column names, in order | An added / dropped / reordered column. |

Column **types** are reported as `INFO`, never as drift: ClickHouse
re-renders types, DEFAULTs, CODECs and TTLs in its own canonical form,
and enforcing textual equality on those is a false-positive machine —
which ends with the check being ignored, i.e. worse than no check. The
snapshot's `schema.sql` remains the authoritative record of exact live
types.

Tables that exist **live but are absent from `tier1_schema.sql`** are
counted and listed but do not fail: `tier1_schema.sql` is the *founding*
DDL, and materialized views, serving tables and later migrations have
legitimately landed on top of it. Enforcing that direction needs a named
baseline with a per-entry reason under `scripts/ci/` (so
`scripts/ci/lint-baseline-growth.sh`'s `Baseline-Growth:` tripwire covers
it) — an ungated allowlist elsewhere would be the same hole with a new
address. Until then `stellarindex_ch_schema_drift_uncodified` makes the
growth visible.

Exit codes: `0` clean, `1` drift, **`2` could-not-compare**. Two is a
failure on purpose — "we could not check" must never be recorded as
"checked, and fine", which is the equivalence this whole ADR exists to
break. The systemd unit sets no `SuccessExitStatus`.

Run it by hand:

```sh
# against the newest snapshot on the box
ch-schema-drift.sh

# against a live SHOW CREATE sweep, no snapshot needed
LIVE=1 ch-schema-drift.sh

# from a checkout, against a snapshot you copied down
INTENT=deploy/clickhouse/tier1_schema.sql \
  LIVE_SCHEMA=/path/to/schema.sql TEXTFILE_DIR=/dev/null \
  configs/ansible/roles/archival-node/files/ch-schema-drift.sh
```

**When it fires, decide which side is wrong before touching anything.**
Drift has two directions and they need opposite fixes: a hand change on
r1 that was never codified (update `tier1_schema.sql` in the same change
that re-applies or accepts it), or repo DDL that was never applied (apply
it). Editing the repo to match live without understanding which is which
codifies the incident.

Metrics: `stellarindex_ch_schema_drift_divergent` (alert on `> 0`),
`_tables`, `_uncodified`, `_last_run_unix` (staleness). The alert rules
themselves are not yet in `deploy/monitoring/rules/storage.yml` — the
producer ships first; wiring the rule is a follow-up in that file.

Self-test: `configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh` (hermetic, no
ClickHouse required — mutates a ClickHouse-rendered fixture one attribute
at a time and asserts each is caught).

## Related

- ADR-0043 §2 — why a full CH backup is rejected and what replaces it.
- ADR-0027 — the `aws-public-blockchain` cold tier that makes the raw
  ledgers independently recoverable.
- `docs/operations/drills/restore-drills.md` — the drill evidence log.
- `backup-failed.md` — the Postgres half of the same ADR.

## Changelog

- 2026-09-04 — `_offsite_stale` is per host and ungated. The absent
  arm was one fleet-wide boolean gated on `offsite_configured == 1`:
  a host that had never configured a target (r1) could not fire, and any
  host pushing would have hidden another that was not. The arm is now
  `last_success_unix unless on (instance) max_over_time(offsite_last_success_unix[72h])`,
  naming the host; the gauge stays as the triage split above. The role
  refuses the backup surface on pubnet without a target and reports the
  gap on a full run instead of aborting it. Promtool coverage: the
  never-configured, gauge-absent, two-host and push-stopped cases in
  `deploy/monitoring/rule-tests/storage-backup_test.yml`.
- 2026-08-28 — absent-series coverage (audit finding backup-restore-2):
  `_stale` gains `or absent_over_time(...[36h])`, `_offsite_stale` gains
  an absent branch gated on `offsite_configured == 1` (new gauge
  `stellarindex_ch_schema_snapshot_offsite_configured`, emitted every
  run) — that gate was removed 2026-09-04, see above; the gauge remains
  — and `stellarindex_ch_schema_snapshot_unit_failed` is added.
  Before this the never-succeeded / every-run-failed cases — the ones the
  alerts exist for — evaluated over an empty vector and could not fire.
  Promtool coverage: `deploy/monitoring/rule-tests/storage-backup_test.yml`.
- 2026-07-25 — initial version, shipped with the §2.1 implementation
  (script + daily timer + staleness alerts). Before this the ClickHouse
  lake had no backup of any kind and no alert that said so.
- 2026-07-25 — added "The drift check" (C6-008 / C6-003):
  `ch-schema-drift.sh` + `ch-schema-drift.timer` compare
  `deploy/clickhouse/tier1_schema.sql` against the snapshot's live
  `SHOW CREATE`. The snapshot recorded the live schema daily but nothing
  ever asserted it matched the repo, and hand-applied Tier-1 DDL has no
  migration runner to notice.
