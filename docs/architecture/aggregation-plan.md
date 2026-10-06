---
title: Pricing and aggregation — from raw trade to served price
last_verified: 2026-10-05
status: binding — config keys, alert names and metric names checked against code 2026-10-05
---

# Pricing and aggregation

How a stored trade becomes a served price: the policy chain, how a
market's spellings and aliases fold into one population, cross-rate
routing, and thin markets. Oracle feeds and manipulation defenses are in
[oracle-manipulation-defense.md](oracle-manipulation-defense.md); ingest is
in [ingest-pipeline.md](ingest-pipeline.md); the system map is
[overview.md](overview.md); the asset-identity and stablecoin traps are in
[domain-traps.md](domain-traps.md). The public methodology is
[docs/methodology/vwap-aggregation.md](../methodology/vwap-aggregation.md).

Money is invariant 1 (ADR-0003): `canonical.Amount` / `*big.Int` legs,
exact `*big.Rat` for every price, mean and inverse, never `float64`;
decimal strings on the wire; a partial total is served as a lower bound
(`lower_bound`), never as a total.

## Ingest preserves truth; aggregation applies policy

Decoders never re-stamp a pair, never drop a trade by source class and
never drop an outlier. All three are policy, applied here:

- A USDT depeg is news, not a correction to hide, so ingest stores the
  real pair (`XLM/USDT`) and the aggregator maps `USDT→USD` at compute
  time. The map is `internal/aggregate/stablecoin.go`.
- A CoinGecko poll is data to record and expose via `/v1/sources`; it just
  does not weigh in VWAP.
- σ-deviance is window-relative; a print that is 5σ for one pair can be
  normal across pairs.

## The policy chain

`internal/aggregate/orchestrator` runs each configured `(pair, window)`
every `interval_seconds` (default 30):

1. **Stablecoin expansion** (`enable_stablecoin_fiat_proxy`, OFF by
   default). Expands a fiat-quoted target to its direct market plus its
   stablecoin backers (`XLM/USD` → `XLM/USDT`, `XLM/USDC`, `XLM/DAI`,
   `XLM/PYUSD`, `XLM/USDP`); `ProxyPair` / `ExpandTargetPair` re-stamp each
   rewritten trade onto the target. Runs first so the class filter judges
   each row by venue.
2. **Class filter** (ON; `disable_class_filter` turns it off). Drops every
   trade whose source is not `ClassExchange` in
   `internal/sources/external/registry.go`. Aggregator, oracle and
   authority-sanity classes never contribute VWAP weight. A venue listed
   on `/v1/sources` with `include_in_vwap=true` contributes; a mismatch is
   a bug.
3. **Outlier filter** (`outlier_sigma_threshold`, default 4.0;
   `internal/aggregate/outliers_local.go`). Robust centre and scale:
   median + 1.4826·MAD, not mean + stdev (masking-resistant). A trade is
   dropped only when it is more than σ robust scales from **every**
   reference: the window median **and** its time-local neighbourhood (its
   own and adjacent 1-minute buckets, or the nearest prints for a thin
   series). The neighbourhood test lets an agreed regime shift through
   and rejects a lone print; it was added after a live false-fire on
   `crypto:XLM/fiat:USD`. `keepIfVolumeMajority` (`outliers.go`) withholds
   the window when dropped prints outweigh the survivors' base volume, so
   dust cannot out-vote a large block.
4. **VWAP** (`internal/aggregate/vwap.go`): Σquote / Σbase over
   `[bucketEnd − W, bucketEnd)`, `bucketEnd` = last closed 1-minute
   boundary. Eligibility needs `min_usd_volume` (default 10,000 USD);
   `max_trades_per_window` (default 10,000) bounds the scan.
