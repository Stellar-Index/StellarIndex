---
adr: 0042
title: The v1 wire shape — Unit-D collapse, slug-route discriminator, freeze contract
status: Proposed
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0042 — The v1 wire shape

- **Decides:** LC-040 (dual-shape `/v1/assets/{slug}`), LC-031 /
  Unit D (residual cross-chain wire), and what "v1.0" promises at the
  public flip. Requires @ash sign-off — these are product calls.
- **Inputs:** docs/architecture/stellar-focus-refactor-plan.md §5
  Tier 3, audit 2026-06-30 LC-030/031/040, the SDK↔spec contract test
  (pkg/client/spec_contract_test.go), the explorer generated-types
  migration (SPEC-GAP register).

## Context

Three wire-shape debts are queued behind one deliberate pre-v1
breaking release, and the public repo flip (v1.0) freezes whatever
shape exists at that moment. Deciding them together — once — is the
point of this ADR.

1. **Unit D (LC-031):** the Stellar-focus refactor removed cross-chain
   *behavior* but `pkg/client` + OpenAPI still carry the R-018
   multi-network shapes (`networks[]`, `NetworkView`,
   `PerNetworkAssetView`, the `/assets/{slug}/{network}` path,
   "cross-chain" class wording). SemVer-major, zero consumers today.
2. **LC-040:** `/v1/assets/{slug}` returns two different shapes
   (`GlobalAssetView` for catalogue slugs, `AssetDetail` for canonical
   ids) with no discriminator — clients shape-sniff on the presence of
   `asset_id` vs `ticker`.
3. **Freeze contract:** what v1.0 actually promises, and how additive
   evolution happens after it.

## Decision (proposed)

### 1. Execute the Unit-D Tier-3 collapse before the public flip

Exactly per stellar-focus-refactor-plan §5 Tier 3 (T3.1–T3.4):
Stellar-only issuance identity in `internal/currency`, remove the
`networks[]`/`NetworkView`/`PerNetworkAssetView` wire shapes and the
`/assets/{slug}/{network}` path, one `pkg/client` major bump, all
three spec artifacts regenerated. The plan's §6 "freeze the shape"
fallback is REJECTED: pre-v1 with no consumers is the only free
moment to do this; after v1.0 it costs a /v2.

### 2. `/v1/assets/{slug}` keeps one route, gains an explicit `kind` discriminator

Splitting routes was considered (canonical ids on `/v1/assets/{id}`,
slugs elsewhere) and rejected: slug URLs are the user-facing currency
identity (`/v1/assets/usdc`) and both shapes genuinely answer "tell me
about this asset". Instead:

- Both payloads gain a required `kind` field:
  `"catalogue"` (today's GlobalAssetView) vs `"stellar_asset"`
  (AssetDetail).
- The spec documents the response as a `oneOf` with a
  `discriminator: {propertyName: kind}`; the SDK exposes a typed
  union (`AssetLookup` with `Kind()`); the explorer drops its
  shape-sniffing.
- Additive on both shapes → lands WITH the Tier-3 major so clients
  absorb one break, not two.

### 3. Naming: wire is already "asset"; internals follow separately

The `Coin*`/`Currencies*` → `Asset*` rename (LC-030/D2) is internal
only — no wire field carries "coin" — so it rides as its own
mechanical PR outside this ADR's breaking release.

### 4. The v1.0 freeze contract

At the public flip:

- **The OpenAPI spec is the contract.** Endpoints in it are stable
  per SemVer (docs/architecture/semver-policy.md): additive = minor,
  breaking = /v2 only.
- **The SDK-coverage register**
  (`uncoveredOperations` in spec_contract_test.go) is the honest
  statement of SDK scope; endpoints listed there are API-stable but
  SDK-uncommitted.
- **Explorer-surface endpoints** (`/v1/ledgers`, `/v1/tx`, search,
  contracts, accounts, diagnostics) are marked `x-stability:
  experimental` in the spec at v1.0 — they serve our own explorer,
  evolve with it, and graduate to stable individually. This is the
  one place we deliberately keep freedom; it must be VISIBLE in the
  spec, not tribal.
- The additive SPEC-GAP fixes from the generated-types migration
  (board #33) land BEFORE the flip so the spec is truthful on day one.

### 5. One breaking release, one announcement

T3.1–T3.4 + the `kind` discriminator ship as a single pre-v1 release
(CHANGELOG BREAKING section, comms template in deploy/comms/), after
which the wire shape is the v1.0 candidate. No further deliberate
breaks before the flip.

## Consequences

- One coordinated break now buys a clean, Stellar-only, discriminated,
  honestly-documented v1 contract — and the contract tests (route↔spec
  ↔SDK + explorer tsc) make post-freeze drift a CI failure rather
  than a customer incident.
- The explorer loses its shape-sniff special case; SDK consumers get
  a typed union instead of duck-typing.
- Anything not ready by the flip ships as `x-stability: experimental`
  rather than blocking v1.0.

## Sign-off checklist (@ash)

- [ ] Tier-3 collapse pre-flip (vs §6 freeze fallback)
- [ ] `kind` discriminator values: `catalogue` / `stellar_asset`
- [ ] `x-stability: experimental` for the explorer surface at v1.0
- [ ] Single bundled breaking release
