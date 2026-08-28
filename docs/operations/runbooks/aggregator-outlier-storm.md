---
title: Runbook — aggregator-outlier-storm
last_verified: 2026-08-28
status: draft
severity: P3
---

# Runbook — `stellarindex_aggregator_outlier_storm` / `stellarindex_aggregator_outlier_trim_fraction`

> **2026-08-28 redesign.** The alert used to gate on
> `sum by (pair) (rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m])) > 10`
> for 2h. On 2026-08-28 it fired for **hours** on `crypto:XLM/fiat:USD`
> and `/GBP` while binance / coinbase / kraken agreed within 0.9 % and the
> served prices were right. The counter measured how many prints the
> **whole-window** median+MAD band trimmed — re-counted every 30 s tick
> per window — and that band trims an *agreed* regime shift (Kraken's
> genuine +2 % XLM/GBP step, matching the XLM/USD × GBP/USD cross) until
> the shift becomes the window majority. It measured a filter artifact,
> not venue disagreement. Two things changed:
>
> 1. The filter is now **time-local** (`internal/aggregate/outliers_local.go`):
>    a print is dropped only when it disagrees with the whole window
>    AND its own neighbourhood (own / adjacent 1-minute buckets, or the
>    nearest 5 prints each side on a thin series). Steps survive; lone
>    wild prints do not.
> 2. `outlier_storm` reads **venue disagreement** directly:
>    `max by (pair) / min by (pair)` of
>    `stellarindex_aggregator_venue_vwap{window="5m"}` − 1 > **1 %** across
>    **≥ 2 venues** for **15 m**. Its sibling
>    `outlier_trim_fraction` reads the CURRENT 24h window's trim share
>    from `stellarindex_aggregator_window_trades` (> **20 %** trimmed,
>    ≥ 20 trades, for **30 m**) — the single-venue spam shape that
>    disagreement cannot see. Both are covered by
>    `deploy/monitoring/rule-tests/aggregator_test.yml`.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_outlier_storm` (venue disagreement) / `stellarindex_aggregator_outlier_trim_fraction` (in-window trim share) |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/aggregator.yml` |
| Typical MTTR | 30 min – several hours |
| Impact | `outlier_storm`: one pair's venues have disagreed by > 1 % on their 5m VWAPs for 15 m+ — one venue is stale, thin, mis-decoding amounts, or a stablecoin leg is skewed. `trim_fraction`: the time-local filter is rejecting > 20 % of one pair's 24h window — a spam / wash / dust wave inside the window. In both cases the **published price is protected** (robust, time-locally filtered VWAP across venues); this is a data-quality / source-health signal, not a customer-facing price error. Harm is a source being wholesale-rejected (completeness) or a thin surviving sample. |

## Symptoms

- `outlier_storm`: `max by (pair)(stellarindex_aggregator_venue_vwap{window="5m"}) / min by (pair)(...) − 1 > 0.01` with ≥ 2 venue series, for a **single** `pair`, sustained past 15 m.
- `trim_fraction`: `1 − window_trades{stage="outlier"} / window_trades{stage="class"} > 0.2` on `window="24h"` with ≥ 20 class-filtered trades, sustained past 30 m.
- The published VWAP for that pair is typically still correct — cross-check the
  pair's `div:<pair>` Redis flag / API `flags.divergence_warning` for actual
  price impact before assuming the served number is wrong.
- **Do NOT wait on `stellarindex_price_divergence_{warning,critical}`** — those
  alerts are INERT (F-1329, no Prometheus producer; divergence values live in
  Postgres + the `div:` Redis cache + the API flag, not the registry).
- An **agreed** move across venues (the 2026-08-28 shape) does **not** fire
  either alert any more. If you see a market-wide move alongside a ticket,
  the ticket is about a venue that did *not* move with the others.

## Quick diagnosis (≤ 5 min)

```sh
# 1) outlier_storm: which venue is the odd one out? One line per source.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_venue_vwap{' | grep 'window="5m"' | sort
# In PromQL, per pair:
#   stellarindex_aggregator_venue_vwap{pair="<pair>",window="5m"}

# 2) trim_fraction: how much of the window is the filter rejecting, and
#    how thin is the survivor set?
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_aggregator_window_trades{' | grep 'window="24h"' | sort

# 3) Is the upstream-trade rate also elevated? (real volume → real outliers)
psql -d stellarindex -c \
  "SELECT pair, source, COUNT(*) AS rows
   FROM trades
   WHERE timestamp > now() - interval '15 minutes'
   GROUP BY pair, source ORDER BY 3 DESC LIMIT 10;"

# 4) The per-tick drop counter is still there as a diagnostic (it re-counts
#    window residents every tick — read it as "band-residents", not "new
#    outliers/s"):
#   topk(5, rate(stellarindex_aggregator_dropped_trades_total{reason="outlier"}[10m]))
```

