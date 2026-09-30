---
title: SEV-1 tabletop — Patroni-driven Timescale failover
last_verified: 2026-09-30
status: draft
severity: P1
exercises_runbook: ../../runbooks/timescale-primary-down.md
playbook_section: ../../sev-playbook.md#4-response-flow
---

# SEV-1 tabletop — Patroni-driven Timescale failover

> **UNVALIDATED.** No Patroni cluster exists yet: r1 runs a single
> `postgresql@15-main.service` and the `patroni` role has no playbook
> (see the banner in
> [`timescale-primary-down.md`](../../runbooks/timescale-primary-down.md)).
> Every command and timing below is taken from the role
> (`configs/ansible/roles/patroni/`) and
> [ha-plan §3.3](../../../architecture/ha-plan.md), not from a run.
> Before the first drill against a real cluster: check each `patronictl`
> flag against the installed version, time one real switchover, and
> flip `status` to `ratified`.

Successor to
[`sev1-timescale-primary-failover.md`](sev1-timescale-primary-failover.md),
whose failover path was operator-driven because Patroni was absent.
~30 min for 3 people. Exercises
[`timescale-primary-down.md`](../../runbooks/timescale-primary-down.md)
(its *undeployed HA design* appendix is the Patroni path),
[`replica-lag.md`](../../runbooks/replica-lag.md) and
[SEV playbook §4 Response flow](../../sev-playbook.md).

The point of this drill: Patroni makes promotion automatic, but it does
not make the *service* recover. The team has to tell "cluster has a
leader" apart from "the API and indexer are writing to it".

## Initial conditions

Read aloud at drill setup.

- Patroni cluster `stellarindex-r1` on `db-01` (Leader), `db-02` and
  `db-03` (Replicas, sync mode `ANY 1`). etcd runs on the same three
  hosts. `patronictl -c /etc/patroni/patroni.yml list` shows all three
  `running`, lag 0.
- There is **no PgBouncer / HAProxy front for Postgres** — the
  `haproxy` role fronts the API only. API and indexer connect to the
  leader's address directly.
- `/v1/readyz` returns 200; SLA probe metrics within target.
- It is **10:15 UTC, Thursday**. Routine traffic.
- Oncall is `<participant 1>`. Backup oncall is `<participant 2>`.
  Engineering manager is reachable.

## Trigger event

Read aloud at drill T+0.

> At 10:16 UTC `db-01` loses power. Postgres, Patroni and the etcd
> member on it vanish together. etcd keeps quorum (2 of 3). The leader
> key expires after the Patroni `ttl` (30 s) and `db-02`, the
> synchronous standby, is promoted.
>
> The first user-visible signal: API 5xx climbs and `/v1/readyz`
> returns 503 with the `postgres` check `ok:false`.

## Injection timeline

Drill leader reads each beat in order; pauses after each for
participants to narrate their response.

| T+ | Beat |
| --- | --- |
| 0:00 | No page. `stellarindex_timescale_primary_down` (`pg_up == 0`) is fed by the exporter on `db-01`, which lost power with it: `db-01`'s `pg_up` goes absent, never 0. |
| 0:40 | `patronictl list` now shows `db-02` Leader, `db-03` Replica, `db-01` absent. `stellarindex_patroni_role{role="leader"}` is 1 on `db-02`. |
| 1:30 | `/v1/readyz` is **still 503**. The API's DSN points at `db-01`'s address. |
| 2:30 | Three pages land together: `stellarindex_api_error_rate_critical`, `stellarindex_postgres_ping_failing` (the indexer pool is still dialling `db-01`) and `stellarindex_postgres_exporter_down` for `db-01`, whose summary reads as a monitoring fault. |
| 5:00 | A customer asks why `/v1/price` is flagged stale. |
| 8:00 | `stellarindex_timescale_primary_down` has **still not fired**. The only alert naming `db-01` is the exporter-down page. |
| 15:00 | `db-01` powers back on. Its exporter returns before Postgres; if the rejoin as a replica takes over 30 s, `stellarindex_timescale_primary_down` now fires for `db-01` — a replica. |
| 20:00 | `stellarindex_timescale_replica_lag` fires for `db-01` while it catches up. |

## Expected response per the playbook

Drill leader compares participant narratives against this
expected sequence.

### Within 5 minutes (per [§2 Timelines](../../sev-playbook.md#2-timelines-the-sla-promises))

- Oncall acknowledges and opens `#incident-<YYYY-MM-DD>-<short>`.
- Initial post: "API errors elevated after a database node failure;
  investigating; updates every 15 min."
- Status page *Investigating* on **API**.

### Within 15 minutes — diagnose

- `/v1/readyz` first:
  `curl -sS https://api.stellarindex.io/v1/readyz | jq '.checks[] | select(.name=="postgres")'`.
- Cluster state from a surviving node, not the dashboard:
  `ssh db-02 patronictl -c /etc/patroni/patroni.yml list` and
  `... history`. Confirm a single Leader and that it was the sync
  standby (no data loss under `synchronous_commit=remote_apply`).
