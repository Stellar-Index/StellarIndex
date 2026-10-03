---
title: Runbook — change-summary-stale
last_verified: 2026-10-03
status: ratified
severity: P3
---

# Runbook — `stellarindex_change_summary_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_change_summary_stale` |
| Severity | P3 (ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/aggregator.yml` (and `configs/prometheus/rules.r1/aggregator.yml`) |
| Typical MTTR | 15 min |
| Impact | Explorer delta strips (h1/h24/d7/d30, ATH/ATL, streak) serve frozen rows. |

## Symptoms

- `time() - stellarindex_change_summary_last_success_unix` is over 30 min:
  no pass of the change-summary worker upserted any entity.
- `stellarindex_change_summary_passes_total{outcome="failed"}` is rising.

## Quick diagnosis (≤ 5 min)

```sh
systemctl is-active stellarindex-aggregator
journalctl -u stellarindex-aggregator --since "-30min" | grep 'change-summary pass had failures'
```

The warn carries `first_err`. Typical causes:

- `no closed observations in window` for every entity: `prices_1m` is not
  advancing; check the price cagg refresh and ingest freshness.
- A Postgres error: reachability, lock contention, or a missing
  `change_summary_5m` migration.

## Mitigation (≤ 15 min)

- [ ] Fix the cause above. The next 5-minute pass upserts every entity.
- [ ] Verification: `stellarindex_change_summary_last_success_unix` advances
      within 5 min and the alert clears.

## Known false-positive patterns

`partial` passes are normal (a pair with no recent trades fails each pass)
and do not trip this alert; only passes with zero upserts do.

## Related

- `internal/aggregate/changesummary/rollup.go`
- [aggregator-silent](aggregator-silent.md) — the aggregator process itself is down.
