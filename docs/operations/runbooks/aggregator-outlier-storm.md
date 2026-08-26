---
title: Runbook — aggregator-outlier-storm
last_verified: 2026-08-26
status: draft
severity: P3
---

# Runbook — `stellarindex_aggregator_outlier_storm`

> **2026-08-26 rescope.** The alert was `sum(rate[10m]) > 5×(rate[1h] offset 1h)`
> for 15m. That comparator **self-poisoned** — a sustained storm's own early
> minutes entered the `[T-2h,T-1h]` baseline and flipped the ratio false at
> ~72m, so it could never survive the very storm it was meant to catch — and
> at 15m it ticketed on every benign single-pair trimming burst. It is now an
> **absolute per-pair sustained gate**:
> `sum by (pair) (rate(...{reason="outlier"}[10m])) > 10` for **2h**. Read
> "storm" below as **persistent price dispersion on one pair**, not a relative
> spike. Covered by `deploy/monitoring/rule-tests/aggregator_test.yml`.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_outlier_storm` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/aggregator.yml` |
| Typical MTTR | 30 min – several hours |
| Impact | One pair's robust-VWAP filter has trimmed >10 trades/s for 2h+. The filter trims toward the intra-window **median** (symmetric MAD band, no oracle reference), so the **published price is protected** — this is a data-quality / source-health signal, not a customer-facing price error. Harm is a source being wholesale-rejected (data completeness) or a durable price dispersion worth investigating. |

## Symptoms

- `sum by (pair) (rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m])) > 10` sustained past the 2h `for:`, for a **single** `pair`.
- The published VWAP for that pair is typically still correct — cross-check the
  pair's `div:<pair>` Redis flag / API `flags.divergence_warning` for actual
  price impact before assuming the served number is wrong.
- **Do NOT wait on `stellarindex_price_divergence_{warning,critical}`** — those
  alerts are INERT (F-1329, no Prometheus producer; divergence values live in
  Postgres + the `div:` Redis cache + the API flag, not the registry).

## Quick diagnosis (≤ 5 min)

```sh
# 1) Confirm the spike is real, not just a baseline calibration issue.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_dropped_trades_total{reason="outlier"}'

# 2) Is the upstream-trade rate also elevated? (real volume → real outliers)
psql -d stellarindex -c \
  "SELECT pair, COUNT(*) AS rows
   FROM trades
   WHERE timestamp > now() - interval '15 minutes'
   GROUP BY pair ORDER BY 2 DESC LIMIT 10;"

# 3) Sanity-check by pair: is the storm one pair or many? The drop
# counter carries the configured target pair (since 2026-08-14).
# In PromQL:
#   topk(5, rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m]))
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_dropped_trades_total{reason="outlier"' | sort
```

Decision tree:

| Trade-rate elevated? | Multiple pairs? | Probable cause | Mitigation |
| -------------------- | --------------- | -------------- | ---------- |
| Yes | Many | Real market-wide event (BTC flash-move, fiat-pair news) | Wait it out; verify divergence-warning fires + clears alongside |
| Yes | One pair | Pair-specific dislocation, possibly a depeg | Check the pair's primary venue; consider pausing stablecoin proxy for that quote if it was a peg break |
| No | Many | Connector regression — every venue producing weird amounts | Check `stellarindex_source_decode_errors_total`; likely a recent decoder change |
| No | One pair, one source | Single connector misbehaving (amount-decimal regression) | Disable that source via config; open ticket against the connector |

### Spam-wave signature (2026-08-14 case)

