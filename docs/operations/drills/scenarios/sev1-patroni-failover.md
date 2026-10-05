---
title: SEV-1 tabletop — Patroni-driven Timescale failover
last_verified: 2026-10-05
status: draft
severity: P1
exercises_runbook: ../../runbooks/timescale-primary-down.md
playbook_section: ../../sev-playbook.md#4-response-flow
---

# SEV-1 tabletop — Patroni-driven failover

> **UNVALIDATED, infra not deployed.** r1 runs one `postgresql@15-main.service`; the `patroni` role
> (`configs/ansible/roles/patroni/`) has no playbook. Commands and timings come from the role and
> [ha-plan §3.3](../../../architecture/ha-plan.md), not from a run. Before the first live drill: check each
> `patronictl` flag against the installed version, time one real switchover, set `status: ratified`.

~30 min, 3 people. Exercises [`timescale-primary-down.md`](../../runbooks/timescale-primary-down.md) (its
undeployed-HA appendix), [`replica-lag.md`](../../runbooks/replica-lag.md) and [SEV playbook §4](../../sev-playbook.md).
Point: Patroni promotes automatically but does not recover the *service*; the team must tell "cluster has a
leader" from "API and indexer are writing to it".

## Setup and trigger

Cluster `stellarindex-r1`: `db-01` Leader, `db-02`/`db-03` Replicas (sync mode `ANY 1`), etcd on the same three hosts,
lag 0 in `patronictl -c /etc/patroni/patroni.yml list`. No PgBouncer/HAProxy in front of Postgres (the `haproxy` role
fronts the API only); API and indexer dial the leader address directly. 10:15 UTC Thursday.

> 10:16 UTC `db-01` loses power (Postgres, Patroni and its etcd member). etcd keeps quorum (2 of 3); the leader key
> expires after `ttl` (30 s) and `db-02`, the sync standby, is promoted. API 5xx climbs; `/v1/readyz` 503, `postgres` `ok:false`.

## Beats (T+ min:sec)

| T+ | Beat |
| --- | --- |
| 0:00 | No page: `stellarindex_timescale_primary_down` (`pg_up == 0`) needs the exporter on `db-01`, which died with it, so `pg_up` is absent, never 0 |
| 0:40 | `patronictl list`: `db-02` Leader, `db-03` Replica, `db-01` absent; `stellarindex_patroni_role{role="leader"}` is 1 on `db-02` |
| 1:30 | `/v1/readyz` still 503: the API DSN points at `db-01` |
| 2:30 | Pages together: `stellarindex_api_error_rate_critical`, `stellarindex_postgres_ping_failing`, `stellarindex_postgres_exporter_down` for `db-01` (reads as a monitoring fault) |
| 5:00 | Customer asks why `/v1/price` is stale |
| 8:00 | `stellarindex_timescale_primary_down` still silent |
| 15:00 | `db-01` powers back; if rejoin as replica takes >30 s, `primary_down` fires for `db-01`, now a replica |
| 20:00 | `stellarindex_timescale_replica_lag` fires for `db-01` while it catches up |

## Expected response

- **5 min:** acknowledge, open channel, post "API errors elevated after a database node failure; investigating; updates every 15 min", status page *Investigating* on API.
- **15 min, diagnose:** `curl -sS https://api.stellarindex.io/v1/readyz | jq '.checks[] | select(.name=="postgres")'`.
  Read cluster state from a survivor: `ssh db-02 patronictl -c /etc/patroni/patroni.yml list` and `... history`; confirm one Leader and
  that it was the sync standby (no loss under `synchronous_commit=remote_apply`). Read `postgres_exporter_down` for `db-01` as host loss.
  The ongoing outage is client routing, not Patroni.
- **30 min, mitigate:** point API and indexer Postgres DSN at the new leader and restart them (indexer too, its pool can hold dead
  connections: [postgres-ping-failing.md](../../runbooks/postgres-ping-failing.md)). Do **not** run `patronictl failover`/`switchover`;
  promotion already happened. Verify: `/v1/readyz` 200 with `postgres` ok; `postgres_ping_failing` clears; ingest cursor advances
  (`stellarindex_ingestion_cursor_stuck` needs ~10 min); `prices_1m` gains a bucket (Timescale jobs on new leader);
  `sudo -u postgres pgbackrest --stanza=stellarindex check` on `db-02` (WAL archiving resumed).
- **1 h, restore redundancy:** `db-01` rejoins as Replica; if it cannot (diverged timeline, `pg_rewind` failure) run
  `patronictl -c /etc/patroni/patroni.yml reinit stellarindex-r1 db-01`, which **wipes `db-01`'s data directory**; only on the confirmed non-leader.
  Status page *Identified, Mitigated, Resolved*.
- **24 h:** postmortem per [§6](../../sev-playbook.md#6-after-the-incident).

## Pass criteria (aim >= 80% pass)

1. Acknowledged within 5 min, channel opened.
2. Confirmed against `/v1/readyz`, not only the metric.
3. Read `patronictl list` on a surviving node before acting.
4. Identified client routing (DSN still on `db-01`) as the cause of ongoing 503s.
5. Detected the outage without a `primary_down` page, read `postgres_exporter_down` as host loss, started no second failover.
6. Verified writes resumed (ping alert clears, cursor advances, `prices_1m` advances), not just readyz.
7. Treated `patronictl reinit` as destructive; ran it only on the confirmed non-leader.
8. Writeup within 24 h with action items.

## Gaps expected (design-seeded; replace with real findings after the first drill)

No Postgres front (every failover is a manual repoint); `primary_down` is blind to host loss (add `absent(pg_up)` per member or page on
`up{job="postgres_exporter"} == 0` for a db host); no cluster-level no-leader alert from `stellarindex_patroni_role{role="leader"}`;
`stellarindex_patroni_role` is a textfile metric and serves frozen values if the scraper dies
(`stellarindex_patroni_textfile_stale`, [patroni-textfile-stale.md](../../runbooks/patroni-textfile-stale.md)).

## Variants

- Planned switchover (first to run live on a staging cluster): `patronictl -c /etc/patroni/patroni.yml switchover stellarindex-r1 --candidate db-03`.
- etcd quorum loss: `db-02` and `db-03` drop; with `failsafe_mode` unset Patroni demotes `db-01` to read-only. Team must not force-promote by hand.
- Lagging candidate: both replicas more than `maximum_lag_on_failover` (1 MiB) behind; Patroni refuses to promote; RPO decision between waiting for `db-01` and accepting loss.
