# Aquarius AMM

**Status:** ✅ Major Soroban AMM. Larger and richer than Soroswap; needs
a more sophisticated decoder because of multi-asset pools (up to 4
tokens) and multiple pool types.

**Repo:** <https://github.com/AquaToken/soroban-amm>
**Verified against:** `liquidity_pool_events/src/lib.rs`, `CLAUDE.md`,
`liquidity_pool_plane/src/interface.rs` at clone time (2026-04-22).

## What it is (verified from CLAUDE.md)

A Rust workspace of 28 Soroban contracts. Rust 1.92+ / Soroban SDK
25.0.2. AQUA governance token powers rewards + DAO voting.

Architecture:

```
User / Frontend
      │
      ▼
liquidity_pool_router ──── entry point: pool factory, deposits, swaps,
      │                    withdrawals, multi-hop routing, rewards config
      │
      ├── liquidity_pool              (xy=k, 2 tokens, fee tiers: 0.1% / 0.3% / 1%)
      ├── liquidity_pool_stableswap   (Curve-style, 2-4 tokens, amp coeff A)
      ├── liquidity_pool_concentrated (Uniswap V3-style, tick-based, WIP feature branch)
      │
      ├── liquidity_pool_plane        (lightweight pool metadata store)
      └── liquidity_pool_liquidity_calculator (batch liquidity queries)
```

### What this means for our indexer

- **Three pool types**, each with different swap math. But — because
  events go through a **central `liquidity_pool_events` module**, the
  event schema is uniform across all pool types. We index once, apply
  everywhere.
- **Stableswap pools can have 2–4 assets.** Our trade decoder must
  handle N-asset pools, not just 2-asset pairs. Our `CanonicalTrade`
  row already supports this (`token_in` / `token_out` only), but
  pool-state rows need variable arity for reserves.
- **Concentrated liquidity is WIP on a feature branch** as of
  2026-04-22 — not yet live on mainnet. We design for it, but don't
  integrate it Phase 1.
- **Multi-tier fees** (0.1 / 0.3 / 1 %) per pool on volatile pools.
  Our `pools` table carries a `fee_bps` column.

## Event schema (verified from `liquidity_pool_events/src/lib.rs`)

All events publish via Soroban's `e.events().publish(topics, body)`.
Topics are **variable-length** — the first topic is the event name
symbol; subsequent topics are addresses (the asset contracts and/or
trader). This means our event filter must be more flexible than
Soroswap's fixed `(Symbol, Symbol)` topic tuples.

All amount fields are **`i128`** (or `u128` at source, converted to
`i128` at emission). [i128 invariant](../decisions.md) applies.

### `deposit_liquidity`

```
topics: [Symbol "deposit_liquidity", assetA: Address,
         assetB: Address,   // optional
         assetC: Address]   // optional
body:   [stake_amount: i128, amountA: i128,
         amountB: i128,     // optional
         amountC: i128]     // optional
```

### `withdraw_liquidity`

Same shape as `deposit_liquidity`, topic `"withdraw_liquidity"`.

### `trade` — **primary signal for us**

```
topics: [Symbol "trade", sold_asset: Address, bought_asset: Address,
         trader: Address]
body:   [sold_amount: i128, bought_amount: i128, fee: i128]
```

**Key differences vs Soroswap:**
- `trade` includes **fee** in the body. Soroswap's swap event does
  not — fee is implicit there. For Aquarius we record actual fees
  per trade.
- `trader` is in topics, not body. Filter by trader-address in the
  event query if needed.
- `sold_asset` / `bought_asset` are in topics. Filter by asset-address
  efficiently for per-asset streams.
- Does **not** carry post-state reserves. See `update_reserves` /
  `reserves_sync` below.

### `update_reserves`

```
topics: [Symbol "update_reserves"]
body:   [reserve0: i128, reserve1: i128, ...]   // one per asset in pool
```

Emitted after every deposit / withdraw / trade. Variable length body
matching the pool's asset count.

### `reserves_sync` — per-asset delta

```
topics: [Symbol "reserves_sync", asset: Address]
body:   [old_reserve: i128, new_reserve: i128]
```

This is distinct from `update_reserves`. It tells us pre- and
post-state for a **single** asset. Useful for audit trails and
anomaly detection.

### Governance / fee events

- `("set_protocol_fee", fraction: u32)`
- `("claim_protocol_fee", asset)` body `(destination, amount)`

### Kill-switch events (observability)

```
kill_deposit / unkill_deposit
kill_swap    / unkill_swap
kill_claim   / unkill_claim
kill_gauges_claim / unkill_gauges_claim
```

We subscribe to the kill / unkill events for every indexed pool so
that our API can flag a pool as "paused" and exclude it from VWAP
aggregation without the data going silently stale.

## Pool plane — batch metadata reads

`liquidity_pool_plane/src/interface.rs:11`:

```rust
fn get(e: Env, pools: Vec<Address>) -> Vec<(Symbol, Vec<u128>, Vec<u128>)>;
```

One RPC call returns, for an arbitrary list of pool addresses:

- `Symbol` — the pool type (e.g. `"constant_product"`, `"stableswap"`).
- `Vec<u128>` — the pool's init params (amp coefficient for
  stableswap, fee tier for volatile, etc.).
- `Vec<u128>` — the pool's current reserves.

For our state reconstruction this is **a batch read over every pool
we care about**, not N individual calls. Much cheaper than polling
each pool contract's `get_reserves()` separately.

## Router — entry point for users

All user-initiated swaps and deposits go through
`liquidity_pool_router`. Pools are identified by
`(sorted_tokens, pool_index_hash)` — tokens must be sorted before any
router call.

The router proxies to the correct pool contract, which emits the
`trade` / `update_reserves` events.

