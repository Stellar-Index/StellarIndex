---
adr: 0040
title: Completing contract-identity gating (phoenix, defindex, aquarius, comet)
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0040: Completing contract-identity gating

## Context

ADR-0035 gates decoders on contract identity, but some protocols have no usable factory namespace: comet shares Balancer-v1 `("POOL", ...)` topics across deployments, and some contracts are never announced by a factory.
Any pubnet contract can emit a colliding shape and inject trades under our source attribution.

## Decision

**§1 Mechanisms.** Extends ADR-0035. Three sanctioned gate mechanisms, all built on `contractid.Registry`. Per-source declarations are in `internal/pipeline/gated_registry.go`.

1. **Factory-descended.** Seeded from `protocol_contracts` plus hard-coded factory IDs, and live creation events register new children (blend, soroswap, sushiswap_v3, phoenix pools, aquarius via the router's `add_pool` events). Aquarius also carries an in-code seed for history.
2. **Curated set with a declared factory.** `WithSeed(curatedSet)` plus `WithFactories`, for children a factory does not announce or whose creation events are untrusted: phoenix stake contracts (the pool deploys them) and defindex (the create body's addresses are attacker-controlled, so it never admits a child).
3. **Curated only.** No factory namespace and no creation event: comet (one mainnet pool, `comet.MainnetGatedSet`), blend_emitter and upshift. The in-code set is the whole trust root, and nothing discovers contracts at runtime. A new pool needs a code change, or an operator `seed-protocol-contracts -source <name>` for the warm table. The WASM-hash gate once proposed for comet was not built. `wasm-drift` (`internal/ops/chops/wasm_drift.go`) only checks and alerts on drift; it never admits a contract.

All three fail closed. An unlisted contract's events are not attributed and show as Claim 2a recognition gaps (ADR-0033), so a missing one is visible.

The boot warm (`GatedRegistryOptions`) seeds every source's `CuratedSet` into its registry. On the indexer path it also reconciles the set into `protocol_contracts` with provenance `factory_id = "curated"`, so the declared trust root reaches the roster and the explorer. `seed-protocol-contracts` is still the operator step for factory-anchored sources.

**§2 Rollout per source.** Seed, gate `Matches()`, wire the gate and reconcile-catalogue entry, re-derive history from the lake (`projector-replay`), then one `compute-completeness -ch` cycle returns `complete=true`. Gate code must not deploy before its seed exists. Where the seed comes from a factory walk, run `seed-protocol-contracts` once the lake covers the factory.

**§3 Enumerating a curated set.** Take every emitter the lake shows for the protocol and classify each against independent proofs: first event inside a factory `create` transaction, address in a factory `create` event body, live WASM hash the team publishes, listing in the team's own registry. A contract with no proof is excluded and flagged, never seeded. A proof that is weak alone (a create-body address) must not admit a contract by itself.

## Invariant

- A gated decoder's `Matches()` requires a registered contract, or a factory for creation events. `internal/pipeline/lockstep_ast_test.go` and `TestApplyGatedOptions_EveryGatedSourceAdmitsARegistryOnlyContract` fail CI on a missed wiring edit.
- A curated-only entry declares a non-empty trust root (`TestGatedSources_curatedOnlyDeclaresTrustRoot`).
- The boot warm seeds `CuratedSet` into every registry it builds, and the indexer reconciles it into `protocol_contracts` (`internal/pipeline/gated_registry_curated_test.go`).
- A claim that creation events predate the lake is made only with a ledger number and a query behind it.

## Consequences

- The injection vector closes source by source, each with seed rows, gated decoder tests, a re-derive and a green verdict.
- An incomplete curated set shows as `complete=false` with named contracts, never as silently attributed foreign trades.
- Curated sets are code and review-gated like any code change.
- Phoenix pools moved from curated to factory-descended once its `create` events were found in the lake (from ledger 51,572,026).

## Evidence

`internal/pipeline/gated_registry.go`, `internal/pipeline/gated_registry_curated_test.go`, `internal/contractid/registry.go`, `docs/protocols/aquarius.md`, `docs/protocols/defindex.md`, `docs/operations/wasm-audits/comet.md`.