Decision tree:

| Trade-rate elevated? | Multiple pairs? | Probable cause | Mitigation |
| -------------------- | --------------- | -------------- | ---------- |
| Yes | Many | One venue lagging a real market-wide move (stale feed, throttled poller) — an agreed move no longer tickets | Check that venue's poller freshness; wait it out once it catches up |
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
trace to one issuer account in the trades table. Since 2026-08-28 this
shape fires `outlier_trim_fraction` (a single venue cannot disagree with
itself, so `outlier_storm` stays silent on it). Diagnose with
`window_trades{stage=...}` for the pair (step 2 above) plus `topk` by
`pair` on the drop counter (step 4 — the `pair` label exists because
attributing this exact wave took ad-hoc SQL), then confirm the
single-issuer pattern in `trades` for that pair. The filter is doing
its job — the VWAP input is protected; treat it as abuse of the venue,
not of the aggregator, and wait it out unless the surviving sample gets
too thin (then see `min_usd_volume` gating). Note the time-local filter
lets a run of > 5 consecutive same-level prints in a THIN series
validate itself — a long single-source spam run at one wrong level is
not rejected (no time-local definition can), so on an SDEX-only pair
also read the survivor count and `min_usd_volume` gating.

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
      `trim_fraction` is sustained > 1 h, raise
      `aggregate.outlier_sigma_threshold` from 4.0 → 5.0 / 6.0 to
      let more rows through while RCA continues — a wide filter
      with weak signal beats a narrow filter dropping legitimate
      data. (An agreed move being trimmed is a BUG in the time-local
      filter, not a calibration issue — capture the pair + window and
      file it.)
- [ ] **Verification**: the venue spread
      (`max/min − 1` of `venue_vwap{window="5m"}`) returns under 1 %,
      and `1 − window_trades{stage="outlier"} / {stage="class"}`
      returns under 0.2.

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

- **(Historical — removed 2026-08-28)** The counter-based gate fired on
  agreed regime shifts (the whole-window band trimmed the new regime)
  and re-counted the trimmed tail every tick. Neither current alert
  reads that counter.
- **Venue leaves the window**: `venue_vwap` series for a source that
  stopped trading are DELETED on the next refresh, so a stale venue
  cannot pin the spread. If a venue's series persists after it went
  quiet, that is a bug in `recordVenueVWAPs`.
- **Stablecoin-leg skew**: a USDT-quoted venue vs a USD-quoted one
  diverge by exactly the USDT/USD basis; > 1 % sustained is a real
  depeg signal, not noise — escalate to the pricing owner.
- **First tick of a new pair**: fewer than 3 valid prices → the filter
  is a no-op (`aggregate.FilterOutliersLocal` returns the input
  unchanged) and `trim_fraction` needs ≥ 20 trades; both stay silent.

## Related

- `aggregator-silent.md` — frequently co-fires when the storm
  filters out *every* row.
- ADR (TBD) σ-vs-MAD outlier filter — long-term migration plan if
  σ-threshold turns out to be too brittle on small windows. Until
  the ADR lands the σ default lives at
  `aggregate.outlier_sigma_threshold = 4.0` in TOML.
- `internal/aggregate/outliers_local.go` — the orchestrator's
  time-local filter; `internal/aggregate/outliers.go` — the
  whole-window form still used by `/v1/vwap` + `/v1/ohlc`. Any
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
- 2026-08-28 — **redesign**: the counter gate fired for hours on agreed
  venues (XLM/USD, XLM/GBP) because the whole-window MAD band trimmed a
  genuine +2 % step and the counter re-counted the tail. Filter made
  time-local (`FilterOutliersLocal`); `outlier_storm` now reads venue
  disagreement from `stellarindex_aggregator_venue_vwap` (> 1 %, ≥ 2
  venues, 15 m); new `outlier_trim_fraction` on
  `stellarindex_aggregator_window_trades` (> 20 % of the 24h window,
  ≥ 20 trades, 30 m) for the single-venue spam shape. promtool cases
  rewritten: agreed step silent, one venue +3 % fires, single venue
  never fires.
