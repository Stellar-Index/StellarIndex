---
adr: 0010
title: Off-chain fiat currencies as AssetType "fiat"
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0010: Off-chain fiat currencies as `AssetType = "fiat"`

## Context

Oracles and FX feeds quote against fiat (USD, EUR, ...), which is not a Stellar asset and has no `code+issuer`.
The first oracle code faked it as a classic asset with an empty issuer, which rendered as `USD-` and broke `ParseAsset` round-trips.

## Decision

`canonical.AssetType` has a fiat variant, `AssetFiat = "fiat"`; the sentinel form is removed.

- String form is `fiat:USD`, object form is `{"type": "fiat", "code": "USD"}`, and SQL storage is the same text column as other assets.
- A fiat asset carries only `Code`; `Issuer` and `ContractID` stay empty.
- `Asset.Validate` accepts a fiat code only if `canonical.IsKnownFiat(code)`; the allow-list is `knownFiatCodes` in `internal/canonical/asset_fiat.go`, currently 133 codes (ISO-4217 plus the massive.com FX feed's set plus VES).
- Adding a fiat code is a one-line code change to that list, never a new ADR.
- Commodities such as XAU are not currencies; they belong to the `rwa:` namespace (ADR-0028). Crypto tickers use `crypto:` (ADR-0014).
- Rejected: a synthetic issuer address, a separate `FiatCurrency` type, an empty-issuer classic asset, and a fake contract asset.

## Invariant

- A fiat asset has no issuer or contract id and an allow-listed code; `Asset.Validate` enforces it.
- `Asset.String()` and `ParseAsset` round-trip every asset shape including fiat; the `asset_test.go` and `asset_fiat_test.go` tables enforce it.
- Every `switch` over `AssetType` either covers every variant, has a `default` with a real body (not empty or only `fallthrough`/`break`), or carries an `//exhaustive:ignore` marker with prose; `TestAssetTypeExhaustiveGuard` in `internal/canonical/asset_type_exhaustive_guard_test.go` enforces it.
- No code accepts an empty-issuer classic asset as fiat.

## Consequences

Each new `AssetType` variant forces every switch that lacks a real `default` to add a case, which the guard finds.
Fiat-quoted API responses are self-describing (`"quote": "fiat:USD"`), and triangulation can treat the USD anchor as a real value.
The pre-v1 sentinel was never shipped, so no data migration was needed.

## Evidence

`internal/canonical/asset.go`, `asset_fiat.go`, `oracle.go`, and the exhaustive guard test above.
Grammar: `docs/reference/api-design.md` section 3.
