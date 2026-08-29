---
title: Runbook — pg-conns-saturated
last_verified: 2026-08-28
status: current
severity: P2
---

# Runbook — `stellarindex_timescale_connections_saturated`

> **R1 DEPLOYMENT REALITY (re-verified 2026-08-28).** PgBouncer is
> **NOT deployed** on r1 — the only trace in the repo is a
> future-tense comment in the archival-node role's `pg_hba.conf.j2`.
> Every binary connects **directly** to Postgres
> (`postgresql@15-main.service`, `max_connections = 200` —
> `configs/ansible/roles/archival-node/defaults/main.yml`
> `postgres_max_connections`). There is no pooler to queue behind, no
> `SHOW POOLS`, no `pool_size` to bump: at 100 % you get
> `FATAL: sorry, too many clients already`, full stop. The rule's own
> description ("PgBouncer should be smoothing this") is itself
> aspirational.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_connections_saturated` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. Metric source: `postgres_exporter` (`localhost:9187`). |
| Typical MTTR | 15–60 min |
| Impact | > 80 % of `max_connections` (200 on r1) in use. New connections start failing with `sorry, too many clients` as we approach 100 %. API latency rises; the indexer's `database/sql` pool fails to acquire connections. |

## Symptoms

- `(pg_stat_activity_count / pg_settings_max_connections) * 100 > 80`
  for ≥ 5 min.
- API latency climbs (connection acquisition becomes the bottleneck).
- Postgres log shows `FATAL: sorry, too many clients already` if
  we've hit 100 %.
- Companion signal: the indexer's `watchPostgresPing` goroutine
  (`cmd/stellarindex-indexer/main.go`) pings its pool every 60 s;
  when the DB stops answering, `stellarindex_postgres_ping_failing`
  **pages** (`postgres-ping-failing.md`) and the indexer journal
  logs the `pool may be wedged` line. If that page is firing too,
  you're at (or past) hard saturation, not just the 80 % ticket.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# What's the state distribution?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT state, wait_event_type, count(*)
   FROM pg_stat_activity
   GROUP BY 1, 2
   ORDER BY count DESC;"

# Who's the top holder?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT application_name, count(*)
   FROM pg_stat_activity
   WHERE state != 'idle'
   GROUP BY application_name
   ORDER BY count DESC;"

# Long-running transactions (common cause of accumulation)
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT pid, application_name, state, wait_event_type,
          now() - xact_start AS xact_age, left(query, 80)
   FROM pg_stat_activity
   WHERE xact_start IS NOT NULL
   ORDER BY xact_age DESC
   LIMIT 10;"
```

## Typical root causes

1. **Long-running transaction leaking connections.** A handler
   opens a transaction, does work, but never commits or rolls
   back because of an unhandled error path.
   - Signal: `xact_age` > 10 min on several connections from one
     `application_name`.
   - Mitigation: kill the offending pids:
     `SELECT pg_terminate_backend(pid)`; fix the code.

2. **Connection leak in one of our binaries** — goroutines that
   never release their `*sql.Conn` (the indexer's pool is Go's
   `database/sql` `*sql.DB`, not pgx). Usually shows as a steadily
   climbing count from one `application_name`.
   - Mitigation: restart the leaking binary
     (`systemctl restart stellarindex-<name>`); fix the leak with
     pprof / `sql.DBStats` introspection.

3. **Burst load** — a marketing campaign, a viral asset, a big
   external caller. Legitimate; the fix is capacity planning, not
   cleanup.

4. **Idle-in-transaction timeout not configured.** Without a
   timeout, a broken client's leaked transactions accumulate
   forever.
   - Mitigation: consider `idle_in_transaction_session_timeout`
     (e.g. `5min`) at the role or db level — via the archival-node
     ansible role, not a live-only edit.

## Lock-table pressure (`stellarindex_timescale_lock_table_pressure`)

