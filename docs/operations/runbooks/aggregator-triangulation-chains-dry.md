---
title: Runbook — aggregator-triangulation-chains-dry
last_verified: 2026-07-25
status: draft
severity: P3
---

# Runbook — `stellarindex_aggregator_triangulation_chains_dry`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_triangulation_chains_dry` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/aggregator.yml` |
| Typical MTTR | 30 min – 4 h |
| Impact | The composite-rate corroboration factor (`triangulation_agreement`) contributes nothing. Thin-pair rates (XLM/GBP, XLM/EUR, ETH/GBP) fall back to the confidence they had before the chains existed — a manipulated single-source print on those pairs has one fewer independent check. **Served prices are unaffected**: `/v1/price` continues to publish direct rates. |

## Background

The composite-rate upgrade (`e9803e69`) shipped four default
triangulation chains — XLM/EUR, XLM/GBP, ETH/GBP, BTC/GBP — each
computed as the deep USD pair × a fiat cross. They exist because live
measurement found those pairs sitting at 44–100% single-source
minutes, where the anomaly freeze's `source_count <= 1` leg is
permanently pre-satisfied and nothing corroborates the print.

The fiat/fiat leg does NOT resolve through the VWAP cache — no
fiat/fiat pair has a producer there. It resolves through the X2.5
forex-snap path: `Store.FXQuoteAtOrBefore` reads the `fx_quotes`
hypertable (daily buckets, source `massive`, written by the forex
worker in the API binary) with a **7-day lookback**
(`fxQuotesSnapLookback`). If no row exists inside that lookback, the
snap misses, the cached-VWAP fallback also misses (nothing produces
fiat/fiat VWAPs), and the chain returns `missing_leg`.

That failure is **silent by design** — a chain that cannot resolve
simply does not publish, and the direct price serves untouched. This
alert is the thing that makes it non-silent.

### Why the sibling FX alert does not cover this

`stellarindex_aggregator_fx_snap_fallback_dominant` cannot fire in
this state, and that was the residual risk this runbook closes:

- its numerator counts *fallbacks* (snap missed, cached VWAP tried),
  which for four chains at one tick each sits around **0.4/s** —
  under its own `> 0.5` floor;
- its denominator is `clamp_min(ok_rate, 1)`, so with `ok` pinned at
  **zero** the ratio is bounded by the numerator and can never reach
  0.5.

A partial FX degradation trips that alert. A *total leg drought* is
exactly the state it is blind to.

## Symptoms

- `sum(rate(stellarindex_aggregator_triangulations_total{outcome="missing_leg"}[15m])) > 0`
  while
  `sum(rate(stellarindex_aggregator_triangulations_total{outcome="ok"}[15m])) == 0`,
  sustained 30 min.
- `/v1/price` responses for the chained pairs show the
  `triangulation_agreement` confidence factor absent / weight 0
  (`triangulation_checked=false` on the wire).
- No customer-visible error; volumes, freshness and prices all normal.

## Quick diagnosis (≤ 5 min)

```sh
# 1) Confirm the shape: missing_leg counting, ok flat at zero.
#    (All five outcomes are pre-seeded at zero, so a genuine 0 here is
#    a real zero, not a scrape gap.)
curl -fs http://localhost:9464/metrics \
  | grep '^stellarindex_aggregator_triangulations_total'

# 2) Is the fiat-FX feed alive at all? This gauge only advances on a
#    committed non-empty fx_quotes batch.
curl -fs http://localhost:9463/metrics \
  | grep '^stellarindex_external_fx_last_quote_unix'

# 3) Is there an fx_quotes row inside the 7-day snap lookback for the
#    crosses the chains need?
psql -d stellarindex -c \
  "SELECT ticker, source, max(bucket_day) AS latest, now()::date - max(bucket_day) AS days_old
     FROM fx_quotes
    WHERE ticker IN ('EUR','GBP')
    GROUP BY ticker, source
    ORDER BY latest DESC;"
```

Decision tree:

| `fx_quotes` newest row | Probable cause | Go to |
| ---------------------- | -------------- | ----- |
| Older than 7 days | Forex worker dead / API binary not running the worker | Mitigation 1 |
| Fresh, but only for tickers the chains do not use | Chain config references a cross with no feed | Mitigation 2 |
| Fresh for EUR + GBP | Not an FX problem — the chain's *crypto* leg is missing from the VWAP cache | Mitigation 3 |
| Table empty | Feed never started on this deployment | Mitigation 1 |

## Mitigation

1. **Dry FX feed (the common case).** The `massive` forex worker runs
   inside the API binary. Check it is up and its last poll succeeded
   (`stellarindex_external_fx_last_quote_unix{source="massive"}`), then
   check the upstream credential/quota. Chains recover on the next
   aggregator tick after the first committed batch — no restart of the
   aggregator is needed.
2. **Chain configured against a cross with no feed.** The chains are
   supplied at the ansible template layer, deliberately not in
   `config.Default()`: a chain only makes sense where the deployment's
   pair set contains its legs. Either add the missing cross to the
   forex worker's ticker list or drop the chain from the deployment's
   `aggregate.triangulations` block.
3. **Crypto leg missing.** `missing_leg` is also returned when the
   NON-fiat leg's VWAP key is absent from Redis. Confirm the deep USD
   pair (e.g. XLM/USD) is publishing — if it is not, this alert is
   downstream of a bigger problem and
   `stellarindex_aggregator_silent` / the pair's own freshness alerts
   are the primary signal.
4. **Verification.** `outcome="ok"` starts incrementing within one
   aggregator tick of the legs resolving; the alert clears after its
   30 min `for:` window.

## Known false-positive patterns

- **Fresh deployment / first boot.** Until the forex worker's first
  committed batch there is no fiat cross, so every chain reports
  `missing_leg`. Expected for the first minutes of a cold start; the
  30 min `for:` absorbs it.
- **A deployment with no chains configured.** Then `missing_leg` never
  increments and the alert cannot fire — which is correct: an
  unconfigured chain set is a deployment choice, not a fault.
- **Aggregator down entirely.** Both series go absent, the `and`
  yields nothing, and this alert stays quiet on purpose —
  `stellarindex_aggregator_silent` owns that case.

## Related

- `aggregator-fx-snap-fallback-dominant.md` — the *partial*
  degradation case (snap missing but chains still publishing).
- `internal/aggregate/orchestrator/triangulate.go` — chain evaluation
  and the outcome labels.
- `internal/storage/timescale/fx_quotes.go` —
  `fxQuotesSnapLookback` (7 d) and the `massive` provenance label.
- `docs/reference/metrics/README.md`
  § `stellarindex_aggregator_triangulations_total` — the outcome enum.

## Changelog

- 2026-07-25 — initial draft, closing the residual risk recorded with
  the composite-rate corroboration change (`e9803e69`): a dry FX feed
  leaves all four chains dead with no alert able to see it.
