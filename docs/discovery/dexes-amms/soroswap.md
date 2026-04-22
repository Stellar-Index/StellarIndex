# Soroswap

**Status:** ✅ Primary Soroban DEX source. Event-based ingest, event
schema verified from source.

**Contracts repo:** <https://github.com/soroswap/core>
**Verified against:** `contracts/pair/src/event.rs`, `contracts/pair/src/lib.rs`,
`contracts/factory/src/event.rs`, `configs.json` at clone time
(2026-04-22).

## Architecture (verified)

Uniswap V2 clone on Soroban. Three top-level contracts:

- **Factory** — creates pair contracts for new asset pairs.
- **Pair** — one per asset pair. Holds reserves, emits swap/deposit/
  withdraw/sync/skim events.
- **Router** — multi-hop convenience layer. `swap_exact_tokens_for_tokens`
  etc. Calls pair contracts under the hood.

## Factory contract — events (from `contracts/factory/src/event.rs`)

All topics are tuples `("SoroswapFactory", <symbol_short>)`:

| Topic                             | Body                                                                        |
| --------------------------------- | --------------------------------------------------------------------------- |
| `("SoroswapFactory", "init")`     | `InitializedEvent { setter: Address }`                                      |
| `("SoroswapFactory", "new_pair")` | `NewPairEvent { token_0, token_1, pair: Address, new_pairs_length: u32 }`   |
| `("SoroswapFactory", "fee_to")`   | `FeeToSettedEvent { setter, old, new }`                                     |
| `("SoroswapFactory", "setter")`   | `NewSetterEvent { old, new }`                                               |
| `("SoroswapFactory", "fees")`     | `NewFeesEnabledEvent { fees_enabled: bool }`                                |

**For us, the only event we care about at the factory level is
`new_pair`** — it tells us when a new pair contract is deployed so we
can subscribe to its pair-level events.

## Pair contract — events (from `contracts/pair/src/event.rs`)

All topics are tuples `("SoroswapPair", <symbol_short>)`. All amount
fields are **`i128`** (our [i128 invariant](../decisions.md) applies).

### `("SoroswapPair", "swap")` — **primary signal for us**

```rust
struct SwapEvent {
    to:           Address,
    amount_0_in:  i128,
    amount_1_in:  i128,
    amount_0_out: i128,
    amount_1_out: i128,
}
```

**Important:** the `SwapEvent` **does NOT carry post-state reserves**.
Only the in/out deltas. This is a **correction to our RFP proposal**,
which claimed "Swap events include post-state reserves".

The reserves are emitted in the `sync` event that follows. Our
ctx-proposal.md needs to be corrected on this point.

### `("SoroswapPair", "sync")` — reserves after every state change

```rust
struct SyncEvent {
    new_reserve_0: i128,
    new_reserve_1: i128,
}
```

Emitted by an internal `update()` function that runs after every
swap, deposit, withdraw, and skim (verified `lib.rs:472-476`). So
**every `swap` event is immediately followed by a `sync` event** with
the new reserves in the same operation.

Consumer pattern: subscribe to both `swap` and `sync`, group events by
`(ledger_sequence, tx_hash, op_index)`, pair each swap with the
immediately following sync.

Alternative: ignore `swap` entirely, drive everything off `sync` +
the pair contract's `get_reserves()` read for state queries. This is
simpler but loses the per-trader `to` field and the in/out
directionality.

### `("SoroswapPair", "deposit")`

```rust
struct DepositEvent {
    to:             Address,
    amount_0:       i128,
    amount_1:       i128,
    liquidity:      i128,  // LP shares minted
    new_reserve_0:  i128,
    new_reserve_1:  i128,
}
```

### `("SoroswapPair", "withdraw")`

```rust
struct WithdrawEvent {
    to:             Address,
    liquidity:      i128,
    amount_0:       i128,
    amount_1:       i128,
    new_reserve_0:  i128,
    new_reserve_1:  i128,
}
```

### `("SoroswapPair", "skim")`

```rust
struct SkimEvent {
    skimmed_0: i128,
    skimmed_1: i128,
}
```

Corrects reserves if extra tokens were sent directly to the pair
contract. Rare; log for completeness.

## Reading reserves

Two options:

1. **Read from events** (`sync` carries post-state; `deposit` /
   `withdraw` also carry new reserves). Preferred — push-based, no
   extra RPC calls.
