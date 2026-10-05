---
title: Post-launch on-call query bundle (L6.7)
last_verified: 2026-05-04
status: operator runbook
---

# Post-launch on-call query bundle

PromQL the on-call runs in Grafana / `promtool query instant` during the **L6.7
first-24h post-launch watch**, each with its expected shape. They complement the
alerts in `configs/prometheus/rules.r1/*.yml` (multi-host copies:
`deploy/monitoring/rules/*.yml`): alerts fire on **bad**, these paint **normal**.

Save each as a starred query in the "Stellar Index — Launch Watch" folder.
Grafana variables: `$range` (default `5m`), `$instance` (across API binaries).

## 1. Request rate per surface

```promql
sum by (route) (rate(http_requests_total[$range]))
```

Healthy: every public surface non-zero once the showcase site drives traffic:
`/v1/price`, `/v1/price/tip`, `/v1/observations`, `/v1/history/since-inception`,
`/v1/assets`, `/v1/oracle/*`, `/v1/sources`, `/v1/issuers`,
`/v1/issuers/{g_strkey}`, `/v1/markets`, `/v1/changes/{entity_type}/{id}`,
`/v1/diagnostics/cursors`. `route="unmatched"` is a 404: fine at low rate,
suspicious if sustained.

## 2. Error rate per surface

```promql
sum by (route) (rate(http_requests_total{status=~"5.."}[$range]))
```

**Bar**: < 0.1% of total request rate per surface (the SLA target is ≥ 99.9% availability). Sustained 5xx on one surface is SEV-2 minimum; triage in runbook `api-5xx.md`.

## 3. p95 / p99 latency per surface

```promql
histogram_quantile(0.95,
  sum by (le, route) (rate(http_request_duration_seconds_bucket[$range])))

histogram_quantile(0.99,
  sum by (le, route) (rate(http_request_duration_seconds_bucket[$range])))
```

**Bar**: p95 ≤ 200ms, p99 ≤ 500ms (service SLA; formal evidence is
`cmd/stellarindex-sla-probe`).

> **Carve-out**: `/v1/markets` does a `GROUP BY base_asset, quote_asset` across
> the 14-day chunk window of `trades`; expect p95 ≤ 300 ms / p99 ≤ 1 s (matches
> k6 `07-catalogue-browse`). `/v1/assets`, `/v1/issuers`,
> `/v1/diagnostics/cursors` hold the standard 200 ms / 500 ms.

## 4. Oracle freshness — every source, every asset

```promql
time() - stellarindex_oracle_last_update_unix
  > 0.5 * stellarindex_oracle_staleness_budget_seconds
```

Rows = a (source, asset) pair has used half its staleness budget; empty =
healthy. `stellarindex_oracle_stale` (`divergence.yml`) fires at the full
budget; this is the early-warning band. The budget is per (source, asset): 10×
the source's declared resolution by default (Reflector 5 min, so 50 min;
Redstone per batch push), or `[[oracle.staleness_overrides]]` for a pair with
its own rhythm. No join needed: the budget gauge carries the same labels as
`last_update_unix`.

## 5. Source events rate by source

```promql
sum by (source) (rate(stellarindex_source_events_total[$range]))
```

Healthy: every source in `internal/sources/external/registry.go` is non-zero,
including oracle/aggregator-class ones (coverage, not VWAP). Source-stopped
fires at zero; this catches abnormal **drops** before zero.

## 6. Aggregator tick health

```promql
sum by (outcome) (rate(stellarindex_aggregator_ticks_total[$range]))
```

Healthy: `outcome="ok"` matches the tick interval (default 30s, ~120/h);
`outcome="error"` only during transient redis/store hiccups. Sustained errors
are SEV-2.

## 7. VWAP cache writes — pair coverage proxy

```promql
rate(stellarindex_aggregator_vwap_writes_total[$range])
```

Unlabelled global throughput; expected = `len(pairs) × len(windows) ×
ticks_per_minute`. Significant under-reporting points at empty-window storms.

## 8. Decode errors per source

```promql
sum by (source) (rate(stellarindex_source_decode_errors_total[$range]))
```

**Bar**: < 1 error/min per source in steady state. A spike on a Soroban source
after an `update_contract` upgrade is SEV-2 ([contract schema evolution](../architecture/ingest-pipeline.md#contract-schema-evolution)
for the pattern, `decode-errors` runbook for the response).

## 9. Confidence-score distribution (spot-check)

`/v1/price` carries `confidence` in [0, 1]; spot-check popular pairs in the
first hour:

```sh
for pair in "native,fiat:USD" "USDC-G...,fiat:USD" ; do
  base=${pair%,*}; quote=${pair##*,}
  curl -s "https://api.stellarindex.io/v1/price?base=${base}&quote=${quote}" \
    | jq '{ pair: "'$pair'", confidence: .data.confidence,
            factors: .data.confidence_factors }'
done
```

Healthy: ≥ 0.7 for major XLM pairs. < 0.5 sustained signals single-source
degradation or a divergence spike nearing alert thresholds.

## 10. Rate-limit fail-open events

```promql
rate(stellarindex_ratelimit_fail_open_total[$range])
```

Non-zero = Redis misbehaving and the middleware failed open (by design: serve
traffic, unguarded rates). Should trend zero; pair with the Redis dashboard.

## 11. Closed-bucket stream subscriber health (L3.9)

```promql
sum by (outcome) (rate(stellarindex_api_stream_subscribe_total[$range]))
sum by (outcome) (rate(stellarindex_aggregator_stream_publish_total[$range]))
```

Healthy: publisher `ok` and subscriber `ok` track each other (within
reconnection noise); `error` / `decode_error` / `malformed` near zero. A
non-trivial gap means fan-out is dropping events: investigate the Redis pub/sub
channel.

## 12. Trade inserts vs `usd_volume` populate ratio

```promql
sum by (source, usd_volume_populated)
  (rate(stellarindex_trade_inserts_total[$range]))
```

Healthy: off-chain CEX/FX sources are dominated by `yes`. On-chain sources
show `yes` only for trades priced by the USD-volume tiers (see
[usd-volume-coverage-plan.md](usd-volume-coverage-plan.md)).

## Launch-day use

[`launch-day-checklist.md`](launch-day-checklist.md) §T-0 step 7 points here.
Keep queries 1-7 + 11 in a tabbed dashboard; pull 8-10 + 12 when investigating.

## Cross-references

- [`deploy/monitoring/README.md`](../../deploy/monitoring/README.md) — alert rules.
- [`docs/operations/launch-day-checklist.md`](launch-day-checklist.md) — cutover orchestration.
- [`docs/operations/sla-probe.md`](sla-probe.md) — SLA-evidence probe (15-min cron).
- [`docs/reference/metrics/README.md`](../reference/metrics/README.md) — every metric referenced.
