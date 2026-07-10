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

This package observes the router's `InvokeContract` call (the
router's own emitted events are separately not-yet-decoded — see
"Deliberately-excluded surface" below), so we capture:

- The user's intent (input token, desired output, slippage tolerance)
- The full path the router selected
- **Where in the tx's call tree the router was invoked** (`call_path`
  / `call_depth` / `call_kind`, ROADMAP #11 — see below)
- A `routed_via=soroswap-router-v1` tag we apply post-hoc to the
  per-pair trades from the same tx (`internal/pipeline/routedvia.go`
  live, `stellarindex-ops tag-routed-via` historical; schema via
  migration 0025's `trades.routed_via` column).

## Wire pattern

`dispatcher.ContractCallDecoder` (same pattern as Band, which
emits no events). Matches on `(contract_id == soroswap-router,
function_name ∈ {swap_exact_tokens_for_tokens,
swap_tokens_for_exact_tokens})`. Decodes the InvokeContract args
from the SCVal blobs into a `RouterSwap` event.

**Sub-invocation coverage (task #48 + ROADMAP #11).** Most real
router traffic does NOT arrive as a top-level op: aggregator
contracts wrap the router as a nested call inside their own
authorized call tree (the 2026-05-21 census measured an **8,729×
undercount** when only top-level ops were walked —
`docs/architecture/contract-call-coverage-audit.md`). The dispatcher
walks the **full Soroban auth tree** per op
(`extractInvokeContractCallTrees`: every
`SorobanAuthorizedInvocation` root + transitively-nested
`SubInvocations`), so this decoder fires wherever the router appears
in the tree. Each captured call records its tree position:

- `RouterSwap.CallPath` — the ordered contract C-strkey chain from
  the top-level invocation down to the router itself. `[router]` for
  a direct call; e.g. `[aggregator, adapter, router]` for a call two
  wrapping layers deep. Sourced from
  `dispatcher.ContractCallContext.CallPathContracts`.
- `RouterSwap.CallDepth` — `len(CallPath)-1` (0 = direct).
- `RouterSwap.CallKind` — `top_level` | `sub_invocation`.

Why the **auth tree** and not diagnostic events: the execution-trace
`fn_call`/`fn_return` diagnostic events are captured **nowhere in
the lake** — stellar-core only emits them into tx meta when
`ENABLE_SOROBAN_DIAGNOSTIC_EVENTS = true`, which our galexie
captive-core config does not set, so ALL history in MinIO/ClickHouse
lacks them and they are not recoverable by re-reading the archive.
The auth tree lives in the op envelope (`InvokeHostFunctionOp.Auth`),
which the lake's `stellar.operations.body_xdr` retains for all
history — so live ingest and every lake re-derive decode the
identical bytes. Known caveat (documented in the coverage audit):
the auth tree is what was *authorized*, not a per-execution trace; a
router call that required no user auth AND was not the top-level op
would be invisible. Every token-moving router call requires
authorization of the token transfers beneath it, so for this source
the auth tree is complete in practice.

## Storage

One `soroswap_router_swaps` row per captured call (migration 0049;
`call_sig` PK discriminator 0056; `call_path`/`call_depth`/`call_kind`
**0101**). Rows written before 0101 carry NULL tree-position columns.

**Historical fill (operator, queued r1 heavy job):** the write path
is `ON CONFLICT DO NOTHING`, so existing NULL rows are not updated in
place — the fill is a DELETE + focused lake re-derive:

```sh
set -a; . /etc/default/stellarindex; set +a
run-heavy-job.sh router-callpath-del \
  psql "$DSN" -c "DELETE FROM soroswap_router_swaps"
run-heavy-job.sh router-callpath-rederive \
  stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
    -sources soroswap-router -contract-calls -write \
    -from 50746272 -to <tip>
```

(50,746,272 = the router's first-deploy ledger. A
soroswap-router-only invocation skips the buffered event pass and the
contract-call pass windows internally, so the full ~12M-ledger range
is one invocation. `BackfillSafe: true` — single WASM hash over the
contract's entire life, `docs/operations/wasm-audits/soroswap-router.md`.)

## Pricing invariant — router rows are NEVER priced

`AmountIn`/`AmountOut` mix a realized amount with a user-supplied
limit (which is which depends on the function), and aggregator-routed
calls routinely pass `amount_out_min = 0` (the wrapper enforces
slippage itself — see the real-bytes fixture). A router row must
never contribute to VWAP/prices. Double-guarded:
`internal/pipeline/sink.go` writes router events ONLY to
`soroswap_router_swaps` (never `trades`), and
`external.Registry` classifies the source `ClassRouter`,
`IncludeInVWAP=false`. Regression tests:
`TestFilterForVWAP_ExcludesSoroswapRouter` +
`TestTick_SoroswapRouterTradeNeverContributesToVWAP`
(`internal/aggregate/orchestrator`). The #11 sub-invocation rows ride
the identical single-table path — no new route into `trades`.

## Deliberately-excluded surface (EVERY-event principle)

Documented per the EVERY-event principle — excluded with reasons,
not silently dropped:

- **Liquidity entry points** (`add_liquidity`, `remove_liquidity`):
  visible to the same auth-tree walk, but their arg shape (token
  pair + desired/min amounts, no hop path) does not fit
  `soroswap_router_swaps`' columns (`path` cardinality ≥ 2,
  `function_name` CHECK). The lake counts the router's own emitted
  `add` (1,057) / `remove` (219) events — a liquidity surface would
  be its own table + decoder arm. Tracked with ROADMAP #89's router
  follow-up.
- **The router's own emitted contract events** (`swap` 168,557 /
  `add` / `remove` / `init` — router topics, distinct from pair
  events): not yet decoded by any source; see
  `internal/sources/soroswap/README.md` census notes (2026-07-10)
  and `docs/protocols/soroswap.md`. This ContractCall source is
  intent-side; the event side is the #89 follow-up.
- **Admin / read-only functions** (`initialize`, `set_pair_fee`,
  `router_pair_for`, quotes, …): move no tokens; out of scope for
  trade attribution.

## Files

- `events.go` — `RouterSwap` event type + canonical contract IDs +
  `CallKind*` constants + `CallSig()` (PK discriminator; tree
  position deliberately excluded from the hash so auth-tree
  duplicates of the same call still dedup).
- `decode.go` — pure SCVal-args → `RouterSwap` parser.
- `consumer.go` — legacy log-only sink shim (persistence is
  `internal/pipeline/sink.go`).
- `dispatcher_adapter.go` — `dispatcher.ContractCallDecoder`
  binding (the production seam).
- `real_bytes_test.go` + `testdata/*.b64` — golden tests over
  UNMODIFIED op bodies from the certified lake: one direct router
  call (ledger 62,000,296) and one aggregator-wrapped sub-invocation
  two levels deep (ledger 62,029,020).
