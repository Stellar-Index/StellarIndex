---
title: Runbook — ClickHouse schema + state snapshot (ADR-0043 §2.1)
last_verified: 2026-09-09
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
(`ch-schema-drift.timer`, 04:10 UTC) and compares, per table declared in
`deploy/clickhouse/tier1_schema.sql`:

| Compared | Why |
| --- | --- |
| existence | Declared in the repo, missing live. |
| `ENGINE` incl. arguments | A `ReplacingMergeTree` whose version column changed dedupes differently. `system.tables.engine` does **not** carry the arguments — this is why the check parses `SHOW CREATE`, not the inventory TSV. |
| `PARTITION BY` | Re-shapes every part. |
| `ORDER BY` | The one that silently corrupts a rebuild rather than failing it. |
| column names, in order | An added / dropped / reordered column. |

### What it compares *against* — live, by default

The check runs its own `SHOW CREATE` sweep off ClickHouse. That is the
default (`ch_schema_drift_use_live: true`) and it is what "repo intent vs
live" in the unit's `Description` means.

Until 2026-09-09 the default read the newest **daily snapshot** instead,
while every description of the check — the unit, the timer, the metric
`HELP`, this runbook — said *live*. Measured that morning on r1 and on
the test nets, in both directions:

- **False drift.** r1's capture was written 05:41; that day's deploy
  shipped a newer `tier1_schema.sql` at 17:17. Eight tables the new
  intent declares were missing from a file written before they existed,
  so the check reported them "ABSENT from the live schema" — all eight
  were live, `account_creator_edges` alone holding 20.9M rows. It would
  have cleared itself at the next morning's capture. Red for a reason
  that is not real and then green on its own is the worst shape a
  control has: it teaches the operator to wait it out.
- **Hidden drift**, the worse half. A comparison that never reads the
  server does not measure live-vs-intent at all — and when ClickHouse is
  *down* it does not fail either: it reports `0 divergent` off a capture
  that ages silently, which is exactly the "we could not check" ==
  "we checked and it's fine" equivalence this ADR exists to break.

