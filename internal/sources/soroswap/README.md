# Soroswap connector

Ingests trade events from the [Soroswap](https://soroswap.finance/)
Soroban DEX. Primary Phase-1 reference:
[`docs/discovery/dexes-amms/soroswap.md`](../../../docs/discovery/dexes-amms/soroswap.md).

## What this ingests

Soroswap has two contract types:

1. **Factory** (one per network) — emits `new_pair` when a new
   liquidity pool is deployed. We track this to discover pairs
   dynamically.
2. **Pair** (one per token pair) — emits `deposit`, `swap`, `sync`,
   `withdraw`, `skim`. We extract trades from `swap` + the
   immediately-following `sync` event (see *quirks* below).

Mainnet addresses (verified during Phase 1 against
`public/mainnet.contracts.json` in soroswap-core):

| Contract | Address |
| --- | --- |
| Factory | `CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2` |
| Router | `CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH` |
| Pair WASM hash | `18051456816b66f12e773a56f77c5794fac1b1fb7ab6e22d4fad5a412770f73e` |

Per-pair addresses are enumerated dynamically by walking the
factory's `new_pair` events from a configured start ledger (or by
checking `contractCodeHash` on-chain — see [soroswap.md §4].

## Quirks

These are the traps that will bite a naive implementation.
Every one of them is encoded in `events.go` / `decode.go`
with a code comment cross-referencing this README.

### Q1 — SwapEvent carries no post-state reserves

The `swap` event has `(amount0_in, amount1_in, amount0_out, amount1_out, to)`.
It does NOT carry the pair's post-trade reserves. The immediately
following `sync` event carries `(reserve0, reserve1)`.

**Correlation rule:** the swap and its sync belong to the same
(ledger, tx_hash, op_index). The pair always emits `sync` right
after any reserves-changing operation — this is hard-coded in the
pair contract's `update()` function (verified in
`contracts/pair/src/lib.rs`).

Our decoder groups events by `(ledger, tx_hash, op_index)` and
expects `swap` + `sync` in that order. Missing sync = reject the
swap with a metric counter; never emit an incomplete trade.

### Q2 — `sync` also fires without a swap

Deposits, withdrawals, and direct `skim` operations ALSO emit
`sync`. A bare `sync` with no preceding swap is NOT a trade. We
drop it (counter-only).

### Q3 — Amounts are i128

The swap event's amount fields are SCVal i128 per the pair contract.
Our decoder returns `canonical.Amount` via the hi/lo parts path.
ADR-0003.

### Q4 — 2-topic shape vs 4-topic shape

Post-P23 (CAP-67), Soroban contracts that also emit classic asset
events may use the unified 4-topic shape. Soroswap's own events
remain 2-topic `(<event_name>, <pair_contract>)` — unrelated to
CAP-67. The 4-topic shape never appears on Soroswap pair contracts.

## Event topic reference

```
swap      topic: ["swap",       <pair_contract_addr>]
sync      topic: ["sync",       <pair_contract_addr>]
deposit   topic: ["deposit",    <pair_contract_addr>]
withdraw  topic: ["withdraw",   <pair_contract_addr>]
skim      topic: ["skim",       <pair_contract_addr>]

new_pair  topic: ["new_pair",   <token0>, <token1>]   (on factory)
```

Topic[0] is a `Symbol` SCVal; topic[1+] are `Address` SCVals.
See `events.go` for the typed enum.

## File layout (CONTRIBUTING.md §five-file convention)

| File | Purpose |
| --- | --- |
| `README.md` | This file. |
| `events.go` | Event identifier constants + topic-shape predicates. |
| `decode.go` | Raw RPC event → `canonical.Trade` (swap+sync). |
| `consumer.go` | Implements `consumer.Source`. Orchestrates the poll loop via `internal/stellarrpc`. |
| `source_test.go` | Unit tests against hand-crafted + fixture events. |
| `../../../test/fixtures/soroswap/` | Golden-file event fixtures. |

## Phase status

- Events + decode: **skeleton**. Topic predicates + the swap+sync
  correlation machinery are implemented; XDR-level SCVal decoding
  is stubbed pending the SDK dependency decision.
- Consumer: **skeleton**. Implements the `consumer.Source`
  interface; backfill + stream both drive through
  `stellarrpc.GetEvents`.

When the XDR decoder lands (separate ADR + PR), `decode.go`'s
`decodeAmount` and `decodeAddress` stubs get real implementations
without touching `consumer.go` or the topic predicates.