5. **Freeze / confidence** (ADR-0019) — see
   [oracle-manipulation-defense.md](oracle-manipulation-defense.md#freeze-layer-9).
6. Result to Redis at `vwap:<base>:<quote>:<window-seconds>`, TTL equal to
   the window; triangulated cross-pairs carry a `:provenance` marker.

### Two serving paths

The configured pair set is served from Redis, already filtered. `/v1/price`
also reads `LatestClosedVWAP1mForPair`: the newest closed `prices_1m`
bucket, a bare Σquote/Σbase CAGG (migration 0002) that skips the chain
above. A pair with no `prices_1m` rows (a synthetic `native/fiat:USD`)
falls through to Redis; any pair with real rows (`crypto:XLM/fiat:USD` via
Kraken/Coinbase books, a Soroban token quoted in `USDC-GA5Z…`) is served
from the CAGG. That read is wrapped in `aggregate.GuardServedVWAP`
(`internal/aggregate/served_guard.go`, wired in `cmd/stellarindex-api`
`storePriceReader.guardServedVWAP1m`): it compares the bucket against the
UNION of a wide ratio band and a MAD band over the pair's recent closed
buckets and, when grossly off, serves the newest clean trailing bucket.
It is deliberately conservative: it catches order-of-magnitude errors,
lets real volatility through (a depeg passes), fails **open** on too
little history, and is pure pass-through for healthy pairs. All math is
exact `*big.Rat`.

### Frozen pairs

A freeze fires only when all three hold (`phase2FreezeFires`,
`internal/aggregate/orchestrator/phase2_freeze.go`): confidence < 0.45,
z > 5 and source_count <= 1. Two of three does not freeze; it surfaces as
`flags.divergence_warning`. A frozen pair serves the held value from Redis
even when a `prices_1m` row exists, overriding the CAGG path above (pinned
by `TestFrozenPairServesHeldValueThroughProductionAdapters`,
`cmd/stellarindex-api/price_frozen_lkg_test.go`).

### Closed-bucket-only serving

ADR-0015: queries filter `bucket <= now() - INTERVAL '<granularity>'`, so
a closed bucket is deterministic and replicates byte-identical across
regions; `/v1/price` freshness is ≤30 s. The orchestrator writes Redis
only; CAGG refresh keeps the tables current. `/v1/price/tip` (ADR-0018)
is the deliberate live exception. Routing every read through the
closed-bucket guard is tracked as #689.

## Asset identity and aliases

An asset is `(code, issuer)`, a SAC address, or `native` — never a code.
XLM has three disjoint identities — `native`, `crypto:XLM` (off-chain
venues) and its SAC — with different venue populations. Every asset-id
read path loops `canonical.AssetAliases` (`internal/canonical/alias.go`);
a path that reads one spelling silently under-reports.

## Folding a market's two spellings

A market is stored in whichever direction the venue printed it. Two
folds put it back together, and they are different decisions:

> Folding a direction adds no market. Folding an alias adds one.

### The direction fold

`(A,B)` and `(B,A)` are the same trades. `Store.TradesInRange` and
`Store.FXQuoteAtOrBefore` (`internal/storage/timescale/trades.go`) read
both directions and re-express each flipped row in the requested
orientation by swapping its legs (`orientTradeTo`). Every aggregate —
VWAP, TWAP, OHLC, volumes (`aggregate.VWAP`, `TWAP`, `ComputeOHLC`,
`TotalBaseVolume`, `TotalQuoteVolume`) — is defined on the two integer
legs of `canonical.Trade`, never a stored price, so the swap re-weights
exactly with no extra step: a flipped row's weight in the requested base
IS its stored quote leg, and the only division is the final
`SetFrac(sumQuote, sumBase)`.

- **No separate re-weighting step.** Inverting each price but keeping the
  stored base as weight gives Σ(b²/q)/Σb; on the fixture that served
  1.0127659574 where the market VWAP is 0.1825.
- **VWAP is exactly reciprocal**: VWAP(A/B) × VWAP(B/A) = 1 for any window.
  TWAP is not (a time-mean of reciprocals is not the reciprocal of a
  time-mean); that is correct, not a defect.
- **Re-express before comparing.** Inversion swaps max and min, so rows
  fold first and extremes are taken after. Row-level fold is safe because
  a row has one price. The CAGG bar readers (`Store.OHLCSeries`,
  `OHLCSeriesReBucketed`, `internal/storage/timescale/aggregates.go`) fold
  per bucket with an explicit `CASE WHEN base_asset = $1 THEN high_price
  ELSE 1.0 / NULLIF(low_price, 0)` (and the mirror for low). Merely
  relabelling a bucket would serve a fully populated, wrong bar (a high of
  10 where the true high is 0.25); an unfolded read is thin but right.
- **Order-independent with scale normalisation**: multiplying both legs
  commutes with swapping them, so `AdjustPrice` runs unchanged.
- **Truncation.** Each direction is its own `LIMIT`ed arm; the union is
  re-sorted and limited again, which is exactly the market's newest
  `limit` rows (same plan as `Store.LatestTradesForPair`).
- **Where it lives.** In the store, not in callers: `TradesInRange` has no
  cursor and its five callers all want "expressed the way I asked";
  folding in callers would duplicate code across `v1.HistoryReader` and
  `orchestrator.Store`. `TradesInRangeAfter` is exempt: a cursor names a
  position in one ordering, so the paged caller merges directions itself.
- **Merge sets dedupe flips**: `distinctMarkets` drops a flip already seen,
  keeps the first spelling and honours SAC-last order; applied in
  `tipMergePairs` and `usdPeggedConstituents`.

Measured on r1 (1 h, `native/USDC-GA5Z…`): rows 2,957 → 5,751 (the unfolded
read saw 51.4% of the market); VWAP moved −0.0016%; high 0.1806 → 0.1818
(+0.65%). Plan cost: 1.72× on a populated 1 h window, 1.02× on 24 h, 1.26×
on an empty pair since 2021, 1.13× for an FX snap; the 8 s handler
ceilings are unchanged.

Pinned by `TestTradesInRange_AggregateOverBothStoredDirections`
(`trades_in_range_direction_test.go`), `TestRawTradeReadsSpanBothStoredDirections`
(`trades_direction_test.go`, query shape) and
`TestCAGGPairReadsFoldBothDirections` (`pair_direction_guard_test.go`,
parses every SQL literal so a new one-direction CAGG read fails CI).

### The alias fold

A classic market and its SAC-wrapped twin (`native/USDC-GA5Z…` vs
`<XLM SAC>/<USDC SAC>`) are different venues. Merging them is a liquidity
decision, not completeness: a thin pool beside a deep book sets the bar's
extremes. Live example (2026-06-02, `GQX-GD7TC72O…`): 660 book prints,
$140; 1 pool print, $0.60, at 13.10 vs the book's 9.54 — a +37.32% high.
Migration 0115's `usd_volume >= 0.01` floor does not stop it ($0.60 is
60× the floor).

