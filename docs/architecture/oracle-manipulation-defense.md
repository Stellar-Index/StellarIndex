---
title: Oracles — manipulation defense, SEP-40 and onboarding a generic oracle
last_verified: 2026-10-05
status: living document — thresholds, metric and alert names checked against code 2026-10-05; the attack catalogue is not re-derived
---

# Oracles: manipulation defense, SEP-40, onboarding

Three questions: how a manipulated venue price is kept out of what we
serve; how SEP-40 oracles are read and served; how a new oracle gets
found and onboarded. The aggregation chain these defenses sit in is
[aggregation-plan.md](aggregation-plan.md); the confidence score and
freeze ladder are [below](#freeze-layer-9) (policy: ADR-0019);
ingest is [ingest-pipeline.md](ingest-pipeline.md); the oracle decoder
traps (Reflector's three contracts, Band at E18 with zero events,
RedStone's feed attribution) are in [domain-traps.md](domain-traps.md).

## Oracle ingest rules

- Oracle outputs are **record-layer only**. Reflector, Band, RedStone,
  CoinGecko and CMC never carry VWAP weight; they are `ClassOracle` /
  aggregator class in `internal/sources/external/registry.go` and appear
  in `sources`, not in the price.
- **Never drop an unmapped symbol.** Record it verbatim as `raw:<symbol>`
  (`canonical.AssetOracleRaw`, `internal/canonical/asset_raw.go`). A raw
  row never reaches VWAP, divergence, a pair leg or a supply key: filter
  on `Asset.IsMapped()` or `asset NOT LIKE 'raw:%'`.
- **Gate on contract identity**, never topic alone (ADR-0035/0040,
  `internal/contractid`), and gate every backfill on a per-WASM-hash
  decoder audit.
- Prices stay full `i128` end to end (ADR-0003).

## Attack pattern

1. Identify a low-liquidity asset whose price feeds a downstream
   protocol's collateral, liquidation or borrowing logic.
2. Take a position in that protocol (a borrow against the asset, or a
   bet that profits from a dislocation).
3. Manipulate the price on a thin venue (small DEX pool, single-source
   CEX, AMM sandwich), often with a flash loan.
4. The oracle reads the manipulated price and pushes it downstream.
5. Withdraw value against the inflated valuation before arbitrage
   restores the fair price; the protocol absorbs the loss.

The exploitable surface is the gap between the oracle's price and the
fair market price; each layer below closes part of it.

### Known incidents

Reflector/USTRY (Stellar, 2026); Mango Markets (Solana,
Oct 2022, ~$117M); Cream Finance (Ethereum, Oct 2021, ~$130M); Inverse
Finance (Ethereum, Apr 2022, ~$15M); Polter Finance (Fantom, Nov 2024,
~$12M); Harvest Finance (Ethereum, Oct 2020, ~$24M); bZx (Ethereum,
Feb 2020, ~$1M across several incidents).

## Defensive layers

| # | Layer | Status | Mechanism |
|---|---|---|---|
| 1 | Multi-source consensus | shipped | VWAP across every `ClassExchange` venue; one venue is diluted by the rest. With one contributing venue it degrades to trusting that venue — the USTRY case |
| 2 | Source-class exclusion | shipped | only `ClassExchange` (verified CEX + DEX trades) weighs; oracle and aggregator classes are excluded so an oracle attack cannot move our price, only lower confidence through the cross-oracle factor (Layer 5) |
| 3 | Liquidity floor per source per bucket | **not shipped** | weight is volume-proportional with no absolute floor; ADR-0019's `liquidity_factor` partly covers it. Proposed: `aggregate.min_pool_tvl_usd` (~$10K), `aggregate.min_per_bucket_volume_usd` (~$1K) |
| 4 | Outlier trimming | shipped, **active** | median + 1.4826·MAD at `outlier_sigma_threshold` 4, plus the time-local test (`outliers_local.go`) and `keepIfVolumeMajority`; trades are dropped before VWAP, not just alerted on |
| 5 | Cross-reference divergence | shipped | every `divergence_min_interval_seconds` (300) against the references below; see below |
| 6 | Closed-bucket serving | shipped | ADR-0015; a single-block spike is diluted across the 1-minute bucket |
| 7 | TWAP alongside VWAP | shipped | `/v1/twap` lets a consumer use a manipulation-resistant mean |
| 8 | Decoder + WASM-version audit gating | shipped | `Backfill: BackfillPerWASM` in `registry.go`; a new or upgraded DEX contract is caught at audit time, not after an exploit |
| 9 | Per-asset confidence + freeze | shipped (ADR-0019 Phases 1–2) | `internal/aggregate/anomaly/`, `baseline/`, `confidence/` |

Layer 5 references, each behind its `[divergence.*]` switch
(`internal/config/config.go`, `cmd/stellarindex-aggregator/main.go`):
CoinGecko (`coingecko`, default on), Chainlink-HTTP (`chainlink`, default
off, needs a feed map), the on-chain oracles read from our ingested
`oracle_updates` rows — Reflector dex/cex/fx (`reflector`), RedStone
(`redstone`), Band (`band`), all default on — and a synthetic USD-cross
reference derived from whichever of those are enabled. The CMC poller is
not wired. Oracles are references only: a compromised oracle cannot move
our VWAP (Layer 2) but can lower confidence through the cross-oracle
factor, and with two or more references disagreeing, set the warning. The
worker writes `div:<pair>` (`internal/cachekeys/keys.go`, 5-minute TTL); `/v1/price`
surfaces `flags.divergence_warning`. The bounded gauge
`stellarindex_divergence_max_abs_fraction` (worst |ours−ref|/ref per
reference, no asset label) feeds `stellarindex_price_divergence_warning`
(> 0.05 for 10m) and `stellarindex_price_divergence_critical` (> 0.10 for
10m) in `deploy/monitoring/rules/divergence.yml`; per-pair detail stays in
`divergence_observations`.

### Freeze (Layer 9)

Applies to `/v1/price` only. `/v1/price/tip` and `/v1/observations` show
anomalies as they happen; their consistency contracts allow it. A frozen
price serves the last known-good value, never the manipulated one.
Thresholds (`internal/config/config.go`):

| Key | Default | Meaning |
|---|---|---|
| `confidence_max_freeze` | 0.45 | freeze when confidence < this. ADR-0019 said 0.10, which needed z ≈ 15 and never fired |
| `z_score_min_freeze` | 5.0 | freeze when z > this |
| `source_count_max_freeze` | 1 | freeze when sources ≤ this |
| `extension_minutes` | 30 | hold extension at each expiry not yet earned |
| `unfreeze_confidence_min` | 0.30 | auto-unfreeze needs confidence > this |
| `unfreeze_z_score_max` | 3.0 | … and z < this |
| `unfreeze_buckets` | 2 | … for this many consecutive buckets |

Baselines are a rolling 30 days. The Phase 3 cross-oracle factor is
wired (`CrossOracleDivergencePct` in `orchestrator/confidence.go`);
production-quality divergence coverage for it is post-launch work.

#### Baseline and z-score

Per `(base, quote)` pair, over 1-minute buckets: `return_median`, `return_mad`
(MAD scaled by 1.4826; MAD rather than sigma because sigma inflates after the first
manipulation in the window and hides the next), `source_count_p50`, `liquidity_p50_usd`.
`z_score = abs(return_pct - return_median) / return_mad`. `MultiBaseline.MaxZScore`
(`internal/aggregate/baseline/multi.go`) takes the largest z across the 1d, 7d and 30d
windows. It sees spikes, not slow drift; `MaxDriftZScore` covers drift, latches about 30
days, and so stays out of the fire, extend and release decisions.

#### Confidence score

`confidence in [0, 1]` is a normalised weighted geometric mean,
`prod(factor_i ^ weight_i) ^ (1 / sum(weights))` (`internal/aggregate/confidence.Compute`):

```
confidence = (
  z_score_factor(z_score)                    ^ w_z       *
  source_count_factor(n_sources)             ^ w_src     *
  diversity_factor(class_count)              ^ w_div     *
  liquidity_factor(bucket_volume)            ^ w_liq     *
  cross_oracle_factor(divergence_pct)        ^ w_xoracle *
  triangulation_agreement_factor(divergence) ^ w_tri     *
  baseline_quality_factor(days_history)      ^ w_qual
) ^ (1 / (w_z + w_src + w_div + w_liq + w_xoracle + w_tri + w_qual))
```

Weights are `[anomaly.weights]`, default 1.0 except `w_tri` (0.5). Factor shapes (`confidence/factors.go`):

- `z_score_factor`: 1.0 at z=0, sigmoid decay to ~0 at z=10.
- `source_count_factor`: logistic with inflection at n=3; `SourceCountFactor(1)` ≈ 0.119.
- `diversity_factor`: 0.5 for one source class, 1.0 for two or more.
- `liquidity_factor`: log-saturating between $1K and a **$1,000,000** ceiling, because
  BTC/USD's 5m bucket p50 is $123,678 and a lower ceiling saturated on the median bucket.
  Volume that cannot be valued in USD reads `LiquidityUnmeasuredFactor` = 0.5.
- `cross_oracle_factor`: 1.0 within 1% of the cross-oracle median, decaying; 0.7 with no data.
- `triangulation_agreement_factor`: the same shape against the composite a configured chain
  implies (1.0 within 2%); default weight 0.5, since a composite reuses our own legs.
  **Weight 0 when unchecked**, so an un-triangulated pair scores exactly as before. A composite
  never feeds `source_count`: counting it as a venue would disarm the `source_count <= 1` leg.
- `baseline_quality_factor`: 0.5 with no baseline, ramping to 1.0 over 30 days.

Calibration: ADR-0019's original `confidence < 0.10` needed z ≈ 15 for a single-source $12K
bucket and never fired; at 0.45 the confidence leg crosses at z ≈ 4.8–5.9, so the independent
`z > 5.0` leg decides. `TestPhase2FreezeFires_CalibratedToADRZBand` pins that band both ways.
Wire: `confidence` plus `confidence_factors` (the seven factor values, the `*_checked` evidence
flags, `liquidity_measured`, and `baseline_age_days` and `bootstrap_capped`.) A chained pair's
confidence is its weakest leg's (`RouteConfidence`).

#### Bootstrap gate

A pair with no usable baseline publishes no `confidence` and is not Phase 2 eligible; the Phase 1
class thresholds (`[anomaly.thresholds]` `warn_pct` / `freeze_pct`: stablecoin and treasury 1/3,
crypto 20/50, governance 50/100, default 30/75) can still freeze it. With a baseline, confidence
is capped at 0.5 until baseline density (1-minute buckets / 1440) clears the gate: the cap releases
at `BootstrapDensityDays` = 28.5 and re-engages only below `BootstrapReengageDensityDays` = 27.
Gate state is in aggregator memory, so a restart re-applies the 28.5 gate.

#### Freeze lifecycle

`internal/aggregate/freeze.Policy`; durations are `[anomaly.phase2]` tunables.

- Initial hold 30 minutes, or 10 for a pair with no corroborating lens ("corroborated" means a
  second lens produced a reading this bucket, not that it agreed).
- At each expiry, if the condition still holds, extend 30 minutes, up to 4 times. After 2 hours
  the freeze escalates (P1, `stellarindex_anomaly_freeze_escalated`) and never auto-unfreezes.
- Auto-unfreeze needs the table's thresholds for two consecutive buckets AND `release_corroborated`:
  a lens reading that agrees within 5% with the fresh candidate (calm alone cannot tell "repriced"
  from "manipulation parked"). A pair with no lens rides the ladder to a human.
- Composite reference (`[aggregate.composite_reference]`): a same-bucket composite agreeing within
  `tolerance_bps` (75) suppresses the fire; agreeing within `release_band_pct` (2%) releases. It
  never counts as a second source.
- Durability: Redis is a cache. `freeze_events` (migrations 0119, 0163 `window_ladders`) is the
  record; a missing marker is rehydrated from an open row, bounded by `hold_until` + 5 minutes grace.
  The aggregator runs one ladder per (pair, window) for 5m, 1h and 24h; the last window to release
  deletes the marker. The override is `stellarindex-ops freeze-unfreeze -reason ...`; a bare
  `redis-cli DEL` is not one, because the next tick rehydrates.

### Engineering observability

- `stellarindex_anomaly_freeze_engaged_total` (counter) and
  `stellarindex_anomaly_freeze_active` (gauge) track freezes; runbook
  [anomaly.md](../operations/runbooks/anomaly.md).
- `stellarindex_aggregator_dropped_trades_total` and
  `stellarindex_aggregator_window_base_volume` show trimming;
  `stellarindex_aggregator_outlier_storm`,
  `stellarindex_aggregator_outlier_trim_fraction` (24h) and
  `stellarindex_aggregator_outlier_volume_trim_fraction` (per window) alert on it
  ([runbook](../operations/runbooks/aggregator.md#stellarindex_aggregator_outlier_storm)).
  Divergence alerts: [divergence.md](../operations/runbooks/divergence.md).
- `stellarindex_anomaly_z_score` and `stellarindex_anomaly_confidence` do not exist in `internal/obs/metrics.go`; read z-score and confidence from the `/v1/price` response (`confidence_factors`) instead.

### Worked example: USTRY

A thin RWA token: $1.00, ~$50K daily volume, ~$2K per bucket,
`return_mad` 0.05%, confidence 0.20 before the attack. An attacker
prints on its Aquarius pool:

| Time | Bucket VWAP | z | Confidence |
|---|---|---|---|
| T+0 | $5.00 | 80σ | 0.04 |
| T+1m | $20.00 | 380σ | 0.03 |
| T+3m | $100.00 | 1980σ | 0.02 |
| T+5m | $50.00 | 980σ | — |

`/v1/price` freezes on the first closed bucket (z > 5, confidence < 0.45)
and serves the last good $1.00 with `flags.frozen`, `stale` and
`single_source` set and `observed_at` held at the last good bucket;
`/v1/price/tip` and `/v1/observations` show the spike as it happens.
Auto-release needs two consecutive buckets with z < 3.0 **and**
confidence > 0.30, so a single-source asset sitting at 0.20 does not
auto-release until its confidence recovers (for example, a second
source). Release also needs a corroborating lens that agrees with the
candidate level (`release_corroborated`, `internal/aggregate/freeze/lifecycle.go`):
a held manipulation is calm too ([freeze lifecycle](#freeze-lifecycle)). Reflector's only observed venue was the manipulated pool, so it
published the spike; its value carries no weight here (Layer 2).

## Open gaps

| Gap | Status | Ref |
|---|---|---|
| Liquidity floor per source per bucket (Layer 3) | not built | — |
| Auto-exclude the offending source on an outlier storm | not built; trimming is per print, not per source, so exclusion is a manual runbook step | — |
| Stablecoin-depeg auto-gating (a depegged stablecoin used as collateral) | not built; manual policy through the class system | INV-1122 |
| Adversarial-testing exercises (below) | recommended, not scheduled | INV-1123 |
| Production-quality divergence coverage for the Phase 3 factor | wired; coverage tuning post-launch | — |
| SEP-50 (NFT) decoder | none exists; out of current scope | INV-1095 |

Adversarial exercises (INV-1123), each with its pass condition: a thin-pool spike (outlier storm fires within one bucket, VWAP barely moves); a single-source compromise (the sigma filter excludes it, the runbook disables it); a coordinated multi-source attack (divergence fires, response flagged not silent); an external-oracle compromise (our VWAP unchanged, confidence may drop via the cross-oracle factor).

## SEP-40: what we serve and why there is no generic reader

SEP-40 is a **read interface** (`lastprice`, `price`, `prices`, `assets`,
`base`, `decimals`, optional `resolution`, `x_*` cross-pair methods), not
an event schema. Only `lastprice`, `prices`, `assets` and `decimals` are
safe to assume: Reflector v3 claims SEP-40 yet has no `twap` or `x_*`.

- **Serve side: done.** `/v1/oracle/lastprice`, `/v1/oracle/prices` (≤200
  records), `/v1/oracle/x_last_price`, `/v1/oracle/latest`
  (`internal/api/v1/oracle_sep40.go`) answer in SEP-40 shape over our
  VWAP/TWAP.
- **Ingest side: native per source.** `reflector` (`("REFLECTOR","update")`
  events; DEX/CEX/FX contracts), `redstone` (`"REDSTONE"` batch events),
  `band` (`ContractCallDecoder` on `relay()`/`force_relay()`, because Band
  emits no events). All sink `canonical.OracleUpdate` through
  `persistOracle`; a new oracle reuses that sink, not a new hypertable.
- **Generic state-read adapter: deferred** (ADR-0045). Reasons: no
  concrete target to design the storage-key layout and state-read
  completeness against; state-read completeness is a different claim
  from event coverage (ADR-0033); and guessing a layout repeats the
  DeFindex tag-1.0.0-vs-mainnet mistake. If built, it lives in
  `internal/sources/sep40/`, rides the ADR-0039 contract state reader
  (lake-native, never stellar-rpc), is gated by a `contractid.Registry`
  allow-list (fail-closed), triggers on `LedgerEntryChangeDecoder` rather
  than polling (a periodic `stellarindex-ops` snapshot only as fallback),
  registers with `IsProjectedEvent` / `notSunkEvents` like any source,
and sits behind a `[sources.sep40]` config gate whose recognition gap
stays fail-closed until the allow-list is seeded.

## Generic oracle discovery

### SEP inventory

Of 59 SEPs (17 Active, 7 Final, 26 Draft, 3 Abandoned at 2026-07-10)
only SEP-40 (oracle) and SEP-41 (token) are load-bearing for generic
interpretation. SEP-50 (NFT) and SEP-56 (vault) are the next analogues, but
their `Deposit`/`Withdraw`-style topics collide with Blend and DeFindex.
SEP-45 and SEP-57 are niche or unconfirmed on mainnet. SEP-49 (upgradeable
contracts) would need `ContractEventTypeSystem` events, which
`internal/dispatcher/census.go` (`captureEligible`) does not capture.

SEP-46/47/48 self-declared metadata (`sep` Wasm-meta entry,
`contractspecv0`) is **not a trust source**: a declaration does not prove
implementation, and method names have twice diverged from deployed
behaviour (Reflector v3; DeFindex). It is an operator-triage hint only.

### Lake census

A one-off scan of r1's `stellar.contract_events` (12.4B rows, ledgers
2 to 63,407,342, 2026-07-10) on the decoded `topic_0_sym` column
(`DistinctTopicShapes`, `internal/storage/clickhouse/recognition.go`),
filtered to these `topic_0_sym` values: price, prices, lastprice, last_price, x_last_price, set_price, update_price, price_update, new_price, oracle, Oracle, ORACLE, feed, PriceData, resolution, write_prices, relay, force_relay, REFLECTOR, REDSTONE, rate, rates, set_rate, symbol_rates, StandardReference, update, base, decimals, assets,
with the ingested Reflector, RedStone and Band contracts excluded. All six
candidate patterns were false positives or dormant test deployments:

| Contract / pattern | Events | Verdict |
|---|---|---|
| `CCWKKEQTMGBNLHDKSYWFOA4IFFR2GT6FRYSHIXQQGNVB64AQHCFXLL4S` (`update`: `doc_id`, `ipfs_cid`) | 2,237 | false positive: beef traceability |
| `CAHDGXF64LG4PA45PPCDFQYRYWH3X33G7JJFGJTKMFMQIFUKNBONLGHD` (`update`, supply-chain IDs) | 250 | false positive |
| `CDFMV3EI2FEGKHQYZXFSKPBEXO2MXKRMQRXK5SM4DVWWHMGEJTH6JVK2` (string `price`) | 151 | dormant test oracle |
| 11 RedStone-Adapter-shaped contracts | 27 | tests |
| 2 rational-price (`price_num`/`price_den`) and 2 developer oracles | 2-3 each | tests |

 No sustained un-ingested SEP-40 oracle existed,
which reaffirmed the ADR-0045 deferral. Scope limit: the census only sees
event-emitting contracts. `stellar.operations.body_xdr` has no plaintext
function-name column, so a Band-alike that emits nothing is invisible to
SQL.

### Discovery pipeline (shipped 2026-07-10)

Option (b) of the three considered ((a) and (c) are under Rejected
options): `internal/canonical/discovery` (see its `doc.go`) extends the
SEP-41 sniffer. Both halves are **sighting-only**: they write `discovered_assets`
and never decode, attribute or emit `canonical.OracleUpdate`
(ADR-0035). An operator triages each sighting (`stellarindex-ops discovery`) and a confirmed oracle follows
[add-onchain-source.md](../contributing/add-onchain-source.md): contract
identity gating, every-event completeness, a per-WASM audit before
backfill is enabled.

#### Event-shaped discovery

`SniffOracleEvent`, hooked in `dispatchOne` (`internal/dispatcher/dispatcher.go`),
records `(contract_id, topic_0_sym, ledger)` for the census symbol set
above, so the census is continuous. A new oracle with a SEP-40/RedStone/Band-like
event shape is sighted although no decoder knows its contract id.

#### Event-less discovery

`SniffOracleCall` is the symmetric hook on the `ContractCallContext` path
(the seam Band's `ContractCallDecoder` uses). It matches function names
against `lastprice`, `price`, `prices`, `relay`, `force_relay`,
`write_prices`, `x_last_price`, so a future event-less oracle under
another contract id is still sighted.

### Rejected options

- **(a) Generic SEP-40 poll/read adapter now** — the ADR-0045 shape; no
  target to design against (see above).
- **(c) Generic decode-by-interface** — decoding anything that claims an
  interface repeats the Reflector-v3 and DeFindex failures mechanically,
  at scale, and breaks contract-identity gating.

When a new incident becomes public: add it under Known incidents, add any
new gap to Open gaps, bump `last_verified`, and cross-reference it from
each affected runbook.

## References

ADR-0010, ADR-0015, ADR-0018, ADR-0019, ADR-0033, ADR-0035, ADR-0039, ADR-0040, ADR-0045;
SEP specs at github.com/stellar/stellar-protocol/tree/master/ecosystem;
[docs/operations/wasm-audits/defindex.md](../operations/wasm-audits/defindex.md).
