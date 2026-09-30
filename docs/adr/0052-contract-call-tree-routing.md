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

A [`ContractCallDecoder`](../../internal/dispatcher/dispatcher.go)
decodes the arguments of an `InvokeContract` call rather than an event.
It exists for contracts whose payload is only in the call — Band's
`relay()` publishes no event, and a Soroswap router swap is recorded as
the router call that requested it.

The dispatcher originally offered these decoders only each operation's
top-level call. Most Soroswap router traffic reaches the router as a
sub-invocation of an aggregator, so that walk missed almost every router
call ([contract-call-coverage-audit.md](../architecture/contract-call-coverage-audit.md),
finding 1). Offering nested calls changes what every such decoder
receives: the same `(contract, function)` can now match at any depth,
and more than once in one operation. That contract was implemented in
`internal/dispatcher` but never recorded as a decision.

The execution trace (`fn_call` diagnostic events) is the ideal source,
but the lake does not have it: stellar-core emits diagnostic events only
with `ENABLE_SOROBAN_DIAGNOSTIC_EVENTS`, our captive cores do not set
it, and the extract keeps only `Contract`-type events (audit doc,
"Walker terrain check"). The Soroban authorization tree in each
`InvokeHostFunction` op is present in `stellar.operations.body_xdr` for
all history.

## Decision

For every operation, the dispatcher routes the top-level
`InvokeContract` call **and every `ContractFn` node of the op's auth
tree** through the ContractCallDecoder chain
(`extractInvokeContractCallTrees` → `walkAuthEntries` → `walkAuthTree`,
pre-order). Decoders must honour these semantics:

1. **`Matches(contractID, functionName)` runs once per call node**, so it
   must stay a cheap string comparison with no argument parsing. The
   first decoder that matches owns the call; later decoders do not see it.
2. **Position is context, not identity.** `CallPath` gives the node's
   pre-order position (`[]` only for the op's real top-level call;
   separately authorized entries are re-rooted beneath it at indices past
   its own sub-invocations) and `CallPathContracts` gives the
   ancestor-and-self contract chain. Within one op, `(TxHash, OpIndex,
   CallPath)` is unique, but the same call can surface at several paths
   when a co-signed tx lists it in more than one auth entry.
3. **Decoded output must be idempotent over those duplicates.** A decoder
   that persists rows keys them on the call's content (contract,
   function, arguments) plus `AuthOccurrence` — the ordinal of a
   byte-identical call within the *same* auth entry — and never on
   `CallPath`. A repeat listed in another entry then collapses on the
   key, while a genuine second call in one entry is kept.
   `soroswap_router.RouterSwap.CallSig` is the reference implementation.
4. **The auth tree records what was authorized, not what executed.**
   `ExecutionCorroborated` is true only for the op's top-level call. A
   decoder whose output can move a price or other manipulation surface
   implements `ExecutionCorroborationRequirer`, and the dispatcher then
   drops uncorroborated calls before `Decode` and counts them.

Event-based decoders are unaffected: `tx.GetTransactionEvents()` already
includes events emitted by nested contracts.

## Consequences

- **Positive:** Calls reached through an aggregator or any other wrapper
  are decoded. `CallPathContracts` also tells a decoder which contract
  wrapped the call, which aggregator attribution relies on.
- **Negative:** Every ContractCallDecoder pays for a `Matches` call per
  auth-tree node. The walk also records authorized calls that may never
  have run. Rule 4 contains that risk for price-bearing decoders only;
  for any other decoder it is an accepted cost.
- **Operational impact:** A decoder added under this contract that reads
  history needs a replay across its contract's lifetime, not just
  forward coverage (AGENTS.md invariant 7 decides which replay path).
- **Downstream design impact:** A new ContractCallDecoder must state its
  dedup key under rule 3 and decide whether it needs rule 4. If
  diagnostic-event capture is ever enabled, routing over the execution
  trace would supersede this ADR for the ledgers that carry it.

## Alternatives considered

1. **Keep top-level-only routing** — rejected because it misses almost
   every aggregator-routed call, which is the dominant way the router is
   reached.
2. **Route over diagnostic `fn_call` events** — rejected for now: the
   events are absent from every captured ledger and cannot be recovered
   without re-running history with the core flag on.
3. **Key decoded rows on `(TxHash, OpIndex, CallPath)`** — rejected
   because a call listed in two co-signed auth entries would be stored
   twice.

## References

- Related ADRs: [ADR-0031](0031-data-derived-coverage-signal.md),
  [ADR-0032](0032-per-source-tables-as-projections.md) (write-path ownership),
  [ADR-0035](0035-factory-anchored-contract-gating.md) (decoders gate on
  contract identity).
- Discovery doc: [contract-call-coverage-audit.md](../architecture/contract-call-coverage-audit.md)
- Code: `internal/dispatcher/dispatcher.go` (`ContractCallDecoder`,
  `ContractCallContext`, `ExecutionCorroborationRequirer`,
  `walkAuthEntries`), `internal/sources/soroswap_router/events.go`
  (`CallSig`).