### The fiat quote leg, per bucket

How `native/fiat:USD`-style series and points combine USD-pegged
constituents (`internal/api/v1/ohlc_fiat_combine.go`,
`usdPeggedConstituentSets`, `usdPegProxyQuotes`; `chart.go` already read
both). Shipped 2026-09-05 for both the series (launch-plan row 1.15) and
the point path (row 1.14), keeping point/series parity (C1-024).

- **Reach.** Over the year to 2026-09-05: 132 markets with a SAC-quoted
  leg, 1,916,996 prints. 24 assets were reachable already; 43 had
  SAC-only USD depth ($14,630,761.46; the largest $6,375,518.23 over
  129,925 prints) and were served nothing.
- **Established vs held-back.** Classic spellings are established; SAC
  spellings are held back. A held-back spelling is suppressed for a
  bucket an established spelling answered, and for **no other bucket** —
  absent, not down-weighted. It can never set a bar's max, min, count or
  volume beside book data. All established spellings are read before any
  held-back one (`assertSACQuotedSeriesReadLast`).
- **Per bucket, never per response.** A per-response first-hit rule makes
  the constituent set depend on the window, and would have served the
  3,356 pool-only days (671,712 prints, $175,962,608.19) as quiet. Per
  bucket, a bar renders identically in every window that contains it.
- **Point path at a stated grain.** "The point window is its own bucket"
  holds only when the window is one bucket, so the point gate runs per
  bucket at `fiatPointGateInterval` = 1m, the finest series interval.
  Point equals series exactly at 1m; at coarser intervals they are
  different questions and may differ.
