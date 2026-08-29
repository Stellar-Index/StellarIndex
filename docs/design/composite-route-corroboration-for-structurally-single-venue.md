# Composite-route corroboration for structurally single-venue pairs: route table, disjointness verdicts, config, and honest fallback (2026-08-28)

> Status: DESIGN (Wave B, 2026-08-28). Produced by a read-only investigation against HEAD 4aa93e96; every claim cites file:line. Open questions at the end are decisions for the operator.

# Composite-route corroboration for structurally single-venue pairs

Status: design (read-only investigation, 2026-08-28). Repo at a33d061f. Author: cold audit pass for D1-REOPENED.

## 1. Problem

`Orchestrator.effectiveSourceCount` (internal/aggregate/orchestrator/triangulate_corroborate.go) MAXes the graph router's `corroborationCount` into the anomaly-freeze `source_count` leg. In production the router finds exactly ONE shortest route per triangulation target, so `corroborationCount` is always 1 and the widening never executes. The freeze keeps firing on thin fiat crosses whose served price is correct.

## 2. What the code actually does (traced)

- Edge graph per window (`buildWindowEdges`): this tick's confidently-published VWAPs (`tickEdgeQuotes`) plus each chain's resolved legs. Fiat/fiat legs resolve via `FXQuoteAtOrBefore` → `fxQuotesSnapAtOrBefore` (fx_quotes table, `rate_usd` per ticker).
- `CombineRoutes` calls `FindRoutes(..., shortestOnly=true)` after `excludeDirectEdge` removes the target's own market. Only the minimum-hop routes survive.
- `corroboratingRouteCount`: 0 if diverged; n if n ≤ 1; else the maximum clique of routes that (a) each have weakest-link confidence ≥ `corroborationMinConfidence` (0.5), (b) pairwise agree within `routerCorroborationAgreePct` (3 %, measured against the smaller price), (c) are pairwise edge-disjoint by UNDIRECTED asset-pair key.
- The sample is recorded ONLY on a confident publish (`publishComposite` → `recordComposite`), read one tick later, and expires after `compositeMaxAgeTicks` = 2.
- Aggregated pair set on r1 = the binary default (`cmd/stellarindex-aggregator/main.go defaultPairs`): {crypto:XLM, native, crypto:BTC, crypto:ETH} × {fiat:USD, fiat:EUR, fiat:GBP}. The template sets no `aggregate.pairs`.
- Chains (template): XLM/EUR ← [XLM/USD, USD/EUR]; XLM/GBP, ETH/GBP, BTC/GBP ← [X/USD, USD/GBP].

Graph nodes: XLM, native, BTC, ETH, USD, EUR, GBP. Edges: 12 crypto/fiat markets + USD/EUR + USD/GBP. There is no crypto/crypto edge and no non-USD fiat/fiat edge. Every target therefore has exactly one 2-hop route (through USD); all 3-hop alternatives are pruned as non-shortest.

## 3. Venue pair inventory (enabled sources)

| Venue | XLM | BTC | ETH | crypto/crypto |
|---|---|---|---|---|
| kraken | USD EUR GBP AUD CAD CHF | USD EUR GBP | USD EUR GBP | none |
| coinbase | USD EUR | USD EUR GBP | USD EUR GBP | none |
| bitstamp | USD EUR GBP **BTC** | USD EUR GBP | USD EUR GBP | XLM/BTC |
| binance | USDT EUR **BTC** | USDT EUR GBP | USDT EUR GBP | XLM/BTC |

XLM/GBP is quoted by kraken + bitstamp only. ETH/BTC and XLM/ETH are listed by no enabled source. FX rows (fx_quotes) come from `massive` (SubclassFX); ECB is not an FX-snap source.

## 4. Why `fiat:EUR/fiat:GBP` is NOT edge-disjoint from the USD pivot

`fxSnapFromRows` (internal/storage/timescale/fx_quotes.go) computes any fiat pair as `rate_usd(Quote) / rate_usd(Base)` with USD contributing an exact 1. So

- USD/GBP = rate_usd(GBP)
- EUR/GBP = rate_usd(GBP) / rate_usd(EUR)

Both read the same fx_quotes GBP row (same bucket, same provider). A stale, wrong or manipulated GBP row moves both routes identically. The router keys edges by asset pair, so it would count XLM→USD→GBP and XLM→EUR→GBP as disjoint and — since their FX factors are algebraically identical — as tightly agreeing. `corroborationCount` would read 2 on one data row. That is precisely the fake independence the count exists to refuse. **No chain may use a non-USD fiat/fiat leg.**

