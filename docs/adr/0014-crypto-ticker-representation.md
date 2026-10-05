---
adr: 0014
title: Crypto tickers as AssetType "crypto"
status: Accepted
date: 2026-04-23
supersedes: []
superseded_by: null
---

# ADR-0014: Crypto tickers as `AssetType = "crypto"`

## Context

Oracles such as Reflector's CEX feed and RedStone publish bare tickers (BTC, ETH, USDT) with no issuer or contract.
These fit none of native, classic, soroban or fiat (ADR-0010), and the decoder was skipping every such update.

## Decision

`canonical.AssetType` has a `crypto` variant, `AssetCrypto = "crypto"`, mirroring `fiat` (ADR-0010).

- String form is `crypto:BTC`, object form is `{"type": "crypto", "code": "BTC"}`, and SQL storage is the same text column.
- A crypto asset carries only `Code`; issuer and contract id stay empty.
- Codes are allow-listed. The live list is `canonical.IsKnownCrypto` in `internal/canonical/asset_crypto.go`, and adding a code is a one-line change there, never a new ADR.
- Stablecoins and wrapped or yield tokens (USDC, USDT, USDT0, DAI, EURC, USDe, SolvBTC, ...) stay `crypto`; they are not normalised to `fiat` at ingest, and the aggregator maps them to a fiat leg at compute time (`internal/aggregate/stablecoin.go`).
- `USDT0` is a distinct code from `USDT`, and the SolvBTC NAV feeds are distinct codes from their USD-quoted variants.
- A feed_id's `/` becomes `_` in the code (URL-path safety), so the USD-quoted SolvBTC NAV feeds are `SolvBTC_FUNDAMENTAL_USD` and `SolvBTC.BBN_FUNDAMENTAL_USD`.
- A feed's quote asset lives in the connector's registry (`redstone.feedRegistry`, ADR-0028), not here: `crypto:BTC` and `crypto:SolvBTC` can be quote assets.
- Rejected: broadening `fiat` into a generic external reference, a synthetic Soroban contract id, an empty-issuer classic asset, and skipping crypto symbols.

## Invariant

- `crypto:USDC` is never the same asset as Circle's classic `USDC:GA5Z...`; keys stay `crypto:` prefixed and are never matched on bare ticker.
- A crypto asset has no issuer or contract id and an allow-listed code, and it round-trips through `String` and `ParseAsset`; `asset_crypto_test.go` enforces it.
- An unmapped oracle symbol is recorded as `raw:<symbol>`, never dropped (AGENTS.md domain rules).
- Every `AssetType` switch covers the variant, has a real-bodied `default`, or carries an `//exhaustive:ignore` marker; `TestAssetTypeExhaustiveGuard` enforces it.

## Consequences

Reflector CEX events decode end to end.
Tickers stay ambiguous across venues, so the oracle's metadata decides which market a price represents.
A future registry may bridge `crypto:USDC` to the on-chain SAC; that is a registry decision, not an asset-type one.

## Evidence

`internal/canonical/asset_crypto.go`, `asset.go`, `asset_crypto_test.go`, and the Reflector real-fixture tests under `internal/sources/reflector/`.
