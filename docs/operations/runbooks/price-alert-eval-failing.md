---
title: Runbook — price-alert-eval-failing
last_verified: 2026-07-05
status: living
severity: P3
---

# Runbook — `stellarindex_price_alert_eval_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_price_alert_eval_failing` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/price-alerts.yml` |
| Typical MTTR | 5–30 min (usually Postgres reachability recovery) |
| Impact | The aggregator's price-alert evaluator can't read the enabled `price_alerts` set, so NO customer price alerts are evaluated. Customers who registered alerts receive no `price.alert` webhooks even when their threshold is crossed. The public pricing surface is unaffected — this is a notifications-only degradation (BACKLOG #60). |

## Symptoms

- `rate(stellarindex_price_alert_eval_total{outcome="list_error"}[5m]) > rate(stellarindex_price_alert_eval_total{outcome="ok"}[5m])` sustained 30+ min.
- Aggregator log lines repeat `price-alert sweep: list enabled alerts failed` with the underlying error (Postgres unreachable, query timeout, permission denied on `price_alerts`).
- Customers report their price-threshold webhooks stopped firing.

## Background — why this fires

When `[price_alerts] enabled = true`, the aggregator runs a ticker
loop (`internal/pricealerts.Worker`, default 30 s) that:

1. `ListEnabledPriceAlerts` — reads every enabled row from `price_alerts`.
2. For each, reads the latest closed 1 m VWAP for the pair and, on a
   crossing (respecting cooldown + `last_fired_at`), enqueues a
   `price.alert` delivery into `webhook_deliveries` for the owning
   account's subscribed webhooks.

`list_error` means step 1 failed — the whole sweep is skipped, so
nothing is evaluated. Common causes:

1. **Postgres unreachable** from the aggregator (network / restart /
   failover). Self-recovers once the connection is back.
2. **`price_alerts` permission denied (42501)** — the table was created
   by the `postgres` superuser instead of the `stellarindex` app role
   (migrations/README rule 7). Fix: `ALTER TABLE price_alerts OWNER TO
   stellarindex`.
3. **Query timeout** under heavy DB load. Usually transient.

`partial_error` (a subset of alerts hit a price-read / enqueue error)
is intentionally NOT part of this alert — it is narrower and self-heals
per-alert.

## Quick diagnosis (≤ 5 min)

```sh
# 1) Confirm which outcome dominates.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_price_alert_eval_total'

# 2) Underlying error from aggregator logs.
journalctl -u stellarindex-aggregator -n 100 \
  | grep 'price-alert sweep: list enabled alerts failed'

# 3) Confirm the table is readable by the app role.
psql "$STELLARINDEX_POSTGRES_DSN" -c 'SELECT count(*) FROM price_alerts;'
```

## Decision tree

| Underlying error | Likely cause | Mitigation |
| ---------------- | ------------ | ---------- |
| connection refused / timeout | Postgres down / failover | Wait for recovery; check `postgres-ping-failing` |
| `permission denied for table price_alerts` | Migration applied as superuser | `ALTER TABLE price_alerts OWNER TO stellarindex` (migrations/README rule 7) |
| statement timeout | DB under load | Check DB load; alert auto-resolves once queries complete |

## Mitigation (≤ 30 min)

- [ ] **Confirm Postgres reachability** from the aggregator host.
- [ ] **Fix table ownership** if the error is `42501` (see decision tree).
- [ ] **Verify** `rate(stellarindex_price_alert_eval_total{outcome="ok"}[5m])`
      recovers above the `list_error` rate; the alert auto-resolves
      after 30 min sustained.
- [ ] If the evaluator should be off, set `[price_alerts] enabled =
      false` and restart the aggregator — the series stops and the
      alert clears.

## Root cause analysis

Capture for the postmortem: the underlying error class, whether the
`price_alerts` table ownership was wrong, and the outage duration
(alert FIRING → RESOLVED).

## Known false-positive patterns

- **Cold start**: aggregator boots, first sweep fires before Postgres
  is ready. The `for: 30m` clause masks this; a single restart blip is
  not this alert.

## Related

- [`docs/architecture/platform-spec.md`](../../architecture/platform-spec.md)
  — §5.3 customer webhooks (the delivery side price alerts reuse).
- `internal/pricealerts/` — the evaluator package.
- `internal/api/v1/dashboardpricealerts/` — the CRUD surface.
- Sibling alert: `postgres-ping-failing` (the broader "aggregator can't
  reach Postgres" signal).

## Changelog

- 2026-07-05 — initial draft alongside the price-alert evaluator
  (BACKLOG #60).
