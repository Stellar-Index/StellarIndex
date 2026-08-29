---
adr: 0028
title: Tokenized real-world assets as AssetType "rwa"
status: Accepted
date: 2026-05-22
accepted: 2026-05-27
supersedes: []
superseded_by: null
---

# ADR-0028: Tokenized real-world assets as `AssetType = "rwa"`

## Context

RedStone's Stellar push-feed deployment is **19 per-feed contracts**.
8 of them price crypto / stablecoin assets the decoder already
models. The other 11 price
**tokenized real-world assets**: tokenized treasuries and
money-market funds, tokenized-BTC variants, and an inverse equity
ETF.

Two problems block ingesting those 11 (task #53):

### 1. `feed_id` ≠ display name

The per-feed contract's `feed_id()` getter is the exact string the
relayer passes in `write_prices(updater, feed_ids, payload)` — the
string our decoder must match. Captured on-chain 2026-05-22 via
`stellar contract invoke … -- feed_id`, five of the 19 differ from
their display name:

| Display | on-chain `feed_id` |
| --- | --- |
| EUROC  | `EUROC/EUR` |
| BENJI  | `BENJI_ETHEREUM_FUNDAMENTAL` |
| iBENJI | `iBENJI_ETHEREUM_FUNDAMENTAL` |
| SolvBTC/FUNDAMENTAL     | `SolvBTC_FUNDAMENTAL` |
| SolvBTC.BBN/FUNDAMENTAL | `SolvBTC.BBN_FUNDAMENTAL` |

The pre-#53 decoder matched `canonical.IsKnownCrypto(feedID)`. Because
the EUROC feed_id is `EUROC/EUR` — not the allow-list entry `EUROC` —
**EUROC never decoded**. Live RedStone coverage was 7 feeds, not 8.

### 2. RWA assets do not fit any existing `AssetType`

`canonical.AssetType` has five variants — native, classic, soroban,
fiat (ADR-0010), crypto (ADR-0014). A tokenized US-Treasury fund like
BENJI is none of them:

- `crypto` (ADR-0014) is semantically wrong. BENJI, GILTS, CETES,
  KTB, TESOURO, USTRY are tokenized **government debt / money-market
  funds**; SPXU is an inverse **equity** ETF. Lumping them into
  `crypto` pollutes every crypto-scoped surface (the explorer's
  crypto views, crypto aggregations, the verified-currency
  catalogue). A tokenized T-bill is not a cryptocurrency.
- `classic` / `soroban` need an issuer G-address / contract
  C-address. RedStone references these by ticker alone.
- `fiat` is an ISO-4217 reference currency. An RWA is a tradable
  instrument, not a currency.

This is the same structural gap ADR-0010 solved for fiat and
ADR-0014 solved for crypto: a bare-ticker reference with no on-chain
identity. RWA is the third sibling.

## Decision

### 1. Extend `canonical.AssetType` with a sixth variant: `rwa`

```go
const (
    AssetNative  AssetType = "native"
    AssetClassic AssetType = "classic"
    AssetSoroban AssetType = "soroban"
    AssetFiat    AssetType = "fiat"
    AssetCrypto  AssetType = "crypto"
    AssetRWA     AssetType = "rwa"     // NEW
)
```

Wire form `rwa:<CODE>` (e.g. `rwa:BENJI`) — unambiguous prefix,
`ParseAsset` dispatches in O(1), identical pattern to `fiat:` and
`crypto:`. Object form `{"type": "rwa", "code": "BENJI"}`. SQL
storage: the same text column; the `rwa:` prefix distinguishes it.

Allow-listed RWA codes (the 8 RWA feeds RedStone publishes on
Stellar mainnet, 2026-05-22):

```
BENJI  iBENJI  GILTS  CETES  KTB  TESOURO  USTRY  SPXU
```

`internal/canonical/asset_rwa.go` holds the allow-list +
`NewRWAAsset` constructor — mirroring `asset_crypto.go`. Extending
the list is a one-line amendment to this ADR.

### 2. The RedStone 19-feed registry

The decoder replaces the `IsKnownCrypto` match with an explicit
registry keyed on the **exact** `feed_id`. The quote rule: a feed_id
of the form `<BASE>/<QUOTE>` is `<QUOTE>`-denominated; all others are
USD (the RedStone convention — only EUROC carries an explicit
suffix).

| feed_id | base asset | quote |
| --- | --- | --- |
| `BTC` `ETH` `USDC` `XLM` `PYUSD` | `crypto:<id>` | USD |
| `EUROB` `MXNe` | `crypto:<id>` | USD |
| `EUROC/EUR` | `crypto:EUROC` | **EUR** |
| `BENJI_ETHEREUM_FUNDAMENTAL` | `rwa:BENJI` | USD |
| `iBENJI_ETHEREUM_FUNDAMENTAL` | `rwa:iBENJI` | USD |
| `GILTS` `CETES` `KTB` `TESOURO` `USTRY` `SPXU` | `rwa:<id>` | USD |
| `SolvBTC` | `crypto:SolvBTC` | USD |
| `SolvBTC_FUNDAMENTAL` | `crypto:SolvBTC_FUNDAMENTAL` | **`crypto:BTC`** |
| `SolvBTC.BBN_FUNDAMENTAL` | `crypto:SolvBTC.BBN_FUNDAMENTAL` | **`crypto:SolvBTC`** |

> The two bare `_FUNDAMENTAL` rows read `USD` until 2026-08-29 (D8).
> They publish a NAV **ratio**, not a dollar price — see the amendment
> at the foot of this ADR. The quote rule above ("all others are USD")
> is therefore the rule for *market* feeds only; a NAV feed is quoted
> in the reserve asset its ratio is denominated in.

### 3. SolvBTC family stays `crypto`

SolvBTC is a BTC-backed crypto token — a cryptocurrency, not a
real-world asset — so the three SolvBTC feeds are `crypto`, added to
the ADR-0014 allow-list (a one-line amendment there, which 0014's
Amendments section explicitly permits).

The `_FUNDAMENTAL` feeds publish NAV rather than market price.
**Proposed:** model each feed_id as its own distinct crypto code
(`SolvBTC`, `SolvBTC_FUNDAMENTAL`, `SolvBTC.BBN_FUNDAMENTAL`) — no
information loss, no collision, the granular-coverage default.
Collapsing market+NAV into one asset would need a price-basis
discriminator the canonical model does not have. **Open for review:**
if the operator prefers a basis dimension on `OracleUpdate` instead,
that is a larger change and a separate ADR.

**Amended 2026-08-29 (D8).** The base codes above stand, but the
*quote* did not: a NAV is denominated in the token's **reserve
asset**, and the SolvBTC family's reserve is crypto, not dollars. The
`OracleUpdate.Quote` is where that basis is carried — no new
dimension was needed after all, only the correct denominator:

| feed_id | published value | quote |
| --- | --- | --- |
| `SolvBTC_FUNDAMENTAL` | 1.00295305 — BTC per SolvBTC | `crypto:BTC` |
| `SolvBTC.BBN_FUNDAMENTAL` | 1.00000000 — SolvBTC per SolvBTC.BBN | `crypto:SolvBTC` |
| `SolvBTC_FUNDAMENTAL/USD` | 78313.02974310 — NAV in dollars | `fiat:USD` |
| `SolvBTC.BBN_FUNDAMENTAL/USD` | 78313.02974310 — NAV in dollars | `fiat:USD` |

Values as served live on r1,
`/v1/oracle/streams?include_unmapped=true`, 2026-08-29. The two
`/USD` legs are byte-identical (as they were on 2026-07-27:
`6543063913439` each), which is what fixes each denominator:
`SolvBTC_FUNDAMENTAL` = NAV_USD ÷ BTC_USD, so it is BTC-denominated;
`SolvBTC.BBN_FUNDAMENTAL` is exactly `1.00000000` on three
independent captures (lake ledger 60104689, 2026-07-27, 2026-08-29)
*and* its NAV_USD equals SolvBTC's NAV_USD, so SolvBTC.BBN is 1:1
with SolvBTC and the ratio is SolvBTC-denominated. Quoting it
`crypto:BTC` would contradict our own `SolvBTC.BBN_FUNDAMENTAL_USD`
row by the SolvBTC premium.

The general rule, pinned by
`TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset` and
`TestFeedRegistry_SuffixedFeedNeverSharesQuoteWithItsBareSibling` in
`internal/sources/redstone/feeds_test.go`: **a bare `_FUNDAMENTAL`
feed may carry a fiat quote only when the token's reserve really is
that fiat** (BENJI, iBENJI, USST, savUSD — cash / T-bill / stablecoin
reserves, each with its evidence recorded in the test's attestation
list). Everything else names its reserve asset.

## Consequences

- **Positive:** all 19 RedStone feeds decode. RedStone becomes the
  engine's only on-chain RWA price source — Reflector covers none of
  these.
- **Positive:** fixes the latent EUROC bug (feed_id `EUROC/EUR`
  silently dropped since the feed launched).
- **Positive:** the per-feed quote rule lets EUROC land as a genuine
  EUR-denominated observation instead of being mislabelled USD —
  the pre-#53 decoder hardcoded USD for every feed.
- **Negative:** a sixth variant for every `switch asset.Type` ladder.
  Mitigated — the allow-list keeps the set closed and small.
- **Negative:** RWA "prices" are NAV-quoted references updated daily
  at best (24h heartbeat). They must not be mixed into market-VWAP.
  Already handled: RedStone is `ClassOracle`, `IncludeInVWAP=false`.
- **Downstream:** the verified-currency catalogue, explorer asset
  views and `/v1/assets` `asset_class` tagging gain an `rwa` value.
  Catalogue entries stay hand-curated (R-018) — this ADR does not
  auto-populate them.

## Alternatives considered

1. **Reuse `crypto` for RWA** — rejected. A tokenized T-bill sharing
   a type with BTC mis-feeds every crypto-scoped surface. The whole
   point of a typed `AssetType` is to keep these distinct.
2. **Amend ADR-0014 to cover RWA** — rejected. ADR-0014's scope is
   crypto tickers; the repo models each bare-ticker category as its
   own sibling ADR (0010 fiat → 0014 crypto → 0028 rwa). A new
   variant is a new decision, and ADRs are immutable.
3. **One generic `external_ref` variant for fiat+crypto+rwa** —
   rejected for the same reason ADR-0014 §Alternatives-1 rejected
   merging fiat and crypto: type-level clarity is the feature.

## Amendments

_Append new RWA codes here as a one-liner. Never supersede this ADR
for an addition._

- 2026-05-22 — initial allow-list of 8 codes (the RWA feeds in
  RedStone's Stellar mainnet deployment). See `canonical.IsKnownRWA`
- 2026-07-27 — added `USDY` (Ondo US Dollar Yield, tokenized-treasury
  note), `USST` (STBL treasury-backed stablecoin), `XAUm` (Matrixdock
  tokenized gold), `deJAAA`, `deJTRSY` (Securitize deRWA tokens of
  Janus Henderson funds) — from RedStone's 2026-07-24 relayer
  expansion (ledger 63624934). Codes strip the feed-id suffixes
  (`_FUNDAMENTAL`, `/USD`) per the BENJI precedent. The registry is
  now 30 feeds; the "19-feed" figure in §2 is historical to the
  2026-05-22 capture — the live list is `redstone.feedRegistry`.
  for the live list.
- 2026-08-29 (D8) — **quote correction, not an allow-list addition.**
  `SolvBTC_FUNDAMENTAL` → `crypto:BTC`, `SolvBTC.BBN_FUNDAMENTAL` →
  `crypto:SolvBTC` (was `fiat:USD` for both). The §2 table and §3 are
  amended above with the live evidence. RedStone's bare
  `_FUNDAMENTAL` feeds publish NAV in the reserve asset; labelling a
  ~1.00 BTC ratio `fiat:USD` made `/v1/oracle/streams` say a
  BTC-backed token was worth $1.00 while its own `/USD` sibling said
  $78,313. Contained from published prices throughout (RedStone is
  `ClassOracle` / `IncludeInVWAP=false`), but served publicly with
  `mapped=true`. The four ADR-0028 RWA `_FUNDAMENTAL` feeds
  (`BENJI`, `iBENJI`, `USST`, plus crypto `savUSD`) are NOT affected —
  their reserves are dollars, so their NAV genuinely is a USD figure.
- 2026-08-29 — added `XAU` (spot gold, 1 troy oz in USD; ISO-4217
  X-code). Source is the **Reflector FX** oracle, not RedStone — every
  reflector-fx event carries an XAU slot (2026-04-23 fixtures),
  recorded as `raw:XAU` since PR #247 and paged by
  `stellarindex_ingestion_oracle_unknown_symbols` on r1 v0.48.0.
  Decision: a commodity reference is a real-world asset, not a
  currency (ADR-0010 keeps it off the fiat list — its tests pin that)
  and not a cryptocurrency, so it shares the `rwa:` namespace with
  `XAUm`; the two stay DISTINCT assets (spot vs the Matrixdock token),
  resolved via `canonical.MapOracleSymbol` (fiat → crypto → RWA → raw).

## References

- Related ADRs: ADR-0010 (fiat), ADR-0014 (crypto) — sibling
  bare-ticker variants; ADR-0003 (i128 no-truncation) — RWA prices
  still flow the U256/i128 path unchanged.
- Implementation: `internal/canonical/asset_rwa.go` (allow-list +
  constructor), `internal/canonical/asset.go` (type + String +
  ParseAsset + Validate), `internal/sources/redstone/decode.go`
  (the feed registry).
