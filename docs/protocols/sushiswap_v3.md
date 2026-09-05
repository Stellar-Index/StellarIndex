---
title: SushiSwap V3 — contract & event verification
last_verified: 2026-09-05
status: current
---

# SushiSwap V3 — contract & event verification

> **For the SushiSwap team:** this documents how Stellar Index
> identifies SushiSwap V3 pools on Stellar and attributes their swaps.
> Every contract below was established from on-chain evidence in the
> certified ledger lake, not from a third-party pool listing.
>
> - **Enumeration method:** factory fan-out (ADR-0035 mechanism 1) —
>   the pool factory's `pool_created` event carries the new pool's
>   address AND its two token identities, so the deploy graph is
>   self-describing and needs no RPC sweep.
> - **Last verified:** 2026-09-05 (source: `internal/sources/sushiswap_v3`;
>   lake sweep over ledgers 61,487,379 → 64,276,390).
> - **Gate status:** ✅ **GATED (factory + 58 pools).**

## What SushiSwap V3 on Stellar is

SushiSwap V3 is a Uniswap-V3-shaped **concentrated-liquidity** AMM
deployed on Soroban. Liquidity providers place capital into a chosen
tick range rather than across the whole price curve, so a pool's price
is carried by a `sqrt_price_x96` (a Q64.96 fixed-point square root of
the price) and a current `tick`, **not** by a pair of constant-product
reserves.

That distinction is load-bearing for anyone reading these rows:

- **Reserve-based TVL is wrong here.** A V3 pool's token balances say
  nothing about how much of that liquidity is in range. Stellar Index
  therefore derives **no** TVL for this source, rather than deriving a
  number that would be quietly incorrect.
- **Reserve-based pricing is wrong here.** No price is computed from
  the pool state. Every trade row carries the two realised swap
  amounts, which are exact and need no curve model at all.

## Contracts

### Factory (the gate trust root)

| Role | Contract | First event |
|---|---|---|
| Pool factory | `CD3KRKGDRVWPXVB3VXLUMQKMX6XZ6Q2H334IVZD4XXNAMKSRVQL5GLYF` | ledger 61,487,379 (2026-03-03) |

**How the factory was established.** Not from a pool listing. The
factory is the contract that emits `pool_created` in the *same
transaction as, and immediately before,* the `init` event of every pool
a public listing names — first proven on tx
`01d797bcc72dbde6664305ae30345e07c08f5a09cabd7fcf2a5e10e5b6b84765`
(ledger 61,487,379), which creates the XLM/USDC 0.30% pool
`CCR2CH4GQVCZHG7CHFVMNANCK45CU5DVKXZIIITDZQAU3CEJZ7RQH2MQ`. It is the
only contract on pubnet emitting `pool_created` for these pools.

The factory has emitted 60 `pool_created` events naming **58 distinct
pools** (two pools carry a duplicate emission inside their own creation
transaction, so the registry seed is idempotent by construction). The
full table lives in `sushiswap_v3.MainnetPools`, keyed by pool address
and carrying `(token0, token1, fee, tick_spacing, created_at)`.

### Fee tiers

The deployed set uses exactly the three canonical V3 tiers, each with
its canonical tick spacing. A pool is identified by
`(token0, token1, fee)` — two pools over the same pair at different fee
tiers are different markets.

| Fee | Pips | Tick spacing | Pools |
|---|---|---|---|
| 0.05% | 500 | 10 | 23 |
| 0.30% | 3000 | 60 | 17 |
| 1.00% | 10000 | 200 | 20 |

### Contract versions

The pools have been upgraded twice, both times driven by the factory
(`wasm_approved` on the factory, then per-pool `pool_upgraded` /
`pool_migrated`):

| Ledger | Event | Note |
|---|---|---|
| 61,594,963 | factory `wasm_approved` | first approved pool WASM |
| 61,594,973 → 61,595,002 | `pool_upgraded` + `pool_migrated` | 3 pools migrated to `schema_version` 1 |
| 62,898,168 | factory `wasm_approved` | second approved pool WASM |
| 62,898,378 → 62,898,525 | `pool_upgraded` | 54 pools |

**The swap body is field-identical across both versions.** Verified
over the whole history: all 97,349 `swap` events carry exactly the same
seven map entries. The decoder reads every field **by name** regardless
(`docs/architecture/contract-schema-evolution.md`), so a future upgrade
that appends or reorders a field stays readable.

## Events

Every event in this protocol carries a **one-element Symbol topic
vector**. The pool address is *not* in the topics, so a topic-only
decoder identifies nothing — see "Gating" below.

| Emitter | Topic | Body | Indexed as |
|---|---|---|---|
| factory | `pool_created` | `{fee, pool_address, sender, tick_spacing, token0, token1}` | registry seed (gate + token mapping); emits no row |
| factory | `wasm_approved`, `pool_upgraded`, `pool_migrated`, `set_protocol_fee` | admin | not claimed |
| pool | `swap` | `{amount0 i128, amount1 i128, liquidity u128, recipient, sender, sqrt_price_x96 u256, tick i32}` | **`trades`** |
| pool | `mint`, `burn`, `collect` | position lifecycle | recognized, projects zero rows |
| pool | `init`, `upgraded`, `migrated` | pool lifecycle | recognized, projects zero rows |

Whole-history counts (ledgers 61,487,379 → 64,276,390, swept
2026-09-05):

