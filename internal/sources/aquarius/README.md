# Aquarius connector

Ingests Aquarius trade events from Soroban pool contracts. The live
decoder is topic-driven and stateless: token identities are carried in
the event topics, so the dispatcher adapter does not need router reads
or a pool-token cache. See the protocol verification page:
[`docs/protocols/aquarius.md`](../../../docs/protocols/aquarius.md).

## What this ingests

Aquarius has multiple pool shapes. The decoder handles the `trade`
event (pricing) plus the liquidity/reserves surface (analytics):

1. **Volatile** — constant-product pools (2 tokens).
2. **Stableswap** — stablecoin-oriented pools (2/3/4 tokens).
3. **Future variants** — any pool/event shape that does not match a
   known topic contract is rejected until explicitly audited and added.

The decoder emits one `canonical.Trade` per accepted `trade` event and
one `ReservesEvent` / `LiquidityEvent` per accepted reserves / liquidity
event. Unlike Soroswap, there is no swap+sync correlation buffer.

## Events we care about

| Event | Topic 0 | Carries | Lands in |
| --- | --- | --- | --- |
| `trade` | `trade` | `token_in`, `token_out`, `user` in topics; sold/bought/fee amounts in body | `trades` (source=aquarius) |
| `update_reserves` | `update_reserves` | POST-STATE reserve vector `Vec<i128>` in body (no token addresses — positional, canonical pool token order) | `aquarius_reserves` (fanned one row per token position) — the first real Aquarius TVL/liquidity-depth signal |
| `deposit_liquidity` | `deposit_liquidity` | per-token amounts + LP shares minted; token addresses in topics `[Symbol, token_0…token_{n-1}]`, body `Vec<i128>=[amount_0…amount_{n-1}, shares]` | `aquarius_liquidity` (action=deposit, fanned per token) |
| `withdraw_liquidity` | `withdraw_liquidity` | same wire shape as deposit; trailing body element is shares BURNED | `aquarius_liquidity` (action=withdraw, fanned per token) |

All four are gated IDENTICALLY on contract identity (ADR-0035/0040,
CS-026): they match only when emitted by a REGISTERED Aquarius pool, so
a look-alike cannot inject fabricated reserves any more than fabricated
trades. Reserves/liquidity are ADDITIVE analytics — Aquarius has no
published price, so these rows never reach VWAP.

`update_reserves` and the liquidity events fire on N-token pools
(topic_count 3/4/5 observed live for 2/3/4-token pools); the decoder
fans out one row per token position rather than assuming a 2-token
(a/b) shape, so stableswap events are captured, not dropped.

## Quirks

### Q1 — Token identity comes from the event topics

Aquarius `trade` events carry `Address(token_in)`,
`Address(token_out)`, and `Address(user)` directly in topics 1..3.
The decoder derives the pair from those topic values; it does not
consult a router or maintain a pool metadata cache.

### Q2 — Trade bodies are tuple-shaped SCVals

The body is a three-element tuple encoded as `ScvVec`:

1. sold amount (`i128`)
2. bought amount (`i128`)
3. fee (`i128`)

This is why the decoder leans on `internal/scval` rather than ad hoc
XDR parsing.

### Q3 — One event per trade

Aquarius differs from Soroswap and Phoenix operationally:

- Soroswap needs swap+sync correlation.
- Phoenix needs 8-field event fan-in.
- Aquarius emits one complete trade event, so decode is a pure
  single-event function.

## File layout

| File | Purpose |
| --- | --- |
| `README.md` | this file |
| `events.go` | topic symbols and source constants |
| `decode.go` | Aquarius `trade` -> `canonical.Trade`; `update_reserves`/`deposit_liquidity`/`withdraw_liquidity` -> `ReservesEvent`/`LiquidityEvent` |
| `dispatcher_adapter.go` | contract-identity-gated dispatcher-facing decoder |
| `consumer.go` | `consumer.Event` wrappers emitted after decode (`TradeEvent`, `ReservesEvent`, `LiquidityEvent`) |
| `decode_test.go`, `adapter_test.go`, `source_test.go`, `topic_decoder_reject_test.go`, `real_fixture_test.go`, `liquidity_decode_test.go` | unit + reject-path + real-fixture coverage |

## Relationship to other connectors

| Aspect | Soroswap | Phoenix | Aquarius |
| --- | --- | --- | --- |
| Event correlation | swap + sync | 8-field fan-in | none |
| Pair identity | derived from contract/pool context | derived across event set | derived directly from topics |
| Dispatcher seam | stateful decoder | stateful decoder | stateless decoder |

## Status

Production. Topic byte-match dispatching, single-event decode, and
real SCVal parsing all run against real fixtures under
`test/fixtures/aquarius/`.