## 5. Route table

Legend: R = real (edge-disjoint and independently sourced); N = nominally disjoint, not independent; X = absent from the graph today.

### XLM/GBP
| Route | Edges | Today | Verdict |
|---|---|---|---|
| XLM→USD→GBP | XLM/USD, USD/GBP(fx GBP row) | present | baseline |
| XLM→EUR→GBP | XLM/EUR, EUR/GBP(fx GBP+EUR rows) | X | **N — reject** |
| XLM→BTC→GBP | XLM/BTC (bitstamp, binance), BTC/GBP (4 venues) | X | **R** once crypto:XLM/crypto:BTC is aggregated |
| XLM→ETH→GBP | XLM/ETH | X (no venue) | unavailable |

### XLM/EUR
| XLM→USD→EUR | present | baseline |
| XLM→GBP→EUR | X | N — reject |
| XLM→BTC→EUR | XLM/BTC, BTC/EUR (4 venues) | X | **R** once XLM/BTC is aggregated |
Note: after #203 (binance XLMEUR + coinbase XLM-EUR) XLM/EUR has 4 direct venues; its source_count leg should already clear 1 without corroboration.

### ETH/GBP
| ETH→USD→GBP | present | baseline |
| ETH→EUR→GBP | X | N — reject |
| ETH→BTC→GBP | ETH/BTC | X — ETH/BTC in no pairs list | R only after a code change to all four venue lists |
**No real second route by configuration.**

### BTC/GBP
| BTC→USD→GBP | present | baseline |
| BTC→EUR→GBP | X | N — reject |
| BTC→XLM→GBP | XLM/BTC, XLM/GBP | X | edge-disjoint but XLM/GBP is the single-venue pair under suspicion; edge confidence ≈ 0.47–0.52 → **nominal** |
| BTC→ETH→GBP | ETH/BTC, ETH/GBP | X | R only after ETH/BTC code change |
**No real second route by configuration.**

## 6. The confidence floor makes even the real route nominal for 30 days

`corroborationMinConfidence` = 0.5 (router.go). `confidence.BootstrapConfidenceCap` = 0.5 for the first `BootstrapDays` = 30 days of an asset's baseline; the unscored fallback is `min(SourceCountFactor(n), 0.5)` = 0.269 at n=2. Route confidence is the minimum over legs, so XLM→BTC→GBP cannot clear 0.5 until XLM/BTC has a 30-day baseline, and afterwards only marginally (the orchestrator's own measurements put a mature sparse pair at 0.477–0.524). Expect `corroborationCount` = 1 for ≥30 days after apply and intermittent 2 afterwards. `path_count` = 2 will appear immediately and must not be read as corroboration (the 2026-08-27 template CORRECTION records exactly that mistake).

## 7. Two design gaps

1. **Corroboration cannot help release a freeze.** `routeTarget` returns `frozen_leg` without `recordComposite` whenever `frozenLeg(target)` is set — and `markFrozenThisTick` fires on every refused tick of a hold. The sample ages out in two ticks, `effectiveSourceCount` falls back to 1, and `releaseCorroborated`'s triangulation lens is (by its own comment) dead mid-freeze. Release depends solely on the synthetic-usd-cross oracle lens.
2. **`corroborationCount` is unobservable.** No metric, no log line, not in the `composite_meta` JSON; `/v1/price` exposes only `flags.triangulated`. The only trace is `sources=N` inside the `freeze engaged` log / `freeze_events.detail->>'reason'`, visible only when a freeze fires.

## 8. Recommended change

### 8.1 Template — `configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`, `[aggregate]`

```toml
# Coverage set (2026-08-28, D1 route-corroboration). `pairs` REPLACES
# the binary default, so all 12 defaults are repeated. crypto:XLM/crypto:BTC
# (bitstamp xlmbtc + binance XLMBTC) gives the router an EDGE-DISJOINT
# second route to XLM/GBP and XLM/EUR (XLM→BTC→GBP / XLM→BTC→EUR). It is
# not min_usd_volume-gated (crypto quote); the corroboration floor (0.5)
# is its only guard and its 30-day bootstrap cap (0.5) means
# corroborationCount stays 1 for ~30 days after apply. Never add a
# fiat:EUR/fiat:GBP style leg: fx snaps derive it from the same USD rows.
pairs = [
  \"crypto:XLM/fiat:USD\", \"crypto:XLM/fiat:EUR\", \"crypto:XLM/fiat:GBP\",
  \"native/fiat:USD\",     \"native/fiat:EUR\",     \"native/fiat:GBP\",
  \"crypto:BTC/fiat:USD\", \"crypto:BTC/fiat:EUR\", \"crypto:BTC/fiat:GBP\",
  \"crypto:ETH/fiat:USD\", \"crypto:ETH/fiat:EUR\", \"crypto:ETH/fiat:GBP\",
  \"crypto:XLM/crypto:BTC\",
]
```

