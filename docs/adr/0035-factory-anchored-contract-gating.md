---
adr: 0035
title: Factory-anchored contract gating for Soroban decoders
status: Accepted
date: 2026-06-12
supersedes: []
superseded_by: null
---

# ADR-0035: Factory-anchored contract gating for Soroban decoders

## Context

Topic symbols are not unique across protocols: every AMM emits `swap`, and SACs and token contracts emit `supply`, `claim` and `mint`.
Decoders that matched on topic alone attributed foreign contracts' events to a protocol. The completeness reconcile then re-derived through the same decoder and reported a clean 100% over polluted data.

## Decision

Soroban event decoders gate `Matches()` on contract identity, never on topic alone. Identity is anchored on the protocol's factory and fans out to every contract the factory creates.

1. **Anchor.** The factory is a hard-coded mainnet trust root, and it is a set, not a scalar. A protocol can have several factories (Blend and Soroswap do), and the set must be verified complete against the lake's creation events, not taken from discovery docs. `contractid.WithFactories` and `Registry.IsFactory` carry it.
2. **Fan out.** Creation events (`new_pair`, `deploy`, `add_pool`, `create`, `pool_created`) announce children into an in-memory `contractid.Registry`, transitively.
3. **Gate.** A creation event matches only when `ev.ContractID` is a registered factory, so a foreign contract cannot inject a child. A child event matches only when its contract is a registered descendant.
4. **Seed from the factory.** The registry is complete before a child's events are processed: live creation events, the genesis walk (`seed-protocol-contracts`), and the warm from the persisted `protocol_contracts` table (`GatedRegistryOptions`).
5. **Where there is no factory** to anchor on, ADR-0040 defines curated-set gates.

Soroswap keeps its own `soroswap_pairs` registry, which also carries token identities. Per-source gate metadata is declared in `internal/pipeline/gated_registry.go`.

## Invariant

- Every Soroban decoder gates `Matches()` on contract identity, and attribution never rests on topic alone (AGENTS.md domain rules). A decoder dispatches on `topic[0]` and decodes by field name.
- The factory gate is set membership, never a single documented address. A creation event from a non-factory is ignored.
- A projection reconcile seeds each registry the way live ingest does. `TestApplyGatedOptions_EveryGatedSourceAdmitsARegistryOnlyContract` (`internal/ops/chops`) guards it.
- A curated-only gate declares a non-empty trust root (`TestGatedSources_curatedOnlyDeclaresTrustRoot`).

## Consequences

- Coverage is a hard function of registry completeness. An unseeded real child is dropped, which is silent under-capture traded for silent over-capture. The factory seed is provable because the creation events are themselves in the hash-chained lake (ADR-0033). Children not created by a factory need ADR-0040.
- Rows written under topic-only matching are phantoms once a gate lands. A gated source needs a one-time lake re-derive (`projector-replay`) to purge them, and the reconcile reports them until then. Gate code must not deploy before its seed exists, or live ingest fail-closes on every pool.
- A factory deployed after enumeration is a residual risk, mitigated by re-running the creation-event enumeration.

## Evidence

`internal/contractid/registry.go`, `internal/pipeline/gated_registry.go`, `internal/sources/soroswap/dispatcher_adapter.go`, `internal/sources/blend/dispatcher_adapter.go`, `internal/pipeline/gated_registry_curated_test.go`, `migrations/0061_protocol_contracts.up.sql`.
