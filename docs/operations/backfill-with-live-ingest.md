---
title: Running backfill + verify-archive alongside live ingest
last_verified: 2026-05-28
status: ratified
---

# Running backfill + verify-archive alongside live ingest

F-0020: a 12-way parallel `soroban-events` fill walk plus a 12-chunk
verify-archive bootstrap exhausted Postgres connections and froze on-chain
trade ingest for 7 h. The W28 back-pressure design is correct (it blocks the
fill producer when its sink is full, so no row lands in `soroban_events`
after its cursor advanced past it), but when the live indexer shares Postgres
with that sink its writes block too. The live indexer must have priority.

## Posture

The live indexer has Postgres to itself; write-heavy walks run in
maintenance windows where the live cursor may lag.

- **Live indexer** always running at full priority; its cursor advances every ledger.
- **Fill walks (`stellarindex-ops backfill`)**: `-parallel 4` or lower while
  live ingest writes. `-parallel 12` only when live ingest is paused or on a
  fresh box.
- **verify-archive**: `verify-archive-tier-a.timer` fires daily at 03:23 UTC
  under `run-heavy-job.sh` (queues behind other heavy jobs). An ad-hoc run
  should `systemctl stop stellarindex-indexer` first OR use `-workers 4` or lower.
- **Galexie + ledgerstream-fill** read MinIO, not Postgres; they do not
  contribute.

## Alerts that page on recurrence

| Signal | Alert | Severity |
| --- | --- | --- |
| Live cursor stalls | `stellarindex_ingestion_source_insert_stale` | ticket |
| Live indexer keeps inserting duplicates only | `stellarindex_ingestion_duplicate_flood` | ticket |
| Aggregator output stops | `stellarindex_aggregator_silent` | **page** |
| Per-asset staleness > 120 s | `stellarindex_api_price_stale` | ticket |
| Postgres connections > 80 % of `max_connections` | `stellarindex_timescale_connections_saturated` | ticket |

Severities are the rules' `labels.severity` (`page` → Discord
#stellarindex-pages, `ticket` → #stellarindex-alerts). Rules live in
`configs/prometheus/rules.r1/`. Duplicate-flood runbook:
[ingestion-duplicate-flood](runbooks/ingestion-duplicate-flood.md).

If any fires while a fill walk or verify-archive runs, assume back-pressure
and stop the heavy walker first.

## Operator commands

### Stop a running fill walk

```sh
# On r1. The fill is a manual invocation (`stellarindex-ops backfill -source
# soroban-events`, normally under run-heavy-job.sh), NOT a systemd unit.
ps -eo pid,args | grep '[s]tellarindex-ops backfill'
# Graceful: drains in-flight rows then exits. Kill by EXPLICIT PID; a
# `pkill -f 'backfill'` self-matches your own ssh shell.
kill -INT <pid>
```

SIGINT lets the in-flight batch finish, preserving cursor coherence. SIGKILL
loses that batch's rows; use it only if the process has not exited within 60 s.

### Stop a running verify-archive

```sh
systemctl stop verify-archive-tier-a.service
```

The timer stays armed; the next scheduled fire still happens.

### Resume the live indexer after a freeze

```sh
# The symptom is "cursor not advancing", not "process not running". Confirm:
systemctl status stellarindex-indexer
# Cursor lag check:
curl -sS http://localhost:3000/v1/diagnostics/cursors \
  | jq '.data[] | select(.source=="ledgerstream") | .lag_seconds'
# Should be under 30 s in steady state.
```

If lag stays high after the heavy walker is stopped:

```sh
systemctl restart stellarindex-indexer
journalctl -u stellarindex-indexer -f
```

## Unshipped hardening options

1. **Per-sink prioritisation in AsyncSink**: live writes take connections
   ahead of fill walks. Moderate cost, medium risk (tracked under W28).
2. **Separate replica for fill-walk writes** via logical replication. High
   cost, medium-high risk (W28 / W30).
3. **Connection-pool reservation**: reserve N connections for the live
   indexer via per-binary pool sizes. Cheapest; prevents connection
   starvation but not in-database lock contention. The next hardening step.

Related: F-0028 (soroban_events lag, same cluster), W30 (cold-tier
interaction with backfill).