`[[aggregate.triangulations]]` unchanged. Inventory unchanged (aggregate config is region-invariant). Ships ONLY via an ansible config apply + aggregator restart (2026-08-25 deploy-gap class), followed by the ansible-drift workflow.

### 8.2 Venue pairs needed for REAL corroboration
- XLM/GBP, XLM/EUR: none required. Optional: Kraken XLM/XBT and Coinbase XLM-BTC (code) to lift XLM/BTC to 3–4 venues.
- ETH/GBP, BTC/GBP: ETH/BTC in kraken/pairs.go, coinbase/pairs.go, bitstamp/pairs.go, binance/pairs.yaml + `aggregate.pairs`; then 30-day maturity. Until then: fallback posture (§10).

### 8.3 Code follow-ups (separate small PRs)
1. Observability: `corroboration_count` in `compositeMeta`; gauge `stellarindex_aggregator_route_corroboration{pair,window}`; Info log `triangulation: composite published` with `path_count`, `corroboration_count`, `combined_confidence`, `diverged`.
2. Mid-freeze corroboration: call `recordComposite` in the frozen-target branch of `routeTarget` (the target's own edge is already excluded from the graph) so the widening survives a hold; re-verify the `releaseCorroborated` triangulation-lens property when doing so.
3. Guard: reject triangulation legs where Base and Quote are both fiat and neither is USD (or key router edges by fx provenance).

## 9. Validation plan on r1

Prerequisites: `stellarindex_external_fx_last_quote_unix{source=\"massive\"}` fresh; `select ticker,max(bucket) from fx_quotes where ticker in ('EUR','GBP') group by 1`; `stellarindex_aggregator_triangulations_total{outcome=\"ok\"}` rising; `select count(distinct source) from trades where base_asset='crypto:XLM' and quote_asset='crypto:BTC' and ts>now()-interval '5 min'` ≥ 2.

With the observability patch: `curl -s localhost:9091/metrics | grep 'route_corroboration{pair=\"crypto:XLM/fiat:GBP\"'` → 2 (after maturity); `journalctl -u stellarindex-aggregator -o cat | grep 'composite published' | grep 'crypto:XLM/fiat:GBP'`.

Without it (today's binary): `redis-cli GET vwap:crypto:XLM:crypto:BTC:300` non-nil; `redis-cli GET 'vwap:crypto:XLM:fiat:GBP:300:composite_meta'` → `path_count: 2` (survivors, not corroboration); `journalctl -u stellarindex-aggregator | grep 'freeze engaged' | grep 'crypto:XLM/fiat:GBP'` → `sources=N` in reason (N=2 only if widening reached the freeze); `psql -c \"select fired_at, detail->>'reason' from freeze_events where base_asset='crypto:XLM' and quote_asset='fiat:GBP' order by fired_at desc limit 20\"` for rate before/after.

## 10. Honest fallback — the posture to sign

For ETH/GBP and BTC/GBP (no real disjoint route by configuration) and for XLM/GBP during the ≥30-day XLM/BTC bootstrap: **freeze-and-auto-release**. Phase 2 fires on `confidence<0.45 AND z>5 AND source_count≤1`; hold 30 min (10 min uncorroborated); auto-release after 2 consecutive calm buckets whose fresh candidate agrees within 5 % with the synthetic-usd-cross reference median (verified live 2026-08-25, `success_count=2`); ladder ×4 then operator escalation. This is bounded, real protection with a served-value pin of ≤30–40 min on a genuine move. What it must never do is claim `source_count>1` through a USD-derived FX cross. Pager: page on ladder escalation, not on engage, for the structurally-single-venue list.

### 10.1 Amendment (2026-08-29) — a composite reference may CORROBORATE or REFUTE the verdict

Product decision (Ash, 2026-08-29), quoted: *"§10's 'an FX cross must never count as a second source' is too absolute. XLM/USD (multi-venue, deep) × USD/GBP (massive.com FX, very deep) is an excellent REFERENCE for XLM/GBP fair value."* And the spec amendment of the same day: *"the composite reference is only as strong as its weakest leg"* — the target/USD leg must itself be corroborated by ≥ N real exchange venues on the CURRENT bucket, and the FX leg must be fresh within its staleness budget and come from the fx source class (massive), never an oracle.

What changes, and what does not:

- **Unchanged (the §10 invariant, now enforced by test):** the composite NEVER contributes to VWAP (registry `IncludeInVWAP` semantics untouched; oracles/fx stay reference-only) and NEVER raises the served `source_count` / `effectiveSourceCount`. The `sources=` field in the freeze reason stays the real venue count.
- **New:** for an allow-listed structurally single-venue target (`[aggregate.composite_reference] targets`, default `crypto:XLM/fiat:GBP`, `crypto:XLM/fiat:EUR`) whose bucket is single-venue, the aggregator rebuilds the target's `[[aggregate.triangulations]]` chain **on the CURRENT bucket** — this tick's crypto/USD leg publish (its exact VWAP and distinct venue count) × an FX snap at the bucket end — and compares it with the fresh direct VWAP (`internal/aggregate/orchestrator/composite_reference.go`). The reading feeds ONE sample into the confidence factor (`triangulation_checked`), the phase-2 verdict and the mid-hold release lens:
  - agreement within `tolerance_bps` (default 75) → the move is market-wide → the phase-2 fire is **suppressed** (`corroboration_basis=composite`);
  - disagreement → venue-specific → **freeze exactly as before** (`corroboration_basis=venue composite_refuted`);
  - unavailable (leg not refreshed this tick, `leg_sources<min_leg_sources` (default 2), `fx_stale` beyond `fx_max_age_hours` (default 76, the Chainlink FX precedent — `fx_quotes` buckets are daily and pause over market closes), `fx_source_class=<oracle>`, no chain) → **freeze exactly as before**, the reason names the cause (`composite_unavailable: leg_sources=1`).
- **Only as strong as its weakest leg, and only if the leg agrees with itself (verifier advisory A1):** every venue's own bucket VWAP on the crypto/USD leg must lie within `leg_dispersion_bps` (default = `tolerance_bps`) of the leg VWAP, else `composite_unavailable: leg_dispersion=…` — a dominant venue plus a dust print 3 % off is one opinion, not two.
- **Mid-hold release uses its own band (advisory A2):** a resolved reference drives `ReleaseCorroborated` against `release_band_pct` (default 2 %), not the shared 5 % cross-oracle band — a held +4 % venue-specific offset stays frozen; a genuine move back within 2 % earns the streak.
- **Why current bucket, never a prior tick's sample:** the rejected first implementation (95da898d) read a tick-lagged composite that AGREED with the pre-spike print and so certified the pre-spike LEVEL — it suppressed a z≈50 single-venue manipulation that must freeze. A reference rebuilt from this tick's legs can only agree with the spike if the deep market moved with it.
- **Observability:** `composite_meta.corroboration_basis` + `composite_leg_sources`, the freeze reason suffix, gauges `stellarindex_aggregator_composite_corroboration{pair,window,verdict}` and `stellarindex_aggregator_composite_reference_leg_sources{pair,window,leg}`, counter `stellarindex_aggregator_composite_freeze_suppressed_total`.
- **Scope fence:** targets with ≥2 real venues on a bucket are never evaluated — their outputs are byte-identical (pinned by `TestCompositeReference_MultiVenueTargetByteIdentical`). Phase 1 (class-threshold) freezes are untouched. §8.1 (adding crypto:XLM/crypto:BTC for a real disjoint route) remains available but is no longer required for the D1 outcome.

## 11. Coverage attestation

EXAMINED (traced): internal/aggregate/router.go (all); internal/aggregate/orchestrator/{triangulate.go, triangulate_corroborate.go, phase2_freeze.go (1–320, 380–400), orchestrator.go (call sites 1098–1113, 1370–1380, 1232–1247, 1831–1859, 1637–1670, 1713–1726)}; internal/aggregate/freeze/lifecycle.go (defaults, streak); internal/aggregate/confidence/{score.go head, factors.go SourceCountFactor}; internal/aggregate/stablecoin.go ExpandTargetPairWithClassicPegs; internal/storage/timescale/{fx_quotes.go 180–317, trades.go 1679–1736, freeze_events.go 596–615}; internal/sources/external/{kraken,coinbase,bitstamp,binance}/pairs.*; registry.go FXSources; internal/config/{config.go AggregateConfig, validate.go 500–760}; cmd/stellarindex-aggregator/main.go defaultPairs; configs/ansible template + defaults + r1.yml; internal/divergence/synthetic.go head; docs/operations/v1-launch-plan.md D1 sections; internal/cachekeys/keys.go key formats. Tests run: `go test ./internal/aggregate/ -run 'Router|Route|Corrobor|EdgeDisjoint'` ok.

NOT EXAMINED: confidence.Compute weighting in full (used the orchestrator's recorded measurements instead); the CEX websocket clients' handling of a crypto/crypto symbol beyond pair registration; API /v1/price serving of a crypto-quoted pair; massive worker ticker list (fx_quotes contents are REQUIRES-LIVE-VERIFY); r1 secrets (vault-encrypted); live r1 state of any kind (no ssh, no network).


## Open questions

- REQUIRES-LIVE-VERIFY: is the deployed aggregator binary on r1 built from a commit including dba24a90 (#203, binance XLMEUR + coinbase XLM-EUR)? Operator: `ssh r1 'stellarindex-aggregator --version'` and `psql -c "select source,count(*) from trades where base_asset='crypto:XLM' and quote_asset='fiat:EUR' and ts>now()-interval '1 hour' group by 1"` — expect 4 sources.
- REQUIRES-LIVE-VERIFY: fx_quotes freshness for EUR and GBP and the massive key being live: `curl -s localhost:9090/api/v1/query?query=stellarindex_external_fx_last_quote_unix` and `psql -c "select ticker,max(bucket) from fx_quotes where ticker in ('EUR','GBP') group by 1"`.
- REQUIRES-LIVE-VERIFY: current XLM/GBP confidence score distribution (is it ≥0.5 when 2 venues are active?) — `journalctl -u stellarindex-aggregator | grep 'freeze engaged' | grep 'crypto:XLM/fiat:GBP' | tail -50` and read confidence= in reason; this bounds how often the BTC→XLM→GBP nominal route could ever clear the floor.
- Decision for Ash: accept freeze-and-auto-release as the signed posture for ETH/GBP and BTC/GBP (no real disjoint route by config), or fund the ETH/BTC venue-pair code change plus 30-day maturity?
- Decision: should crypto:XLM/crypto:BTC be exposed on /v1/price as a served pair (it will be once aggregated), or should there be a served-pair allow-list distinct from the routing pair set?
- Should the router's edge identity be extended with data-provenance keys (fx_quotes ticker rows) so USD-derived FX crosses are structurally non-disjoint, rather than relying on operators never configuring them?

## Risks

- Adding crypto:XLM/crypto:BTC as an aggregated pair publishes a new served VWAP key (vwap:crypto:XLM:crypto:BTC:*) that is NOT min_usd_volume-gated (dropForMinUSDVolume returns false for crypto quotes) — a dust XLM/BTC print can be served on /v1/price for that pair; the router side is protected by the 0.5 corroboration floor and highestConfidencePrice anchoring, the direct-serving side is not.
- Setting aggregate.pairs replaces the default set: omitting any of the 12 defaults (especially native/fiat:USD, crypto:XLM/fiat:USD) silently stops publishing those keys — the API 404 class from 2026-05-04. The list in the design repeats all 12; verify against defaultPairs() at review.
- Corroboration will remain 1 for ~30 days after apply (BootstrapConfidenceCap 0.5 == corroborationMinConfidence 0.5; unscored fallback 0.269). Anyone reading path_count=2 as success repeats the 2026-08-27 pathCount/corroborationCount confusion.
- Even mature, a 2-venue XLM/BTC edge scores near the 0.5 floor; corroboration may flap tick to tick, making freeze behaviour on XLM/GBP intermittent rather than clearly better. Consider adding Kraken XLM/XBT + Coinbase XLM-BTC (code) to raise SourceCountFactor.
- Mid-freeze the widening is dead (F6): the first single-venue anomaly on XLM/GBP still freezes normally if the BTC route sample is stale, and once frozen the route contributes nothing to release; release depends entirely on the synthetic-usd-cross lens being fresh (reflector-cex + reflector-fx/chainlink FX; chainlink FX max_age 76h weekends).
- Any future chain or pair that introduces a non-USD fiat/fiat edge (EUR/GBP, EUR/CHF via massive rows) will be counted as independent by the router — the trap is latent until a validation rule or provenance-aware edge identity exists.
- This design was produced read-only against the repo at a33d061f; whether v0.47.2 on r1 contains #203's XLMEUR/XLM-EUR pairs and whether massive's key is live are unverified (vault-encrypted secrets, no ssh).