- Recognise the database outage without a primary-down page: on host
  loss that alert is silent, and `stellarindex_postgres_exporter_down`
  for `db-01` means the node is gone, not that monitoring broke. Both
  are per-host; neither says whether the cluster has a leader.
- Recognise the real outage is **client routing**: Patroni recovered,
  the services did not follow.

### Within 30 minutes — mitigate

- Point the API and indexer at the new leader (the services' Postgres
  DSN) and restart them. Restart the indexer even if it looks healthy —
  its pool can hold dead connections
  ([`postgres-ping-failing.md`](../../runbooks/postgres-ping-failing.md)).
- Do **not** run `patronictl failover` / `switchover` — promotion
  already happened. A manual switchover back to `db-01` is a later,
  planned action, not a mitigation.
- Verify recovery:
  - `/v1/readyz` → 200 with `{"name":"postgres","ok":true}`.
  - `stellarindex_postgres_ping_failing` clears; the ingest cursor
    advances (`stellarindex_ingestion_cursor_stuck` needs ~10 min).
  - TimescaleDB background jobs run on the new leader: `prices_1m`
    gains a fresh bucket.
  - WAL archiving resumed from the new leader:
    `sudo -u postgres pgbackrest --stanza=stellarindex check` on `db-02`.

### Within 1 hour — restore redundancy

- `db-01` rejoins as a Replica (`patronictl list`). If it cannot
  (diverged timeline, `pg_rewind` failure), rebuild it with
  `patronictl -c /etc/patroni/patroni.yml reinit stellarindex-r1 db-01`
  — this **wipes `db-01`'s data directory**; run it only on the
  confirmed non-leader.
- Status page *Identified* → *Mitigated* → *Resolved*.

### Within 24 hours — postmortem

Postmortem doc per [§6 After the incident](../../sev-playbook.md#6-after-the-incident).
Action items filed with owners + due dates.

## Validation criteria

Score `pass` / `partial` / `fail` per criterion. Aim for ≥ 80% pass.

| # | Criterion |
| --- | --- |
| 1 | Did oncall acknowledge within 5 min and open the channel? |
| 2 | Did anyone confirm against `/v1/readyz` rather than just the metric? |
| 3 | Did the team read cluster state from `patronictl list` on a surviving node before acting? |
| 4 | Did the team identify client routing (DSN still on `db-01`), not Patroni, as the cause of the ongoing 503s? |
| 5 | Did the team detect the outage without a `stellarindex_timescale_primary_down` page, read `stellarindex_postgres_exporter_down` for `db-01` as host loss, and not start a second failover? |
| 6 | Did the team verify writes resumed (ping alert clears, cursor advances, `prices_1m` advances), not just readyz? |
| 7 | Did the team treat `patronictl reinit` as destructive and run it only on the confirmed non-leader? |
| 8 | Did the writeup land within 24 h with action items? |

## Common gaps expected

Seeded from the design, not from a prior run — replace with real
findings after the first drill.

- **No Postgres front.** Without PgBouncer/HAProxy (or a multi-host DSN
  that selects the read-write node) every failover is a manual repoint.
  Action item template: "Land a Postgres front so clients follow the
  leader."
- **`primary_down` is blind to host loss.** `pg_up == 0` needs a live
  exporter on the same host; a powered-off node emits no `pg_up` at all.
  Action item template: "Add `absent(pg_up)` per cluster member, or page
  `up{job="postgres_exporter"} == 0` on a db host as a database outage."
- **No cluster-level signal.** Every Postgres alert is per-host. Action
  item template: "Add a no-leader alert from
  `stellarindex_patroni_role{role="leader"}`."
- **Stale Patroni metrics.** `stellarindex_patroni_role` is read from a
  textfile; if the scraper dies it serves frozen values
  (`stellarindex_patroni_textfile_stale`,
  [`patroni-textfile-stale.md`](../../runbooks/patroni-textfile-stale.md)).

## Variant scenarios

- **Planned switchover.** No fault; the drill leader asks for a
  maintenance switchover to `db-03`:
  `patronictl -c /etc/patroni/patroni.yml switchover stellarindex-r1 --candidate db-03`.
  Tests the client-repoint procedure without incident pressure; this is
  the first variant to run live once a staging cluster exists.
- **etcd quorum loss.** `db-02` and `db-03` both drop. Leader `db-01`
  survives but etcd has no quorum; with `failsafe_mode` unset, Patroni
  demotes it to read-only rather than risk split-brain. Tests whether
  the team recognises a DCS outage and does not force-promote by hand.
- **Lagging candidate.** Sync replication is broken before the fault
  and both replicas are more than `maximum_lag_on_failover` (1 MiB)
  behind. Patroni refuses to promote. Tests the RPO decision between
  waiting for `db-01` and accepting data loss.

## Pairs with

- [SEV-1 Timescale primary failover](sev1-timescale-primary-failover.md)
  — the single-node predecessor; run it while r1 is still unclustered.
- [SEV-2 Redis Sentinel failover](sev2-redis-sentinel-failover.md)
  — the other stateful tier's automatic failover.
