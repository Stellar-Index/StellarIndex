---
title: Runbook — protocol-events-rollup-failing
last_verified: 2026-07-06
status: draft
severity: P3
---

# Runbook — `stellarindex_protocol_events_rollup_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_protocol_events_rollup_failing` (informational) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/aggregator.yml` + `configs/prometheus/rules.r1/aggregator.yml` |
| Typical MTTR | 5–15 min (almost always Postgres reachability, shared with louder alerts) |
| Impact | `/v1/protocols` and `/v1/protocols/{name}` `events_24h` counters stop advancing — they hold their last-good value (stale, NOT zero). NO customer pricing impact. |

## Symptoms

- `stellarindex_protocol_events_rollup_sweeps_total{outcome="refresh_error"}`
  increasing; `outcome="ok"` flat.
- `journalctl -u stellarindex-aggregator | grep "protocol-events rollup refresh failed"`
  shows the wrapped Postgres error every ~2 min.
- The explorer's protocol pages / `/v1/protocols` show an `events_24h`
  number frozen at the last successful sweep.

## Quick diagnosis (≤ 5 min)

```sh
# Is it failing, and how often?
curl -s localhost:9464/metrics | grep stellarindex_protocol_events_rollup_sweeps_total

# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-aggregator --since -30min | grep -i "protocol-events rollup"

# refresh_error is almost always Postgres. Check the table exists
# (migration 0086):
sudo -u postgres psql stellarindex -c '\d protocol_events_24h'

# And that the census still runs by hand (this is the query the worker
# folds into the rollup):
sudo -u postgres psql stellarindex -c 'SELECT source, events_24h FROM protocol_events_24h ORDER BY events_24h DESC;'
```

## Mitigation (≤ 15 min)

- Missing table → migration 0086 didn't apply on this deployment. Run
  the migrator (deploy.yml auto-applies; manual:
  `stellarindex-migrate -dir /usr/local/share/stellarindex/migrations up`).
- Postgres down / lock contention → follow the storage runbook; this
  alert clears itself on the next successful sweep (the worker retries
  forever, and the upsert-then-prune is idempotent).
- No operator "catch-up" step exists or is needed: the next sweep
  recomputes the full trailing-24h census from scratch.

## Root cause analysis

The worker is dependency-thin: one census transaction (a UNION ALL
count over the served protocol hypertables) plus an upsert + prune, on
a 2-minute cadence. A failure here with healthy `/v1/price` traffic
almost always means the aggregator host lost Postgres reachability, or
the served tier is under lock pressure — check what else fired in the
same window.

## Known false-positive patterns

None known yet. The alert requires 30 min of continuous failures at a
2-min sweep cadence, so single transient Postgres blips do not fire it.

## Related

- Metric reference: [`stellarindex_protocol_events_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_protocol_events_rollup_sweeps_total)
  + [`stellarindex_protocol_events_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_protocol_events_rollup_sweep_duration_seconds)
- Worker: `internal/aggregate/protoeventsrollup/worker.go` (wired in `cmd/stellarindex-aggregator/main.go`)
- Census + upsert SQL: `internal/storage/timescale/protocol_stats.go`
- Table: `migrations/0086_create_protocol_events_24h_rollup.up.sql`
- Endpoint served from the rollup: `/v1/protocols` (`events_24h`)
- Catalogue row: [alerts-catalog.md](../alerts-catalog.md)

## Changelog

- 2026-07-06 — created with the protocol-events rollup (#43).
