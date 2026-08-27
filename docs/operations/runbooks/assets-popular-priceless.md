---
title: Runbook — assets-popular-priceless
last_verified: 2026-08-25
status: living
severity: P3
---

# Runbook — `stellarindex_assets_popular_priceless`

Covers both tripwire alerts:

- `stellarindex_assets_popular_priceless` — a real coverage gap exists.
- `stellarindex_priceless_coverage_check_stale` — the tripwire itself
  stopped sweeping (it is blind, so a gap would go unseen).

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_assets_popular_priceless` / `stellarindex_priceless_coverage_check_stale` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/pricing-coverage.yml` |
| Emitted by | `internal/pricelesscoverage` (aggregator sweep, 10 min) |
| Typical MTTR | 30–120 min (usually adding a missing USD quote-path) |
| Impact | A genuinely-traded asset renders priceless on `/v1/assets` and every downstream surface. No wrong price is served (fail-closed) — the gap is a MISSING price, not a bad one. |

## Background — what "popular + priceless + not withheld" means

Each sweep the aggregator asks, per asset, whether ALL of the following
hold (the classifier is `internal/pricelesscoverage.popularPriceless`):

1. **Priceless** — no servable USD/XLM-proxy price
   (`prices_1m` has no non-null VWAP against USDC / its SAC / `fiat:USD`
   / `native` / the XLM SAC in the last 24 h).
2. **Not withheld** — its trailing-24 h priced volume is ABOVE the
   substance serve floor ($1,000). A below-floor market is one the
   substance gate withholds *by design* (fail-closed), so its
   pricelessness is expected and does NOT count.
3. **Not wash** — the busiest single unordered `(maker, taker)` account
   pair owns **< 90 %** of its 7 d priced volume. A volume-painting wash
   farm (the reported scam AUD: ~108/109 of its trades one wallet pair)
   contributes NO market-character volume, so it can never be "popular".
4. **Popular by market-character volume** — 7 d priced volume **> $10k**
   OR 7 d trades **> 5,000**.

The gauge is the COUNT of assets meeting all four. `> 0` for 1 h+ pages.

> Why the market-character floor matters: a raw-volume floor would let
> every wash farm self-select into this alert. The concentration filter
> is what keeps the scam AUD (huge raw volume, one wallet pair) SILENT
> while still catching a genuinely-traded asset that lost its price path.

## Quick diagnosis (≤ 5 min)

```sh
# 1) How many assets, and is the sweep fresh?
curl -fs http://localhost:9465/metrics \
  | grep -E '^stellarindex_(assets_popular_priceless|priceless_coverage_check_)'

# 2) WHICH assets — the worker logs each firing asset every sweep.
journalctl -u stellarindex-aggregator -n 500 \
  | grep 'priceless-popular coverage gap'
#   -> asset_id, volume_7d_usd, trades_7d, top_account_pair_share
```

Each `priceless-popular coverage gap` warn line names the `asset_id` and
its signals. Pick one and reproduce:

```sh
# Does /v1/assets/{asset_id} really serve no price_usd?
curl -fs "https://api.stellarindex.io/v1/assets/<asset_id>" | jq '.data.price_usd'
# What quotes does it actually trade against? (the missing-path clue)
# BOTH stored directions — a pair is written as (A,B) AND (B,A), each
# holding only PART of the market. Grouping on base_asset alone
# under-reports the true bucket count by roughly half and will send you
# chasing a thin-market theory that is not real (2026-08-27: CBIJ/XLM
# reads 13 buckets one way, 14 the other, 27 unioned — floor is 20).
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT CASE WHEN base_asset = '<asset_id>' THEN quote_asset ELSE base_asset END AS counterparty,
          count(*) AS buckets, sum(trade_count) AS trades, max(bucket) AS last_seen
     FROM prices_1m
    WHERE (base_asset = '<asset_id>' OR quote_asset = '<asset_id>')
      AND bucket >= now() - INTERVAL '24 hours'
    GROUP BY 1 ORDER BY 2 DESC;"

# Is the asset even ELIGIBLE for a catalogue price? A Soroban-native
# asset is NOT (see the decision tree) — this returns 0 rows for one.
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT count(*) FROM classic_assets WHERE asset_id = '<asset_id>';"
```

Note the gate itself is direction-safe — `Store.PairMarketSubstance`
already unions both directions and de-dupes with `GROUP BY bucket`. It is
the *ad-hoc diagnosis query* that misleads, not the production measurement.

## Decision tree — `stellarindex_assets_popular_priceless`

