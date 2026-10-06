---
adr: 0052
title: ContractCallDecoder routing walks the whole auth call tree
status: Accepted
date: 2026-09-29
supersedes: []
superseded_by: null
---

# ADR-0052: ContractCallDecoder routing walks the whole auth call tree

## Context

A `ContractCallDecoder` decodes an `InvokeContract` call's arguments for contracts whose payload is only in the call (Band `relay()`, Soroswap router swaps). Router traffic mostly arrives as a sub-invocation of an aggregator, so top-level-only routing missed it. The lake holds no `fn_call` diagnostic events, but each op's auth tree is in `stellar.operations.body_xdr` for all history.

## Decision

The dispatcher routes the top-level call and every `ContractFn` node of the op's auth tree through the decoder chain (`extractInvokeContractCallTrees` -> `walkAuthEntries` -> `walkAuthTree`, pre-order, in `internal/dispatcher/dispatcher.go`).

1. `Matches(contractID, functionName)` runs once per call node, so it stays a cheap string compare. The first matching decoder owns the call.
2. `CallPath` is position, not identity: `[]` only for the real top-level call, separately authorized entries are re-rooted beneath it past its own sub-invocations. `CallPathContracts` is the ancestor-and-self contract chain.
3. Decoded output must be idempotent over duplicates: persisted rows key on call content plus `AuthOccurrence` (ordinal of a byte-identical call within one auth entry), never on `CallPath`. `soroswap_router.RouterSwap.CallSig` is the reference.
4. The auth tree records what was authorized, not what executed: `ExecutionCorroborated` is true only for the top-level call. A decoder whose output can move a price implements `ExecutionCorroborationRequirer` (Band does), and the dispatcher drops and counts its uncorroborated calls before `Decode`.

Event-based decoders are unaffected: `tx.GetTransactionEvents()` already includes nested-contract events.

## Invariant

- Nested and aggregator-wrapped calls are routed with correct paths: `internal/dispatcher/extract_call_trees_test.go`.
- The first matching decoder wins: `TestRouteContractCall_routesToFirstMatch` in `internal/dispatcher/contract_call_test.go`.
- Repeated identical calls in one auth entry stay distinct in the stored key: `internal/sources/soroswap_router/auth_occurrence_test.go`.
- A declared-but-unexecuted call never reaches a corroboration-requiring decoder: `TestProcessLedger_ForgedBandAuthEntry_NotRecognisedAsPrice` in `internal/sources/band/forged_auth_reject_test.go`.

## Consequences

- Calls reached through any wrapper are decoded; `CallPathContracts` lets a decoder attribute the wrapping aggregator.
- Every decoder pays one `Matches` per auth-tree node, and non-price decoders accept that authorized calls may never have run.
- A new ContractCallDecoder must state its dedup key under rule 3 and decide on rule 4; one that reads history needs a replay across its contract's lifetime (AGENTS.md invariant 7 picks the path).
- If diagnostic-event capture is enabled, routing over the execution trace would supersede this ADR for the ledgers that carry it.

## Evidence

`internal/dispatcher/dispatcher.go` (`ContractCallDecoder`, `ContractCallContext`, `ExecutionCorroborationRequirer`, `walkAuthEntries`), `internal/sources/soroswap_router/events.go` (`CallSig`).
