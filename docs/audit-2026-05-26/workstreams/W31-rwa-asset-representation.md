# W31 — RWA asset representation (ADR-0028)

## Scope

The RWA (real-world-asset) asset class introduced in ADR-0028:

- `internal/canonical/asset_rwa.go` + tests
- ADR-0028 text vs implementation
- where RWA assets surface in API responses
- the asset_class taxonomy extension (fiat / stablecoin / crypto
  / RWA / +future)

## Inputs

- ADR-0028 (`docs/adr/0028-rwa-asset-representation.md`)
- `internal/canonical/asset_rwa.go` + `asset_rwa_test.go`
- `internal/canonical/asset.go` (kind/class taxonomy)
- `internal/api/v1/assets*.go` handlers
- migration 0023 + `asset_registry.go`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W31.1 | ADR-0028 defines the RWA class with a precise scope (what qualifies as RWA, what doesn't) | ADR text |
| W31.2 | `canonical.AssetRWA` exposes the fields the API surface needs (underlying instrument, issuer, jurisdiction, etc.) | type def |
| W31.3 | Parse/format round-trip preserved | tests |
| W31.4 | `internal/canonical/asset.go` recognises the RWA kind in its kind switch | code |
| W31.5 | `/v1/assets/{slug}` returns RWA-class assets correctly | handler + integration test |
| W31.6 | Verified-currency catalogue marks RWA seed entries (if any) | `internal/currency/data/seed.yaml` |
| W31.7 | NO RWA assets are silently classified as fiat or crypto | classification rules |
| W31.8 | NO crypto or stablecoin is misclassified as RWA | classification rules |
| W31.9 | The aggregator policy doesn't double-count RWA classes | aggregator class policy |
| W31.10 | RWA price feeds (if any) are normalised + classed correctly in `internal/sources/external/registry.go` | registry |
| W31.11 | docs/architecture/multi-network-assets-migration.md mentions RWA where relevant | doc audit |

## Closure criteria

Every check terminal. Findings on:

- any RWA seed entry that's actually a crypto / stablecoin
- any RWA price that flows through an exchange-class aggregator
  pipeline (could distort VWAP)
- any place we say "this is a fiat" where we mean "RWA backed by
  fiat"
