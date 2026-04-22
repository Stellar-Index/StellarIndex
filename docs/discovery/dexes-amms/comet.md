# Comet

**Status:** ✅ Code-verified. **Not in our original proposal.** Added
during adversarial-audit follow-up (2026-04-22).

**Repo:** <https://github.com/CometDEX/comet-contracts-v1>
**Verified against:** `contracts/src/c_pool/event.rs`,
`contracts/src/c_pool/call_logic/pool.rs` at clone time (2026-04-22).

## What it is

Soroban implementation of **Balancer v1's weighted AMM** — pools hold
N ≥ 2 tokens each with its own weight, and trading math preserves
the constant weighted-geometric-mean invariant rather than Uniswap's
simple `x*y=k`.

**Key property:** N-asset pools with arbitrary weights (e.g. 80/20
ETH/USDC, or 33/33/33 BTC/ETH/USDC). This is different from both
Soroswap (2-asset constant-product, 30 bps) and Aquarius (2-asset
constant-product + 2-4 asset stableswap).

## Vendored inside Blend

Notably, a **`comet.wasm` binary is vendored at the root of the Blend
contracts repo** (`blend-contracts/comet.wasm`). Blend uses a Comet
pool as its backstop LP (BLND / USDC weighted pool). So Comet is
already on pubnet as part of Blend's live deployment.

Whether there is a standalone Comet DEX with public trading pools
beyond Blend's backstop is an **open question** — see §Open items.

## Event schema (verified)

From `c_pool/event.rs` and `c_pool/call_logic/pool.rs:21` where the
topic constant is defined:

```rust
const POOL: Symbol = symbol_short!("POOL");
```

All events use the topic tuple `("POOL", <event_name>)` — note this
uses the uppercase `POOL` symbol, not a per-protocol namespace.

### `SwapEvent` — `("POOL", "swap")`

```rust
struct SwapEvent {
    caller:           Address,
    token_in:         Address,
    token_out:        Address,
    token_amount_in:  i128,
    token_amount_out: i128,
}
```

Clean structured body (unlike Phoenix's 8-event unwrap). One event per
swap; all fields in the body. **Similar shape to Soroswap's
SwapEvent.**

### `JoinEvent` — `("POOL", "join_pool")`

```rust
struct JoinEvent {
    caller:          Address,
    token_in:        Address,
    token_amount_in: i128,
}
```

Multi-token joins emit one `JoinEvent` per token — so joining a
3-token pool produces 3 events (verified by cross-referencing the
join-pool call emitting events in a loop).

### `ExitEvent` — `("POOL", "exit_pool")`

Symmetrical with join:

```rust
struct ExitEvent {
    caller:           Address,
    token_out:        Address,
    token_amount_out: i128,
}
```

### `DepositEvent` — `("POOL", "deposit")`

Single-asset deposit (single-sided liquidity addition):

```rust
struct DepositEvent {
    caller:          Address,
    token_in:        Address,
    token_amount_in: i128,
}
```

### `WithdrawEvent` — `("POOL", "withdraw")`

Single-asset withdraw:

```rust
struct WithdrawEvent {
    caller:           Address,
    token_out:        Address,
    token_amount_out: i128,
    pool_amount_in:   i128,   // pool-share tokens burned
}
```

### What's absent

- **No reserve / sync event.** Same situation as Phoenix. To maintain
  a running reserve snapshot we must accumulate from the
  swap/join/exit/deposit/withdraw deltas, or query pool state
  directly.
- **No pool-creation event in the observed surface.** Comet pools
  are deployed via a factory that's out of scope for our clone —
  see `contracts-contracts/factory/` (different path).

## i128 everywhere

Every amount field in every Comet event is `i128`. Our
[i128 invariant](../decisions.md) applies.

## Pricing from a Comet swap

Like Soroswap, we can derive the executed price per swap:

```
price_out_per_in = token_amount_out / token_amount_in
```

But **unlike Soroswap's constant-product**, a Comet pool can have
unequal weights and/or more than 2 tokens. The reserve-implied spot
price formula is:

```
spot_price_out_per_in =
    (reserve_in  / weight_in) /
    (reserve_out / weight_out) * (1 + swap_fee)
```

— i.e. the Balancer v1 formula. Requires knowing both reserves and
both weights. Our pool-state tracker must capture weight alongside
reserves.

## Ingest strategy

1. **Find Comet pools.** Either:
   - Query factory events (when we identify the factory contract —
     open item), or
   - Hardcode the Blend backstop pool address (available in Blend's
     deploy manifest) as the minimum coverage.
2. Subscribe to `("POOL", "swap")`, `join_pool`, `exit_pool`,
   `deposit`, `withdraw` for each known pool.
3. For each swap event → canonical trade row.
4. Track reserves from join/exit deltas or by periodic pool-state
   reads.

Because Blend's backstop pool trades a specific BLND/USDC pair,
ingesting it contributes a BLND price source for us. Worth having
even as the minimum case.

## Open items

- [ ] Find the Comet factory contract and any public Comet DEX front-
      end beyond Blend's backstop. Searches suggest there's a
      `cometDEX` site but no confirmed pubnet trading deployment.
- [ ] Read pool initialization to capture the `weight` field per
      token. This is needed for our spot-price formula.
- [ ] Identify whether Comet pools support up to N=8 tokens
      (Balancer v1 max) on Stellar, or a narrower limit.
- [ ] Confirm Blend's backstop Comet pool address on mainnet.
- [ ] Decide whether Comet coverage is worth Phase-1 effort or
      deferred to Phase-2+ (Blend backstop is low-volume; non-Blend
      Comet pools unclear).

## Verdict

Low-priority compared to Soroswap/Aquarius/Phoenix — but including
Blend's backstop pool gives us BLND pricing at near-zero additional
cost once the decoder exists. Shared event schema (i128 structured
bodies) means it reuses our generic Soroban-AMM decoder with minor
adjustments.

## References

- Comet contracts repo: <https://github.com/CometDEX/comet-contracts-v1>
- Blend's vendored `comet.wasm`:
  `.discovery-repos/blend-contracts/comet.wasm`
- Balancer v1 whitepaper:
  <https://balancer.fi/whitepaper.pdf> (for the weighted-AMM math)
- Related: [soroswap.md](soroswap.md), [aquarius.md](aquarius.md),
  [blend.md](blend.md), [phoenix.md](phoenix.md).