| Finding | Likely cause | Mitigation |
| ------- | ------------ | ---------- |
| Asset trades only against a stablecoin/quote NOT in the USD-proxy set | Missing USD-proxy bridge (the AUDD/EURC class — PR #152 added USDC/SAC) | Extend the proxy set in `listAssetsBaseSelect` + `coverageQuoteProxies` (keep them in lockstep) and re-derive |
| Asset trades only against another classic asset with no USD path | No triangulation route to USD | Confirm the intermediate has a USD price; add the pair to the chain if warranted |
| Asset is a SAC form of a classic that IS priced | Alias fold gap | Confirm `[supply].sac_wrappers` maps the SAC; the alias registry should fold it (task #28 Part A) |
| Asset is genuinely a scam we should not price | It should be labelled/withheld, not surfaced here | Add it to the scam directory / withhold path so it stops counting |
| Asset is **Soroban-native** (56-char `C…` contract, no classic twin) and `classic_assets` has no row for it | **Structural — it can never receive a catalogue price.** Both catalogue price queries are built on `classic_assets` (`FROM classic_assets ca` in the listing, `JOIN classic_assets ca ON ca.asset_id = chosen.asset_id` in the detail, `internal/storage/timescale/asset_catalogue.go`), and that table holds **zero** contract assets. The substance gate is NOT the blocker — it runs and *allows* the asset, which is why `price_usd` is null with **no withheld reason** | Requires extending the catalogue price path to Soroban-native assets. Do **not** paper over it by recording a synthetic withheld verdict — that converts a real coverage gap into a silent one, which is precisely what this alert exists to catch |

**Soroban assets: SAC wrapper vs Soroban-native.** These behave completely
differently and the distinction is the first thing to establish:

- A **SAC wrapper** of a classic asset (AQUA, SHX, EURC, BTC, XRP, PYUSD,
  sUSD, BLND, CETES, VELO…) is folded onto its classic form by the alias
  registry and prices normally. Nothing to do.
- A **Soroban-native** asset — a contract with no classic counterpart — has
  no `classic_assets` row and so no price path at all.

Measured 2026-08-27: of the 15 Soroban assets over $1k/24h, **13 were SAC
wrappers (all correctly priced)** and exactly **2 were Soroban-native and
unpriced** — `CBIJ…` ($19k/24h) and `CAUP7…` ($9.4k/24h). The gap is
narrow, but it is a genuine capability gap, not a tuning problem.

Note also that a Soroban-native asset may only be reachable through
*another* Soroban-native asset: `CAUP7` trades against nothing but `CBIJ`,
so pricing it needs a **transitive hop** (`CAUP7/CBIJ × CBIJ_usd`). The
catalogue prices the long tail through exactly two hard-coded CTEs —
`direct_usd` and `asset_vs_xlm` — with no transitive step, and the
multi-hop graph router (`MaxHops=3`) only operates over `cfg.Pairs`, a
~10-pair operator allow-list that does not serve the long tail.

## Decision tree — `stellarindex_priceless_coverage_check_stale`

| Finding | Likely cause | Mitigation |
| ------- | ------------ | ---------- |
| `candidate read failed` warns in the aggregator log | Postgres unreachable / query error | Restore Postgres reachability; sweep resumes next tick |
| No `priceless-popular coverage tripwire: wired` at startup | Worker not started | Confirm the aggregator build + restart; check for a panic in `worker.Recover(logger, "priceless-coverage")` |
| Sweep slow (full-catalogue scan) | Trades hypertable pressure | Check DB load; the scan is 24 h/7 d windowed and should be seconds |

## Mitigation (≤ 120 min)

- [ ] Identify the firing asset(s) from the warn logs.
- [ ] Reproduce the missing `price_usd` and inspect the asset's quote mix.
- [ ] Apply the appropriate fix from the decision tree (usually a
      missing USD-proxy quote-path). Keep `coverageQuoteProxies`
      (`internal/storage/timescale/priceless_coverage.go`) in lockstep
      with the catalogue's `direct_usd` / `asset_vs_xlm` quote set.
- [ ] Verify `stellarindex_assets_popular_priceless` returns to 0 on the
      next sweep; the alert auto-resolves after 1 h.

## Known false-positive patterns

- **Just-listed asset mid-pricing**: an asset that crossed the
  popularity floor minutes ago, before its first price bucket
  materialised. The `for: 1h` gate masks this.
- **Process restart**: `last_success_unix` reads its pre-sweep 0 until
  the first sweep completes (seconds after start). The staleness
  `for: 30m` gate masks this.

## Related

- `internal/pricelesscoverage/` — the tripwire worker + classifier.
- `internal/storage/timescale/priceless_coverage.go` — the candidate SQL.
- `internal/pricingguard/substance.go` — the substance serve floor the
  withheld verdict tracks.
- PR #152 (`assets:` USDC/SAC stablecoin-proxy bridge) — the class of fix
  a firing alert usually needs.
- `feat/scam-labels-and-volume-character` (PR #161) — the volume-character
  design the market-character filter mirrors.

## Changelog

- 2026-08-25 — initial draft alongside the priceless-popular tripwire
  (task #28 Part B).
- 2026-08-27 — added the **Soroban-native** decision-tree row after the
  `CAUP7…` firing was misdiagnosed three times (as a routing gap, then a
  thin-market/`MinBuckets` problem, then a pair-direction bug). None were
  correct: `classic_assets` holds no contract assets, so the price is
  never computed and the gate never withholds. Also corrected the quick-
  diagnosis quote-mix query to union both stored pair directions — the
  single-direction form under-reports buckets by ~half and is what
  produced the false thin-market diagnosis.