| Topic | Events | Pools |
|---|---|---|
| `swap` | 97,349 | 53 |
| `burn` | 2,410 | 12 |
| `collect` | 2,391 | 12 |
| `mint` | 975 | 55 |
| `init` | 60 | 58 |
| `upgraded` | 57 | 54 |
| `migrated` | 3 | 3 |

## How a swap becomes a trade

`amount0` and `amount1` are **signed deltas from the pool's point of
view**: a positive delta is a token the pool received (the trader sold
it) and a negative delta a token the pool paid out (the trader bought
it). A price-forming swap has exactly one of each.

- `amount0 > 0 && amount1 < 0` → base = token0 at `amount0`,
  quote = token1 at `|amount1|`
- `amount1 > 0 && amount0 < 0` → base = token1 at `amount1`,
  quote = token0 at `|amount0|`

Amounts are exact i128 magnitudes in each token's own smallest unit; no
decimals are applied and no ratio is computed at decode time. Assets
are identified by **contract** (`token0` / `token1` from the creation
event) — a Stellar Asset Contract for a classic asset, a plain Soroban
token otherwise. A bare asset code is never an identity here.

`taker` is the swap's output recipient. `maker` is left empty: an AMM
has no resting counterparty, which is why the source is listed in
`timescale.AMMSignerSources` so the tx source account is tagged as the
initiator.

**Trade identity fans out by event index.** A router multi-hop or a
split route invokes several pools inside one operation, so each swap's
`op_index` is packed with its in-operation event index
(`canonical.FanoutOpIndex`). Without that, every swap after the first in
an operation would collide on the trades key and be dropped silently by
the writer. The lake shows the shape is real: tx
`f6fb00ef40f433340925e768e656ebf7a8db4ac1839cc9b8603a2a980836c308`
carries swaps at event indices 2 and 14 of one operation.

### The one refused swap

One event in the protocol's whole history is **not** a trade: ledger
62,712,211 on pool `CCR2CH4G…` carries `amount0=1, amount1=0` — a
one-unit dust swap whose output rounded to nothing. It has no
derivable price, so it is refused as a recognized no-op
(`ErrNonDirectionalSwap`): zero rows, no error, counted as
expected-zero by the ADR-0033 re-derive rather than leaving the ledger
blind. Every other one of the 97,349 swaps has exactly opposite signs.

## Gating

**The identity gate is the whole safety story for this source.** Every
event carries a one-element Symbol topic, and the symbols are the most
generic on the network: a bounded 10,000-ledger census of pubnet puts
`mint` at **33%** and `burn` at **12%** of *all* contract events, and
any contract may emit a map under a `swap` symbol. A topic-only decoder
would attribute a large slice of the network's token traffic to this
source and — far worse — would let an arbitrary contract mint trades at
prices of its own choosing.

Per ADR-0035, `Matches()` therefore keys on contract identity only:

- `pool_created` is accepted **only** from `sushiswap_v3.MainnetFactories`.
  This is the trust root. Without it any contract could announce a pool
  with token identities of its choosing, register itself, and have its
  own swaps recorded as real trades.
- Every other event is accepted **only** from a registered pool.

The registry is seeded three ways, all rooted at the factory:

1. **In-code curated table** (`MainnetPools`, all 58 pools) — the
   cold-start trust root, and the only place token identities live.
2. **`protocol_contracts` DB warm** — the operator seam, so a pool
   admitted after the table was frozen survives a restart.
3. **Live `pool_created` events** — seeds both the gate and the token
   map, and fires the persistence hook.

**Coverage note.** An un-seeded real pool has its events dropped, so
registry completeness is load-bearing. It is held by the factory's own
`pool_created` events living in the lake from the factory's first ledger
(substrate continuity, ADR-0033 Claim 1). A pool the curated table
misses fails **closed** into a visible recognition gap; it is never
silently mis-attributed.

**One known drift, deliberately visible.** `protocol_contracts` stores a
contract *set*, not token identities. A pool admitted purely through the
DB warm is therefore gated IN but has no token mapping until its
creation event is replayed — its swaps are dropped and counted
(`Decoder.SkippedUnknownPool`) rather than written with invented assets.
The fix is a `projector-replay` from that pool's creation ledger. A
richer per-pool table (in the shape of `soroswap_pairs`) would close it
permanently and is the natural follow-up.

## Volume

This is a coverage and completeness integration, not a volume one.
Recent activity on the largest pool is on the order of 150 swaps a day.
Whole-history totals are in the events table above.

## Scope of the current decoder

`swap` → `trades` is the whole projected surface today. `mint`, `burn`
and `collect` are the concentrated-liquidity **position** lifecycle:
they are gated and recognized, and deliberately project zero rows,
because a V3 position is `(owner, tick_lower, tick_upper)` and wants a
table of its own rather than being forced into a reserve-shaped
liquidity row. That table — in the shape of `soroswap_liquidity`, plus
per-position tick ranges — is the natural next increment.

## References

- Decoder: `internal/sources/sushiswap_v3/`
- Gate wiring: `internal/pipeline/gated_registry.go`
- Completeness catalogue: `internal/ops/chops/reconciliation_catalogue.go`
- [ADR-0035 — factory-anchored contract gating](../adr/0035-factory-anchored-contract-gating.md)
- [Ingest pipeline](../architecture/ingest-pipeline.md)