A fourth cause, seen live on 2026-08-14: an SDEX **token farm** — one
issuer (`GBGRBCUB6…`) spamming self-trades across its own tokens —
rather than a market event or a connector bug. Signature: the drops
concentrate on a single configured pair, the dropped rows show wide
price dispersion (25–37% between consecutive trades — real markets
don't gap like that repeatedly) and **dust-sized** volumes, and all
trace to one issuer account in the trades table. Diagnose with
`topk` by `pair` on the drop counter (step 3 above — the `pair` label
exists because attributing this exact wave took ad-hoc SQL), then
confirm the single-issuer pattern in `trades` for that pair. The σ
filter is doing its job — the VWAP input is protected; treat it as
abuse of the venue, not of the aggregator, and wait it out unless the
surviving sample gets too thin (then see `min_usd_volume` gating).

## Mitigation (≤ 15 min)

- [ ] **Real market event**: leave the filter doing its job. Annotate
      the on-call channel with the incident timestamp + the pair(s)
      affected so the postmortem-window divergence numbers have context.
- [ ] **Connector regression**: identify the offending source via
      `stellarindex_source_events_total` × `stellarindex_source_decode_errors_total`
      ratio + recent deploy diff. Disable that source in TOML
      (`[external.<venue>] enabled = false`) and reload — the
      orchestrator picks up the change at next tick.
- [ ] **Filter mis-calibration**: if neither of the above holds and
      the storm is sustained > 1 h, lower
      `aggregate.outlier_sigma_threshold` from 4.0 → 5.0 / 6.0 to
      let more rows through while RCA continues — a wide filter
      with weak signal beats a narrow filter dropping legitimate
      data.
- [ ] **Verification**: `dropped_trades_total{reason="outlier"}`
      rate returns within 5× of baseline.

## Root cause analysis

Capture for the postmortem:

- A 1-hour metric range showing the spike + recovery.
- Trade-table samples around the spike — what *did* the wild rows
  look like? (Source, pair, base/quote amounts, timestamp.)
- If it was a connector regression: the most recent commit touching
  the offending source's `parse.go` / `decode.go`.
- If it was a real market event: external context (CoinGecko /
  CoinMarketCap headlines, Tradingview screenshots) so the postmortem
  doesn't read as a pure stellarindex failure when it's the world.

## Known false-positive patterns

- **(Historical — removed 2026-08-26)** The old `offset 1h` comparator
  divide-by-zero in the first hour after deploy no longer applies: the
  absolute gate has no baseline term. A cold start is silent until a
  pair's trim rate actually exceeds 10/s for 2h.
- **Aggregator restart**: ticks bunching up post-restart can briefly
  inflate the per-10m rate; the 2h `for:` absorbs it — a restart blip
  clears long before the gate elapses.
- **First tick of a new pair**: a freshly-added pair with sparse
  trades has σ ≈ 0 → every row drops. Mitigation: the filter's
  fewer-than-3-prices guard should kick in (verify
  `aggregate.FilterOutliers` returned input unchanged); if it
  didn't, that's a real bug.

## Related

- `aggregator-silent.md` — frequently co-fires when the storm
  filters out *every* row.
- ADR (TBD) σ-vs-MAD outlier filter — long-term migration plan if
  σ-threshold turns out to be too brittle on small windows. Until
  the ADR lands the σ default lives at
  `aggregate.outlier_sigma_threshold = 4.0` in TOML.
- `internal/aggregate/outliers.go` — filter implementation. Any
  algorithm change must update this runbook.

## Changelog

- 2026-04-25 — initial draft alongside the aggregator metrics
  PR #26 wire-up.
- 2026-08-24 — per-pair `pair` label on the drop counter (task #29);
  spam-wave signature section from the 2026-08-14 token-farm storm.
- 2026-08-26 — **rescope**: replaced the self-poisoning
  `5×[1h] offset 1h` / 15m comparator with an absolute per-pair
  sustained gate (`sum by (pair) rate[10m] > 10`, for 2h) after live
  pubnet data showed it ticketed on benign single-pair trimming bursts
  (peaks 130–320/s, self-clearing <1h) while being structurally unable
  to fire on a genuinely sustained storm. Clarified the price is
  median-protected; flagged the INERT `price_divergence_*` alerts.
  Added `rule-tests/aggregator_test.yml`.
