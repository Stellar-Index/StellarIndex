---
title: Runbook — price-alert-eval-failing
last_verified: 2026-07-05
status: living
severity: P3
---

# Runbook — `stellarindex_price_alert_eval_failing`

## At a glance

Price-alert evaluator failing; one alert, one section.

P3 (ticket). Rule: `deploy/monitoring/rules/price-alerts.yml`. `rate(stellarindex_price_alert_eval_total{outcome="list_error"}[5m]) > rate(stellarindex_price_alert_eval_total{outcome="ok"}[5m])` for 30+ min: the aggregator's evaluator (`internal/pricealerts.Worker`, 30 s ticker, `[price_alerts] enabled = true`) cannot read the enabled `price_alerts` set, so NO customer `price.alert` webhooks fire. Public pricing is unaffected.

Sibling alerts: `stellarindex_price_alert_eval_no_successful_sweeps` (every alert errors: `partial_error` with zero `ok`), `stellarindex_price_alert_evaluations_failing` (over half of per-alert evaluations end `error` or `timeout`), `stellarindex_price_alert_sweep_stale` (no sweep for 15+ min). Each alert's reads share a deadline of a third of the sweep interval (10 s); a pair that times out every sweep is usually never-traded (`LatestClosedVWAP1mForPair`).

## Check

```sh
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_price_alert_eval_total'
journalctl -u stellarindex-aggregator -n 100 \
  | grep 'price-alert sweep: list enabled alerts failed'
psql "$STELLARINDEX_POSTGRES_DSN" -c 'SELECT count(*) FROM price_alerts;'
```

```promql
sum by (outcome) (rate(stellarindex_price_alert_evaluated_total[15m]))
time() - stellarindex_price_alert_last_sweep_unix
```

## Likely causes

- Postgres unreachable (connection refused / timeout): see [`postgres.md#stellarindex_postgres_ping_failing`](postgres.md#stellarindex_postgres_ping_failing).
- `permission denied for table price_alerts` (42501): table created by the superuser instead of the app role (migrations/README rule 7).
- Statement timeout under DB load (transient).

## Fix

1. Confirm Postgres reachability from the aggregator host.
2. On `42501`: `ALTER TABLE price_alerts OWNER TO stellarindex`.
3. Verify `rate(stellarindex_price_alert_eval_total{outcome="ok"}[5m])` exceeds the `list_error` rate; the alert resolves after 30 min sustained.
4. To turn the evaluator off: set `[price_alerts] enabled = false` and restart the aggregator.

Escalate: record the error class, whether ownership was wrong, and the outage duration. A restart blip is masked by `for: 30m`.

## Related

- [Alerts catalogue](../alerts-catalog.md)
