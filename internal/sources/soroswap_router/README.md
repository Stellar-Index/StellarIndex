# soroswap_router

Decoder for the **Soroswap Router** contract (mainnet
`CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH`).

## Why a separate source

The existing `internal/sources/soroswap` package decodes events
emitted by individual **pair** contracts (`SoroswapPair("swap")`,
`SoroswapPair("sync")`). When a user does a single-hop swap directly
against a pair, those events are the full picture.

When a user routes a multi-hop swap (e.g. USDC → XLM → BTC), they
call the **router** contract's `swap_exact_tokens_for_tokens()`
function with a path. The router internally calls each pair contract
in sequence; **each pair emits its own swap event**. Without
router-level visibility we see N independent swaps with no signal
that they belong to one user-intent.

This package observes the router's `InvokeContract` call (the router
itself emits no events — it just calls down to pair contracts), so
we capture:

- The user's intent (input token, desired output, slippage tolerance)
- The full path the router selected
- A `routed_via=soroswap-router-v1` tag we apply post-hoc to the
  per-pair trades from the same tx (Phase B follow-up — schema is
  ready via migration 0025's `trades.routed_via` column).

## Wire pattern

`dispatcher.ContractCallDecoder` (same pattern as Band, which also
emits no events). Matches on `(contract_id == soroswap-router,
function_name ∈ {swap_exact_tokens_for_tokens,
swap_tokens_for_exact_tokens})`. Decodes the InvokeContract args
from the SCVal blobs into a `RouterSwap` event.

## Files

- `events.go` — `RouterSwap` event type + canonical contract IDs.
- `decode.go` — pure SCVal-args → `RouterSwap` parser.
- `consumer.go` — sink shim for the persistence layer (no-op
  for now; routed_via tagging is Phase B).
- `dispatcher_adapter.go` — `dispatcher.ContractCallDecoder`
  binding.

## Phase B / future work

1. **Routed_via tagging at insert time.** Pipeline observes a
   `RouterSwap` event in the same tx batch as one or more
   per-pair `Trade` events; tags the trades' `routed_via` column.
2. **Backfill `routers` table.** Pre-seed with this contract
   address + DeFindex vault contracts as those ship.
3. **WASM audit.** Per docs/operations/wasm-audits/README.md;
   when this lands the source's `BackfillSafe` flag in
   `external.Registry` flips to `true`.