The **other** alert that routes to this runbook:
`stellarindex_timescale_lock_table_pressure`
(`rules.r1/storage.yml`, `severity: ticket`, `for: 5m`) fires when
`sum by (instance)(pg_locks_count) / on (instance)
(pg_settings_max_locks_per_transaction × pg_settings_max_connections)
> 0.7`. The `sum` is load-bearing: postgres_exporter emits
`pg_locks_count` once per (datname, mode), so the pre-#301
unqualified division matched no series and this alert was silently
dead from the day the exporter landed. The lock table is a fixed
shared-memory arena sized `max_locks_per_transaction ×
max_connections`; exhausting it is SQLSTATE 53200
("out of shared memory") and dropped INSERTs — the 2026-05-06 SEV-3
class, which recurred 2026-05-15 when `trades` grew to ~2,738 chunks
and TimescaleDB's per-chunk locking thrashed the arena. Everything
above about `pg_stat_activity` finds **nothing relevant** for this
alert — it's lock slots, not connections.

Diagnosis:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT (SELECT count(*) FROM pg_locks) AS locks_held,
          current_setting('max_locks_per_transaction')::int
            * current_setting('max_connections')::int AS lock_table_size;"
```

r1 templates `max_locks_per_transaction = 4096`
(`postgresql.conf.j2`, `postgres_max_locks_per_transaction` in the
archival-node defaults — 819,200 entries with `max_connections=200`).
The fix for sustained pressure is bumping
`postgres_max_locks_per_transaction` in the archival-node ansible
role and reapplying (`--check --diff` first) — **never** a live-only
`ALTER SYSTEM` edit, which the next ansible run reverts (CLAUDE.md
"codify every host change"). The bump needs a Postgres restart.

## Mitigation

- [ ] Step 1 — identify the top holder (above). If the firing alert
      is `_lock_table_pressure`, jump to that section instead.
- [ ] Step 2 — if long-running xact: terminate the offenders with
      `pg_terminate_backend`, then fix the code.
- [ ] Step 3 — if a binary is leaking: restart it; consider
      `idle_in_transaction_session_timeout` as a backstop (via
      ansible).
- [ ] Step 4 — if genuine growth: raise `postgres_max_connections`
      in the archival-node role deliberately (each connection costs
      backend memory) — again via ansible + reapply, never live-only.
- [ ] Verification: utilization drops under 50 % sustained; no
      more `too many clients` errors in logs.

## Root cause analysis

- Which application name was the offender?
- Timeline of connection growth — steady climb (leak) or spike
  (burst)?
- Correlated API metrics — did request rate rise in lockstep?

## Known false-positive patterns

- **Backup process** briefly opens many connections (pgBackRest
  `--process-max`). Expected; narrow to backup window.
- **Brief spike during an outage of a downstream service**
  (Redis) — API handlers take longer, so connection hold-time
  rises, so utilization spikes. Resolves when the downstream
  recovers.

## Related

- `api-latency.md` — upstream symptom.
- `postgres-ping-failing.md` — the indexer-side page that fires when
  saturation becomes unavailability.
- `db-disk-full.md` — writes blocked → xacts can't commit →
  connections hold.

## Changelog

- 2026-08-29 — corrected the quoted `_lock_table_pressure` expr: it
  now sums `pg_locks_count` over the exporter's per-(datname, mode)
  series and joins `on (instance)`. The previous unqualified
  division could never match real postgres_exporter output, so the
  alert had been inert since F-0152 put the exporter on r1 (#301).
- 2026-08-28 — re-verified against HEAD. Added the R1 DEPLOYMENT
  REALITY banner: PgBouncer is not deployed (future-tense comment in
  `pg_hba.conf.j2` only); all PgBouncer diagnosis/mitigation
  (`SHOW POOLS` on :6432, `pool_size` bumps) replaced with
  direct-Postgres guidance (terminate offenders,
  `idle_in_transaction_session_timeout`, restart the leaking binary).
  Added the lock-table-pressure section
  (`stellarindex_timescale_lock_table_pressure` routes here; fix =
  `postgres_max_locks_per_transaction` bump in the ansible role, not
  a live edit). Indexer pool corrected to `database/sql` (not pgx),
  with `watchPostgresPing` / `stellarindex_postgres_ping_failing` as
  the companion signal. Rule citation → `rules.r1/storage.yml`;
  replica cross-ref dropped (no replica).
- 2026-04-23 — initial draft.