For us: we can subscribe to the router's events too (some governance-
level events live there — fee routing, protocol kills, etc.), but the
**trade-relevant events come from the pool contracts**, not the
router.

## Pool identification

`(sorted_tokens, pool_index_hash)`. For a 2-asset pool on volatile
with a 0.3 % fee, the pool index hash encodes the fee tier. For
stableswap, it encodes the amp coefficient + fee. We can enumerate
pools by walking events (or asking the router for the current pool
registry at startup).

## Special considerations

1. **Upgrade delay** — contracts have a `UPGRADE_DELAY = 259200s`
   (3 days) for coordinated upgrades, with emergency mode bypass.
   Our indexer should be robust to contract hash changes between
   reads; always decode events by topic name, not by cached WASM
   hash.
2. **SEP-0041 LP token** — pool shares are standard SEP-41 tokens.
   The token contract itself emits `transfer` events if we want to
   track LP share movements.
3. **Rewards checkpoints** — rewards are checkpointed before balance
   changes. For pricing we don't care, but be aware if we ever index
   LP-provider positions.

## Known mainnet addresses (verified 2026-04-22)

### Router

```
CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK
```

**Verified** against stellar.expert's public contract API:

- Deployed 2024-07-25 (Unix `1721894668`).
- WASM hash `8844a760cf16788117b2a5a91d736794b3869c302aee47f8fbbcd0cc1a1096fd`.
- Creator `GAV5FBMKD2ZF4X2MGWDNQYUP7KFL7MRM6HZBY7HKQLB4BRHSCCX5J6VS`.
- Validation: `verified` against
  `AquaToken/soroban-amm`, package
  `soroban-liquidity-pool-router-contract`, commit
  `38abab4de4b933db3200a5ef1321f4fb1d44ccb4`.
- **Heavy production usage**: 377,971 subinvocations, **1,801,199
  events**, 9,681 storage entries at audit time. This is a very
  active contract.

Sourced from <https://docs.aqua.network/developers/code-examples/prerequisites-and-basics>.

### XLM SAC (referenced by Aquarius docs)

```
CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA
```

This is the **canonical native-XLM Stellar Asset Contract** on
pubnet. Not Aquarius-specific — it's the standard SAC wrapper for
native XLM that every Soroban protocol uses. Our asset normaliser
must recognise this specific address as "XLM" when seeing it in
pool-token lists.

### Testnet Router (for completeness)

```
CBCFTQSPDBAIZ6R6PJQKSQWKNKWH2QIV3I4J72SHWBIK3ADRRAM5A6GD
```

### Factory / pool-plane addresses

Not published on the docs page we read. Per Aquarius's CLAUDE.md
architecture, the **router is the entry point** for all
user-facing operations; factory / plane are internals. We enumerate
deployed pools by walking router events.

**Open item:** ask Aquarius for (or derive from router reads) the
addresses of:

- `liquidity_pool_plane` (for batch pool-metadata queries).
- `liquidity_pool_liquidity_calculator`.
- `locker_feed` (AQUA voting-power oracle).
- `fees_collector`.

## Aggregation strategy implications

Our ctx-proposal.md describes aggregating across multiple Soroban
AMMs. Aquarius differs from Soroswap in ways that affect aggregation:

- **Fee tiers:** price impact between a 0.1 % Aquarius pool and a
  0.3 % Soroswap pool differs at the same reserve ratio. Our
  implied-price calculation must fee-adjust before cross-pool
  comparison.
- **Stableswap curve:** for stable pools (A=100+) the effective price
  is nearly flat across a wide range of reserves. The simple
  `reserve_A / reserve_B` formula is wrong for stableswap. We must
  use the Curve invariant with the pool's amplification coefficient
  to compute a fair mid-price.
- **4-asset pools:** a single stableswap pool contributes to 6
  different pair-prices (C(4,2)). We emit one row per ordered pair
  in our `pools_latest_prices` view.

## Ingest plan (MVP)

1. Enumerate all Aquarius pool addresses (via events or router
   query).
2. Subscribe to `trade`, `update_reserves`, `reserves_sync`,
   `kill_swap`, `unkill_swap` events for all pools.
3. Every `trade` → canonical `Trade` row in TimescaleDB:
   `time, source="AQUARIUS", pool_address, pool_type, token_in,
    token_out, amount_in, amount_out, fee, trader`.
4. Every `update_reserves` → snapshot row in `pool_reserves` with
   full reserve vector (variable length column — Postgres `numeric[]`).
5. Every `kill_swap` / `unkill_swap` → flip a `pools.is_active` flag
   we check during VWAP window aggregation.
6. Nightly: `plane.get(all_pools)` batch-read sanity check — alert if
   our computed reserves disagree with on-chain plane by > 1 stroop.

## Open items

- [ ] Find the mainnet Aquarius router address. Cross-check against
      stellar.expert or an Aquarius deployment repo.
- [ ] Enumerate deployed pool contracts (volatile + stableswap) on
      mainnet and produce a seed list.
- [ ] Implement stableswap fair-price calculation (Curve invariant
      with amp coefficient). Reference their
      `liquidity_pool_stableswap/src/pool.rs` math.
- [ ] Watch `liquidity_pool_concentrated` for mainnet deployment;
      design our schema to accommodate tick-based pools when it ships.
- [ ] Confirm the variable-length topic handling in our event filter:
      can stellar-rpc match on "first topic = 'trade'" without
      pinning the remaining topic count? If not, we subscribe by
      contract ID only and filter topic-match client-side.

## Related

- [soroswap.md](soroswap.md) — the other major Soroban AMM.
- [sdex.md](sdex.md) — native classic DEX.
- [../decisions.md](../decisions.md) — i128 invariant.