- **Scale lift per bucket.** Constituents arrive at different scales
  (SDEX 7 dp, CEX 8 dp). Each bucket lifts to its own maximum scale by
  multiplying both legs by 10^(max−scale) — exact, no division; prices,
  extremes and counts are untouched, only absolute volumes move. A
  response-wide maximum made a bucket's volume depend on other days
  (10× on the book's own day). The `sources` column every `prices_*` CAGG
  has carried since migration 0002 (recreated in 0147) was simply not
  SELECTed; the same defect already understated `v_base` by 3.41% on a
  three-constituent `native/fiat:USD` bar.
- **Rejected shapes**, with plan costs: a CTE joined back on bucket
  (1.34×), a union at row grain (2.08×), `WITH ORDINALITY` + `FILTER`
  (1.09×) — refused because its failure mode is silently wrong sums on a
  money path, unprovable without a database. Shipped cost: `OHLCSeries` 1.005×, `OHLCSeriesReBucketed` 1.25×
  (1h→4h, 30 days), constituent reads 21 → 24 (`native/fiat:USD`).
- **Not done**: fiat bars still do not attribute `sources` on the wire —
  the combine always merged SDEX, four CEX and the FX pollers
  unattributed; changing that is a spec change.

Pinned by `TestFiatSeries_PoolFillsOnlyTheBucketsTheBookCannotAnswer`,
`TestFiatSeries_ABucketRendersTheSameInEveryWindow`,
`TestFiatSeries_ThinSACPoolNeverSetsABarBesideBookData`,
`TestFiatPointEqualsTheFinestSeriesExactly`,
`TestFiatPoint_PoolInAnAnsweredBucketIsSuppressed`,
`TestFiatPointMatchesSeries_AcrossVenueScales`,
`TestFiatSeries_EstablishedMixedScaleBucketIsWindowInvariant` and
`TestOHLCSeries_FiatProbeSpansWhatTheCombineReads` (the
`ohlcCoverageSet` floor spans exactly what the combine reads). Two earlier
parity pins were vacuous (one scale only; empty CAGG `sources`) and were
rebuilt with real venue names and scales.

## Cross-rates: triangulation and the router

When no direct market exists, `internal/aggregate/router.go` chains fresh
per-pair rates through hub assets (XLM, USD, BTC, …) and corroborates
across every independent path of the shortest length. Rates and inverses
are exact `*big.Rat` (`new(big.Rat).Inv(p)`, never `1.0/p`). Routes combine
by the **member median** of the highest-confidence tier, not a weighted
mean, so one divergent survivor cannot drag the result. A route's
confidence is its weakest edge (USD volume, trade count, source count,
dispersion, recency), so a dust print cannot launder itself into a
confident valuation through a hub. `min_route_confidence` ships at 0 so
serving stays permissive; `RouteTrustFloor` gates leg-substitution
reroutes and corroboration counts. Triangulated pairs
(`internal/aggregate/orchestrator/triangulate.go`) set `flags.triangulated`;
reroutes set `rerouted`, and `pivot_unverified` marks a composite leg
that was all stablecoin prints at par (a depeg there went unchecked).

## Thin markets

A market below the substance floor is withheld from `price_usd` unless the
caller asks with `include_thin=true`; the price then carries
`thin_market: true` (`internal/api/v1/envelope.go`, `assets.go`).
`/v1/price`, `/v1/price/batch` and `/v1/price/at` withhold it the same way;
`/v1/vwap` and `/v1/twap` serve such a market by default and flag it the
same way. A thin price is display-only: no valuation, total or series
derives from it.

## Configuration, observability, API

Config (`[aggregate]`, full reference in
[docs/reference/config/README.md](../reference/config/README.md)): `pairs`,
`windows` (5m, 1h, 24h), `interval_seconds`, `max_trades_per_window`,
`disable_class_filter`, `enable_stablecoin_fiat_proxy`,
`outlier_sigma_threshold`, `vwap_window_seconds` / `twap_window_seconds`
(unread, retired by GH-1129; windows come from `windows`), `min_usd_volume`, `triangulation_enabled`,
`divergence_min_interval_seconds` (300), `min_route_confidence`.

Metrics ([docs/reference/metrics/README.md](../reference/metrics/README.md)):
`stellarindex_aggregator_ticks_total{outcome}`,
`stellarindex_aggregator_vwap_writes_total`,
`stellarindex_aggregator_empty_windows_total`,
`stellarindex_aggregator_dropped_trades_total{reason,pair}`,
`stellarindex_aggregator_window_base_volume`,
`stellarindex_divergence_refresh_total{outcome}`. No other per-pair
labels: pair lenses live in Redis keys and the API, not `/metrics`.
Baseline-comparator alerts use `offset 1h`: expect noise, and suppress
them, for the first hour after a deploy.

Alerts: `deploy/monitoring/rules/aggregator.yml`, kept in lockstep with
`configs/prometheus/rules.r1/aggregator.yml` by
`scripts/ci/lint-rule-equivalence`; runbooks in
[aggregator.md](../operations/runbooks/aggregator.md). The `stellarindex_aggregator_*`
set: `silent`, `outlier_storm`, `outlier_trim_fraction`,
`outlier_volume_trim_fraction`, `fx_snap_fallback_dominant`,
`triangulation_chains_dry`, `bootstrap_cap_reengaged`, `class_drop_spike`,
`cache_write_errors`. The same file holds the rollup and sweep alerts
(`stellarindex_protocol_events_rollup_failing`,
`stellarindex_asset_volume_rollup_failing`,
`stellarindex_asset_character_rollup_failing`,
`stellarindex_nonstandard_decimals_correction_failing`,
`stellarindex_decimals_guard_sweep_stale`,
`stellarindex_customer_webhook_fanout_failing`,
`stellarindex_change_summary_stale`).

Divergence (`internal/aggregate/orchestrator/divergence_refresh.go`): per
tick, `divergence.Service` checks every enabled reference (CoinGecko, Chainlink-HTTP,
Reflector, RedStone, Band, synthetic USD-cross; see
[oracle-manipulation-defense.md](oracle-manipulation-defense.md)), writes
`div:<pair>` (`internal/cachekeys/keys.go`) with a TTL of
max(5m, refresh interval + worst pass + 1m) (`internal/divergence/worker.go`), and
`/v1/price` reads it into `flags.divergence_warning`. Outcomes `ok`,
`no_vwap`, `parse_error`, `refresh_error`; a sustained `refresh_error`
fires `stellarindex_divergence_refresh_error_dominant`.

API: `/v1/price` (Redis or guarded `prices_1m`, then a last-trade
fallback), `/v1/price/tip`, `/v1/vwap` and `/v1/twap` (on-query over
`TradesInRange`, `to` clamped to the last closed minute), `/v1/ohlc`,
`/v1/sources` (the registry), `/v1/markets` (`DistinctPairs`).

## Boundaries

- **No persisted VWAP, but not stateless.** VWAP lives in Redis with a
  TTL; if Redis is lost the next tick rebuilds it from trades. Timescale
  holds only audit mirrors — `price_source_contributions`
  (`ContributionSink`), `freeze_events` (`FreezeWriter`),
  `divergence_observations` (`DivergenceSink`) — and no served price is
  read from them. The orchestrator carries load-bearing cross-tick memory
  (`prevVWAPs`, `frozenPrevVWAPs`, `freezeStates`, `lastComposites`,
  `tickEdgeQuotes`; documented in `orchestrator.go`). A restart loses all
  but `freezeStates` (read back from `freeze_events`), and a frozen pair
  with no `prev` is unscored (`phase2:unscored`): it can neither fire nor
  release. Restarts are safe, not free.
- **No cross-binary coupling.** Aggregator → API is Redis keys plus the
  static registry; the API cannot read the orchestrator's `Stats()`.

## Open and deferred

| Item | Why deferred | Ref |
|---|---|---|
| Pre-compute TWAP in the orchestrator | `/v1/twap` reads trades on each request; VWAP dominates traffic and pre-computing costs Redis without a demand signal | INV-1067 (discarded) |
| Explicit CAGG refresh driver | Timescale's background job suffices until consumers need historical CAGGs at fresh-data SLAs | — |
| Per-source weighted VWAP | every contributing source weighs 100; `Metadata.DefaultWeight` is shaped for config overrides, and the `aggregate.VWAP` math change lands when an operator needs it | — |
| Every read through the closed-bucket guard | — | #689 |
| `sources` on the wire for fiat bars | spec change | — |

## References

ADR-0003 (money), ADR-0015 (closed-bucket serving), ADR-0018 (tip),
ADR-0019 (freeze/confidence); [docs/methodology/twap-ohlc.md](../methodology/twap-ohlc.md);
[docs/operations/v1-launch-plan.md](../operations/v1-launch-plan.md) rows
1.14–1.16; `internal/aggregate/doc.go`.
