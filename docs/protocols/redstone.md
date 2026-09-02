---
title: RedStone — contract & event verification
last_verified: 2026-07-27
status: current
---

# RedStone — contract & event verification

> **For the RedStone team:** this is the Adapter contract and the 30-feed
> registry Stellar Index ingests. Please confirm the Adapter address and
> tell us if new feeds have been added since our 2026-07-24 capture (a
> feed we don't have in the registry is skipped, not mis-attributed —
> see Q3).
>
> - **Enumeration method:** single Adapter contract (pinned by ID) + an
>   in-code registry of the 30 mainnet `feed_id` strings (19 captured
>   2026-05-22 + 11 from the 2026-07-24 relayer expansion), each mapped
>   to a canonical `(base, quote)` pair.
> - **Last verified:** 2026-07-27 (source: `internal/sources/redstone`;
>   feed_ids captured on-chain 2026-05-22 + 2026-07-24; WASM audit
>   `docs/operations/wasm-audits/redstone.md`, 2026-04-29).
> - **Gate status:** ✅ Gated (ADR-0035): the decoder matches ONLY the
>   configured Adapter contract ID.

## What RedStone is

[RedStone](https://app.redstone.finance) is a multi-feed oracle. On
Stellar, **one Soroban Adapter contract owns price storage for every
feed**; thin per-feed proxy contracts delegate reads to the Adapter
but emit no events (they only serve `price()` reads, so we do not
subscribe to them).

| Role | Mainnet address |
|---|---|
| Adapter (the only subscribed contract) | `CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG` |

## Event decoded — one batch event, N feed updates

RedStone emits a single `("REDSTONE",)` event each time the relayer
pushes a batch update. Decoding one event produces **one
`canonical.OracleUpdate` per `(feed_id, price)` pair** in the batch
(synthetic `op_index` spaced by 1024 so each feed keeps a distinct
identity in `oracle_updates`).

| Field | Where it appears | Decoded as |
|---|---|---|
| `updater` | body Map | relayer `Address` (kept for audit, ignored for VWAP) |
| `updated_feeds` | body Map → `Vec<PriceData>` | one row per feed updated this batch |
| `price` (per feed) | `PriceData.price` | `U256` at fixed `DECIMALS = 8` |
| `package_timestamp` / `write_timestamp` | `PriceData` | `u64` Unix **milliseconds** |
| **`feed_ids`** | **InvokeContract op args** (NOT the event body) | `Vec<String>` — see Q1 |

### Q1 — `feed_ids` are not in the event body

The relayer calls `adapter.write_prices(updater, feed_ids, payload)`;
the emitted event carries prices + timestamps but **no feed identifiers**.
The decoder reads `feed_ids` from `events.Event.OpArgs` (populated by
`internal/dispatcher` from the InvokeContract op envelope) and zips
one-to-one against `updated_feeds` when lengths match. When the
Adapter's freshness verifier rejects a feed, the entry drops from
`updated_feeds` without dropping from `feed_ids`, breaking the zip —
a real, ongoing class (1,626 events across [59258375, tip] on the
2026-07-29 full completeness verify). For those, the decoder recovers
the mapping from the **signed payload** in `OpArgs[2]`: the Adapter
stores each accepted feed's signer-value **median**, so each surviving
price must equal exactly one candidate feed's payload median at its
`package_timestamp` (`internal/sources/redstone/payload.go`; verified
byte-exact against the real ledger-59,258,375 event — the surviving
price equals BTC's median of three signer values, ETH was the dropped
feed). Attribution demands uniqueness and a bijection; anything
ambiguous — e.g. two USD-stables with identical medians — refuses the
whole event (`ErrAmbiguousSubset`, wrapped in
`ErrFeedIDCountMismatch`, counted on
`stellarindex_source_decode_errors_total{source="redstone"}`) so it
stays in the completeness verifier's honest-blind class rather than
misattributing.

### Q2 — Event body is wrapped in `ScVal::Bytes`

The Rust Adapter does `self.to_xdr(env).to_val()`, yielding an
`ScVal::Bytes` holding XDR-serialised body bytes — not the `ScVal::Map`
you'd expect. The decoder type-tests and unwraps the inner XDR before
destructuring.

## Feed registry (ADR-0028)

`feeds.go` holds all 30 mainnet feeds keyed on the **exact** on-chain
`feed_id()` string (19 captured 2026-05-22, 11 more from the
2026-07-24 relayer expansion — see below). The feed_id is not always
the display name — `EUROC` is `EUROC/EUR`, `BENJI` is
`BENJI_ETHEREUM_FUNDAMENTAL`, SolvBTC variants carry `_FUNDAMENTAL`
suffixes. Two correctness consequences:

- **Quote asset is per-feed.** RedStone publishes USD-denominated
  *market* prices unless the feed_id carries an explicit `/<QUOTE>`
  suffix (`EUROC/EUR`; the 2026-07-24 `/USD` suffixes restate the
  default). A bare `_FUNDAMENTAL` feed is the exception: it publishes
  NAV in the token's **reserve asset**, so `SolvBTC_FUNDAMENTAL` is
  quoted `crypto:BTC` and `SolvBTC.BBN_FUNDAMENTAL` `crypto:SolvBTC`.
  The registry carries the quote per feed. (Pre-registry the decoder
  hardcoded USD, mislabelling EUROC; until 2026-08-29 the two SolvBTC
  NAV ratios were still `fiat:USD` — D8, see below.)
- **RWA feeds** (BENJI, GILTS, TESOURO, CETES, KTB, USTRY, SPXU, iBENJI,
  USDY, USST, XAUm, deJAAA, deJTRSY)
  decode as `canonical.AssetRWA`, deliberately NOT `crypto`, so a
  tokenized T-bill never lands in a crypto-scoped surface.
- **A feed_id outside the registry** is skipped
  per-entry and counted on
  `stellarindex_source_unknown_symbols_total{source="redstone"}`
  (alert `stellarindex_ingestion_oracle_unknown_symbols`) — skipped,
  never mis-attributed.

### 2026-07-24 relayer expansion (ledger 63624934)

RedStone's relayer began publishing 11 feed_ids beyond the original
19-feed registry. Fail-closed behavior held — unknown feeds were
skipped per-entry, and batches containing ONLY new feeds were refused
whole (`ErrEmptyUpdates`) — but that meant ~5,600 events went
undecoded until the registry caught up (2026-07-27). Per-feed
decisions, each verified live 2026-07-27 against
`api.redstone.finance` (`?provider=redstone`) with CoinGecko
magnitude cross-checks:

| feed_id | maps to | quote | evidence (live 2026-07-27) |
| --- | --- | --- | --- |
| `EUROC` (bare) | `crypto:EUROC` | **USD** | 1.1398 ≈ EUR/USD (CG euro-coin 1.14) — distinct series from the EUR-quoted `EUROC/EUR` feed (1.0003) |
| `USDe` | `crypto:USDe` | USD | 0.9998 (CG ethena-usde 0.99957) — Ethena synthetic dollar, crypto per ADR-0014 |
| `sUSDe` | `crypto:sUSDe` | USD | 1.2407 (CG ethena-staked-usde 1.24) — staked, value-accruing |
| `savUSD_FUNDAMENTAL` | `crypto:savUSD_FUNDAMENTAL` | USD | 1.1877 — Avant staked USD, crypto-native yield vault (same class as sUSDe, NOT rwa) |
| `SolvBTC_FUNDAMENTAL/USD` | `crypto:SolvBTC_FUNDAMENTAL_USD` | USD | 65,430 — NAV **in USD**; the unsuffixed feed publishes the NAV **ratio** vs BTC (1.0029, confirmed in our stored rows), so the two get distinct codes. Feed_id `/` normalized to `_` (URL-path safety) |
| `SolvBTC.BBN_FUNDAMENTAL/USD` | `crypto:SolvBTC.BBN_FUNDAMENTAL_USD` | USD | 65,430 — byte-identical to the SolvBTC leg on every capture, which is what identifies SolvBTC (not BTC) as SolvBTC.BBN's reserve |
| `USDY_FUNDAMENTAL/USD` | `rwa:USDY` | USD | 1.1408 (CG ondo-us-dollar-yield 1.14) — Ondo tokenized-treasury note |
| `USST_FUNDAMENTAL` | `rwa:USST` | USD | 1.0096 — STBL treasury-backed stablecoin |
| `XAUm_FUNDAMENTAL/USD` | `rwa:XAUm` | USD | 4,115.67/oz (CG pax-gold 4,088) — Matrixdock tokenized gold |
| `deJAAA_FUNDAMENTAL/USD` | `rwa:deJAAA` | USD | 1.0404 — Securitize deRWA of Janus Henderson JAAA (CLO ETF) |
| `deJTRSY_FUNDAMENTAL/USD` | `rwa:deJTRSY` | USD | 1.0315 — Securitize deRWA of Janus Henderson JTRSY (treasury fund) |

None of the new feeds needs `Invert` (all are published
token-in-quote, unlike MXNe's market-FX orientation). RWA codes strip
the feed-id suffix per the BENJI precedent; crypto codes keep the
full feed_id so market vs NAV series never collide. A registry
invariant test pins that no two feed_ids share a `(base, quote)`
pair — same-batch feeds can never double-write one series.

### NAV feeds are quoted in their reserve asset (D8, 2026-08-29)

A bare `_FUNDAMENTAL` feed publishes **net asset value in the asset
the token is a claim on**, which is USD only when the reserve is
dollars. Two feeds were registered `fiat:USD` anyway:

| feed_id | published (r1, 2026-08-29) | was | now |
| --- | --- | --- | --- |
| `SolvBTC_FUNDAMENTAL` | `1.00295305` — BTC per SolvBTC | `fiat:USD` | `crypto:BTC` |
| `SolvBTC.BBN_FUNDAMENTAL` | `1.00000000` — SolvBTC per SolvBTC.BBN | `fiat:USD` | `crypto:SolvBTC` |
| `SolvBTC_FUNDAMENTAL/USD` | `78313.02974310` | `fiat:USD` | `fiat:USD` (unchanged) |
| `SolvBTC.BBN_FUNDAMENTAL/USD` | `78313.02974310` | `fiat:USD` | `fiat:USD` (unchanged) |

So `/v1/oracle/streams?include_unmapped=true` served, with
`mapped=true`, a BTC-backed token at "$1.00" next to its own
`$78,313.03` row. Never entered a published price — RedStone is
`ClassOracle` / `IncludeInVWAP=false` — but it was public.

Denominators derived from the rows themselves: the two `/USD` legs are
byte-identical (also on 2026-07-27: `6543063913439` each), so
`SolvBTC_FUNDAMENTAL` = NAV_USD ÷ BTC_USD ⇒ BTC-denominated, while
`SolvBTC.BBN_FUNDAMENTAL` is exactly `1.00000000` on three independent
captures (lake ledger 60104689, 2026-07-27, 2026-08-29) with a NAV_USD
equal to SolvBTC's ⇒ SolvBTC.BBN is 1:1 with SolvBTC and its ratio is
SolvBTC-denominated.

The four RWA/crypto `_FUNDAMENTAL` feeds whose reserve genuinely is
dollars — `BENJI`, `iBENJI`, `USST`, `savUSD` — keep `fiat:USD`, each
with its evidence recorded in `feeds_test.go`'s attestation list.
`TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset` fails CI for any
future bare `_FUNDAMENTAL` feed registered against a fiat quote
without that evidence.

## Aggregator treatment — reported, not counted

Class `Oracle` / `IncludeInVWAP=false` (`external.Registry`). Surfaced on
`/v1/sources` for transparency, excluded from VWAP (RedStone publishes
already-aggregated derived prices under its own methodology). RWA feeds
are additionally never eligible for market VWAP.

## Topic census re-confirmation (ROADMAP #89, 2026-07-10)

A read-only lake topic census against the Adapter contract found
only the `REDSTONE` topic[0] (153,763 events) — no other topic
emitted. Consistent with "one batch event, N feed updates" above.

## Backfill safety

`BackfillSafe = true` (audited 2026-04-29). The Adapter's events are
durable Soroban events; backfill decodes identically to live, subject to
`events.Event.OpArgs` availability (populated for backfill ledgers via
`internal/dispatcher`).

## Update cadence / staleness

A feed may go quiet up to 24h if the underlying price hasn't moved > 0.2%.
The decoder publishes `DefaultResolutionSeconds = 86400` so the
`oracle-stale` alert (fires at > 10× resolution) uses the correct
threshold for a legitimately quiet feed.

## References

- Source package: `internal/sources/redstone/README.md`
- ADR-0028 (RWA asset modelling); ADR-0014 (crypto-ticker representation)
- Sibling oracles: [reflector.md](reflector.md), [band.md](band.md)