2. **Call `get_reserves()`** on the pair contract — returns
   `(i128, i128)`. Use for startup-time state reconstruction and for
   any period without event coverage.

## Price derivation

Per-swap executed price:

```
if amount_0_in > 0 && amount_1_out > 0:        # token0 → token1 swap
    price_1_per_0 = amount_1_out / amount_0_in
    price_0_per_1 = amount_0_in  / amount_1_out
if amount_1_in > 0 && amount_0_out > 0:        # token1 → token0 swap
    price_0_per_1 = amount_0_out / amount_1_in
    price_1_per_0 = amount_1_in  / amount_0_out
```

Reserve-implied spot price (after update):

```
spot_0_per_1 = new_reserve_0 / new_reserve_1
spot_1_per_0 = new_reserve_1 / new_reserve_0
```

Both computations done with `big.Rat` / `decimal.Decimal`, never
`float64`.

Soroswap fee is the Uniswap V2 standard **30 bps** baked into the pair
math (verified in `contracts/pair/src/lib.rs` math functions). It
doesn't appear in events — it's implicit in the output amount. If we
need fee-adjusted pricing, derive from the k invariant rather than
trusting a flat 0.3 % assumption.

## Known mainnet addresses

From our ctx-proposal.md:

- **Factory:** `CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2`
- **Router:**  `CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH`

These are from the proposal and have not yet been cross-verified
against an on-chain source. The repo's `configs.json` references
`.soroban/router.json` and `public/router.json` as the source of
truth for deployed addresses — verify against that before Phase 2
work begins.

## i128 handling

Every amount field in Soroswap events is `i128`. Our decoder pipeline
must:

- Not truncate to `i64`. `cdp-pipeline-workflow` does this and is
  broken (see [withobsrvr-cdp-pipeline-workflow.md](../data-sources/withobsrvr-cdp-pipeline-workflow.md)).
- Handle negative i128 correctly (two's complement, sign bit on
  `Hi`). See `stellar-extract/scval_converter.go` for the reference
  implementation we'll either depend on or copy.
- Persist to PostgreSQL `NUMERIC` columns.
- Expose as JSON strings in the API.

## Ingest plan

### Live path (hot path)

1. Bootstrap: walk factory events backward to enumerate all existing
   pair contracts (`SoroswapFactory:new_pair`).
2. Subscribe to stellar-rpc `getEvents` with two filters:
   - `contractIds: [factory_address], topics: [["SoroswapFactory", "new_pair"]]`
   - `contractIds: <dynamic pair list>, topics: [["SoroswapPair", "swap"]]`
     (and `sync` in a parallel subscription).
3. On `new_pair`, dynamically add the new pair address to the subscription
   set.
4. For each `swap` event, record the trade; correlate with the
   immediately-following `sync` in the same `(ledger, tx, op)` for
   the reserve snapshot.
5. Persist to `trades` and `pool_reserves` TimescaleDB hypertables.

### Backfill path (cold)

Same but through our Galexie data lake — each ledger's
`LedgerCloseMeta` has the full event stream. Replay from ledger of
Soroswap factory deployment forward.

## Open items

- [ ] Cross-verify factory and router addresses against the repo's
      `public/router.json` / `.soroban/router.json`. The config file
      references these but they weren't visible in our clone — check
      at deploy time.
- [ ] Enumerate **all** Soroswap pair contracts currently deployed.
      Straight walk of `new_pair` events in our data lake. Produce
      a seed list with (pair_address, token_0, token_1) for storage.
- [ ] Decide whether we persist `deposit`/`withdraw`/`skim` events as
      ledger-state signals in our DB, or only the swap+sync pair. TLP
      (LP share) analytics might need them.
- [ ] Measure event throughput on pubnet — how many Soroswap events
      per day at peak. Informs our getEvents pagination budget.
- [ ] Cross-reference with ctx-proposal.md to correct the "Swap events
      include post-state reserves" statement.

## Related

- [sdex.md](sdex.md) — classic DEX claim-atom extraction.
- [../notes/getevents-v2-proposal.md](../notes/getevents-v2-proposal.md)
  — event-stream ingestion plan.
- [../decisions.md](../decisions.md) — i128 invariant, storage rules.
- [aquarius.md](aquarius.md) — the other major Soroban AMM.
