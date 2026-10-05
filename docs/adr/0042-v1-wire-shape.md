---
adr: 0042
title: The v1 wire shape — Unit-D collapse, slug-route discriminator, freeze contract
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0042: The v1 wire shape

## Context

The public flip freezes whatever wire shape exists, and three debts sat in it: cross-chain shapes the Stellar-only refactor left in the spec and SDK, a `/v1/assets/{slug}` route returning two shapes with no discriminator, and no statement of what v1.0 promises.

## Decision

1. The Stellar-only collapse ran before the flip: no `networks[]`, `NetworkView`, `PerNetworkAssetView` or `/assets/{slug}/{network}` in the spec, SDK or API. Freezing the old shape was rejected, since removing it after v1.0 costs a `/v2`.
2. `/v1/assets/{slug}` stays one route. Both payloads carry a required `kind`, `"catalogue"` or `"stellar_asset"`, and the SDK exposes them as the typed union `AssetLookup`. The spec documents the response as a plain `oneOf` of the two envelopes, not an OpenAPI `discriminator` block, because `kind` sits inside each envelope's `data` and the spec renderer does not resolve that.
3. The OpenAPI spec is the v1.0 contract: additive change is a minor release, breaking change is `/v2` only (`docs/architecture/semver-policy.md`).
4. `uncoveredOperations` in `pkg/client/spec_contract_test.go` is the statement of SDK scope: listed endpoints are API-stable but SDK-uncommitted.
5. Explorer-surface endpoints (`/ledgers`, `/tx`, `/search`, `/contracts`, `/accounts`, `/diagnostics`, `/operations`, `/directory`) are marked `x-stability: experimental` in the spec. They serve our own explorer and graduate to stable one at a time.

## Invariant

- Every SDK method exists in the spec, and every spec operation is SDK-covered or listed in `uncoveredOperations` with a reason: `pkg/client/spec_contract_test.go`.
- Both `/v1/assets/{slug}` payloads carry `kind`, and the SDK dispatches on it: `internal/api/v1/assets_global_test.go`, `pkg/client/asset_lookup.go`.
- Explorer-surface operations stay `x-stability: experimental` in `openapi/stellar-index.v1.yaml`; a new explorer endpoint is marked the same way, by review.

## Consequences

- The explorer and SDK dispatch on `kind` instead of sniffing field names.
- Experimental endpoints may change without a major bump; that freedom is visible in the spec.
- Anything not ready at the flip ships experimental rather than blocking v1.0.

## Evidence

`openapi/stellar-index.v1.yaml` (`getAsset` `oneOf`, `x-stability`), `pkg/client/asset_lookup.go`, `pkg/client/spec_contract_test.go`.
