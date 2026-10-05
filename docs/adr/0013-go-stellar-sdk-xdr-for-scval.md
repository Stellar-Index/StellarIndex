---
adr: 0013
title: Adopt go-stellar-sdk/xdr for SCVal decoding in source connectors
status: Accepted
date: 2026-04-23
supersedes: []
superseded_by: null
---

# ADR-0013: Adopt go-stellar-sdk/xdr for SCVal decoding in source connectors

## Context

Soroban connectors must decode SCVal and ContractEvent XDR in-process, since Horizon is ruled out (ADR-0001).
The XDR definitions are large and regenerate with every protocol release, so a hand-rolled fork would need a rebase each time and has already produced bugs elsewhere.

## Decision

Decode SCVal with `github.com/stellar/go-stellar-sdk/xdr`, pinned in `VERSIONS.md` and `go.mod`.
- `internal/scval` is a thin wrapper that re-exports `scval.ScVal` and `scval.ScMapEntry` as aliases and holds the shared decode helpers; many connector `decode.go` files also use the SDK's `xdr` types directly.
- The original limit of the SDK dependency to `.../xdr` and `.../strkey` is retired: the ingest path also uses the SDK's ledger, datastore and history-archive packages.
- The SDK's own RPC client is not adopted. Production ingest reads Galexie MinIO LedgerCloseMeta, and `internal/stellarrpc` survives only for the `rpc-probe` diagnostic and fixture capture (AGENTS.md invariant 6).
- Rejected: a hand-rolled or vendored SCVal parser, a separate decoder process, and decoding stellar-rpc `getEvents` JSON (it returns opaque base64 XDR).

## Invariant

- The SDK version is pinned, and an upgrade that changes wire encoding fails `TestGolden_symbolBytes` in `internal/scval/scval_test.go` before it ships.
- Decoders keep ADR-0003 amount safety across the decode path.

## Consequences

Each protocol bump costs one pin change, not a fork rebase.
The dependency graph grows by the SDK's indirect modules, and the `decoderHooks` seam in each `decode.go` lets tests inject fakes without touching the SDK path.
The `consumer.Source` interface is unaffected, so a different decoder could replace this later.

## Evidence

`internal/scval/` and its golden test, per-venue `decode_test.go` files with captured fixtures under `test/fixtures/`, and `VERSIONS.md`.