The cost argument for reading the snapshot ("one sweep a day serves both
the backup and the check") does not survive measurement: the sweep is
~45 `SHOW CREATE`s over loopback, the same work the snapshot job already
does daily, and the comparison itself is sub-second. The check also runs
*on* the node it queries.

The snapshot mode is kept as an explicit choice — a retained capture is
the only way to ask "did the repo match live on 2026-08-01?", and 90 days
of them are on the box for that. What it may no longer do is answer with
a capture it cannot answer from:

| Mode | Selected by | Reads |
| --- | --- | --- |
| live *(default)* | nothing, or `LIVE=1` | a fresh `SHOW CREATE` sweep via `$CH_HTTP` |
| capture | `LIVE_SCHEMA=<file>` | the file the operator named |
| snapshot | `LIVE=0` | the newest `schema.sql` under `$SNAPSHOT_DIR` |

In snapshot mode, if the intent file is **newer than the capture** the
check **refuses** (exit `2`) and names both sides rather than reporting
drift. A capture written before the intent was installed cannot tell a
table that is genuinely missing live from one that was declared after
the capture was taken. It is a refusal, not a mute: with a capture at
least as new as the intent, that mode still reports every real
divergence, and every message names the file it read
(`ABSENT from the snapshot …/schema.sql (captured 20260908T054100Z)`)
rather than claiming "the live schema".

### What it compares *with* — an intent that can prove it is current

The `live` side is now read straight off the server. The **intent** side
is a file the archival-node role ships to the host, and until 2026-09-09
that was the whole of it: the check's reference arrived by *the same
convergence the check is supposed to police*. Measured that day on both
test nets, by pulling the `transactions` DDL out of each host's own
shipped `tier1_schema.sql` and comparing it to that host's live schema:

| Host | Shipped intent | Live schema | Verdict |
| --- | --- | --- | --- |
| testnet | 2026-09-08 — post-#482 | pre-#482 | **DRIFT** — correct |
| futurenet | 2026-08-26 — pre-#482 | pre-#482 | **clean** — false |

Both hosts carried the *same* stale live schema
(`transactions.ingested_at` last, where the repo has put it after `memo`
since [#482](https://github.com/Stellar-Index/StellarIndex/pull/482)
merged on 2026-09-02). testnet reported it. futurenet read clean only
because its intent file was stale by the same two weeks — two wrongs
reading as a right. **The check went green exactly where the host was
furthest behind**, which is the one shape a control must never have.

So the shipped copy now carries its own provenance, stamped by the role's
*Ship the repo's Tier-1 lake DDL* task as `--` comments the parser
already strips:

```
-- Intent-Version: v0.67.0                 the release the shipped copy came from
-- Intent-Schema-Commit: e2465baf6 (2026-09-09)   when the DDL content last changed
```

and the check compares `Intent-Version` against **what this host is
running** — the `deployed-versions` sidecars, the same "live source of
record" [`deployed-versions.md`](../deployed-versions.md) names and
`deploy.yml`'s config-apply baseline reads. Lowest tag across the
release-managed binaries, `stellarindex-migrate` excluded, exactly as
`deploy.yml` computes it (#427): config from a release is unapplied if
*any* binary predates it, and `migrate` legitimately lags and gates no
config surface. Only `Intent-Version` moves the verdict; the schema
commit is there so the operator can see how old the DDL itself is
without an SSH round-trip.

**Three outcomes, not two.** When the intent predates the deployed
release the check reports **`NOT CONVERGED`** — it does not pass, and it
does *not* report drift. Reporting drift would blame the schema for a
provisioning gap and send the operator into "decide which side is wrong"
below, which is the wrong procedure and the one that ends with
`tier1_schema.sql` edited to match a server it was never compared
against. It is a refusal (exit `2`, this script's existing
*could-not-compare*): a reference that is not the repo's current
statement cannot answer whether live is what the repository says it
should be. Nothing is compared, so no drift gauge is published.

An **unstamped** intent splits by who supplied it. The shipped copy is
stamped by the same role run that installs the script, so an unstamped
*shipped* copy is itself a provisioning gap and refuses the same way. An
intent the **operator** named — a checkout's
`deploy/clickhouse/tier1_schema.sql`, a copy pulled down by hand — is
stamped by nothing and is not refused: it compares, and says in the log
and in the metric that its verdict is `UNVOUCHED`.

A release that changes no config surface still moves the sidecars, so a
host can read `NOT CONVERGED` while its DDL is in fact current. That
costs one idempotent role run. The opposite error is what futurenet
already had, and a 60M-ledger re-derive against an `ORDER BY` the repo
does not declare does not error — it silently writes a mis-sorted table.
This is also the position `scripts/ci/ansible-drift.baseline` already
takes on this very task: repo-ahead-of-host "IS drift — the fix is to
apply the playbook", not an allowance. And unlike a hardcoded expected
count, the state is always clearable by an action the operator controls,
so it cannot become the permanently-firing alert that is the same as no
alert.

**Clearing it** re-ships the intent from the current checkout:

```sh
cd configs/ansible
ansible-playbook -i inventory/<host>.yml playbooks/archival-node.yml \
  --tags ch-schema-drift
```

Then re-run the check. The verdict it gives afterwards is trustworthy —
and if it is `DRIFT`, that drift is real and "decide which side is wrong"
below applies.

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

Exit codes: `0` clean, `1` drift, **`2` could-not-compare** — which
covers both "ClickHouse is unreachable" and "the intent side is not
current" (`NOT CONVERGED`). Two is a failure on purpose — "we could
not check" must never be recorded as "checked, and fine", which is the
equivalence this whole ADR exists to break. The systemd unit sets no
`SuccessExitStatus`, and `ch-schema-drift.service` is not in the
`stellarindex_systemd_unit_failed` exclusion list, so a refusal pages a
ticket through that alert. The exit code does not distinguish the two
kinds of refusal; the metrics below do.

Run it by hand:

```sh
# against live — the default; needs no snapshot and no environment.
# Also checks its own intent against the deployed-versions sidecars.
ch-schema-drift.sh

# against the newest retained capture on the box (up to a day old)
LIVE=0 ch-schema-drift.sh

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
`_tables`, `_uncodified`, `_last_run_unix` (staleness), `_live`
(`1` = the verdict came from a fresh sweep, `0` = from a captured file —
a clean verdict is only as current as what it read), and the convergence
half added on 2026-09-09:

| Series | Meaning |
| --- | --- |
| `_intent_verified` | `1` = both the intent's stamp and the host's release were readable, so `_intent_converged` is a real verdict. `0` = one side was unavailable; the drift verdict beside it is **unvouched**. This is the gate's own canary, the same idea as `stellarindex_binary_version_probe_success`. |
| `_intent_converged` | `1` = the shipped intent is from a release at least as new as the one this host runs. `0` = `NOT CONVERGED`. **Absent** when unverified — an absent verdict is honest, a fabricated one is not. |
| `_intent_info{intent_version,host_version}` | The two releases actually compared, so the gap is legible without SSH. |

The three states are separable, and never by exit code alone:

```promql
# converged and clean
stellarindex_ch_schema_drift_intent_converged == 1
  and stellarindex_ch_schema_drift_divergent == 0

# converged and drifted  — the ch-schema-restore procedure below applies
stellarindex_ch_schema_drift_intent_converged == 1
  and stellarindex_ch_schema_drift_divergent > 0

# NOT CONVERGED — a provisioning gap; re-apply the role, then re-read
stellarindex_ch_schema_drift_intent_converged == 0
```

A `NOT CONVERGED` run rewrites the textfile with **only** the
`_last_run_unix` and convergence series: it compared nothing, so
`_divergent` goes absent rather than carrying an invented count — and,
more to the point, rather than leaving the previous run's `divergent 0`
on the floor for node_exporter to serve indefinitely, which would be the
same false clean by a slower route.

The alert rules themselves are not yet in
`deploy/monitoring/rules/storage.yml` — the producer ships first; wiring
the rules (including one on `_intent_converged == 0`) is a follow-up in
that file.

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

- 2026-09-09 — **the drift check can tell whether its own reference is
  current.** The intent side is a file the archival-node role ships, so
  it arrived by the same convergence the check polices: measured that day
  on both test nets, testnet (intent 09-08, post-#482) correctly reported
  its pre-#482 live schema as DRIFT while futurenet (intent 08-26,
  pre-#482) read **clean** off the *same* stale live schema, because its
  intent was stale by the same two weeks. The check went green exactly
  where the host was furthest behind. The shipped copy now carries
  `-- Intent-Version:` and `-- Intent-Schema-Commit:`, stamped at ship
  time, and the check compares the former against the release the host
  runs (the `deployed-versions` sidecars — lowest managed binary,
  `migrate` excluded, as `deploy.yml` computes it). An intent older than
  the deployed release is a third outcome, `NOT CONVERGED`: exit `2`,
  no comparison, no drift verdict, and no `_divergent` gauge — a
  provisioning gap must not be reported as schema drift, which would send
  the operator to the wrong half of this runbook. An unstamped *shipped*
  copy refuses the same way; an unstamped intent the *operator* named
  compares but is marked `UNVOUCHED`. New gauges
  `stellarindex_ch_schema_drift_intent_verified`, `_intent_converged` and
  `_intent_info`, which make converged-and-clean, converged-and-drifted
  and not-converged separable in Prometheus. Coverage: 12 new assertions
  in `configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh`
  (47 total), each proven red by mutation.
- 2026-09-09 — **the drift check compares against live by default.** It
  had compared against the newest daily snapshot while calling itself
  "repo intent vs live", which cost both directions: 8 live r1 tables
  reported ABSENT because a deploy shipped a newer intent than the
  morning's capture, and no live-vs-intent measurement at all (including
  a confident `0 divergent` whenever ClickHouse was down). `LIVE` now
  defaults to a fresh `SHOW CREATE` sweep; `LIVE=0` selects the snapshot
  explicitly and **refuses** (exit `2`) when the intent is newer than the
  capture; every DRIFT/UNCODIFIED line names the source it actually read;
  and the unit's and timer's `Description` render from the same variable
  as `Environment=LIVE=`, so they cannot again advertise a mode the unit
  does not set. New gauge `stellarindex_ch_schema_drift_live`. Coverage:
  9 new/changed assertions in
  `configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh`,
  each proven red against the pre-fix script.
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
