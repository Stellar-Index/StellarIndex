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

RedStone's Stellar feeds include tokenized treasuries, funds, gold and an inverse equity ETF, referenced by bare ticker. They fit none of native, classic, soroban, fiat or crypto, and calling them `crypto` pollutes every crypto-scoped surface. A feed's on-chain `feed_id` also differs from its display name (`EUROC/EUR`, `BENJI_ETHEREUM_FUNDAMENTAL`), so an allow-list match on the display name silently drops feeds.

## Decision

`canonical.AssetType` has a sixth variant, `rwa`, written `rwa:<CODE>` and parsed by prefix like `fiat:` and `crypto:`. The closed allow-list is `knownRWACodes` in `internal/canonical/asset_rwa.go`; adding a code is a one-line change there and an addition never supersedes this ADR. Codes strip feed-id suffixes (`_FUNDAMENTAL`, `/USD`). Spot gold `XAU` is `rwa`, distinct from the Matrixdock token `XAUm`; it is not fiat (ADR-0010) and not crypto. `canonical.MapOracleSymbol` resolves fiat, then crypto, then RWA, then `raw:`.

The RedStone decoder uses an explicit registry (`feedRegistry`, `internal/sources/redstone/feeds.go`) keyed on the exact on-chain `feed_id`. A `<BASE>/<QUOTE>` feed is quoted in `<QUOTE>`; other market feeds are USD. The SolvBTC family stays `crypto`, each `feed_id` its own code.

A bare `_FUNDAMENTAL` feed publishes a NAV ratio and is quoted in the token's reserve asset: `SolvBTC_FUNDAMENTAL` in `crypto:BTC`, `SolvBTC.BBN_FUNDAMENTAL` in `crypto:SolvBTC`. It may carry a fiat quote only when the reserve really is that fiat (BENJI, iBENJI, USST, savUSD).

## Invariant

A NAV feed never carries a quote that is not its reserve asset, and a suffixed feed never shares a quote with its bare sibling. `TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset` and `TestFeedRegistry_SuffixedFeedNeverSharesQuoteWithItsBareSibling` enforce this. RWA prices are NAV references and stay out of market VWAP: RedStone and Reflector FX are `ClassOracle` with `IncludeInVWAP=false`.

## Consequences

- Every `switch asset.Type` ladder gains a variant; the closed allow-list keeps it small.
- The verified-currency catalogue stays hand-curated; this ADR does not populate it.
- `/v1/assets` `asset_class` and explorer views gain an `rwa` value.

## Evidence

- `internal/canonical/asset.go`, `asset_rwa.go`, `asset_raw.go`; `internal/canonical/asset_fiat_test.go` keeps XAU off the fiat list.
- `internal/sources/redstone/feeds.go`, `feeds_test.go`; `internal/sources/external/registry.go` (`IncludeInVWAP=false`).
