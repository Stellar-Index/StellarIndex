# Phoenix DEX connector

Ingests trade events from [Phoenix](https://app.phoenix-hub.io) —
a Stellar-native DEX with x·y=k + stableswap pools. See the
protocol verification page:
[`docs/protocols/phoenix.md`](../../../docs/protocols/phoenix.md).

## What this ingests

Phoenix's event model is **high-cardinality and unusual** relative to
Soroswap / Aquarius / Blend / Comet. Verified from Phoenix's
`contracts/pool/src/contract.rs:1172-1185`:

A single `swap` on a Phoenix pool emits **8 separate Soroban
events**, each with a 2-tuple topic `("swap", "<field_name>")` and a
one-value body:

| Topic[0] | Topic[1] | Body type |
| -------- | -------- | --------- |
| `swap` | `sender` | Address |
| `swap` | `sell_token` | Address |
| `swap` | `offer_amount` | i128 |
| `swap` | `actual received amount` | i128 (note: spaces in key) |
| `swap` | `buy_token` | Address |
| `swap` | `return_amount` | i128 |
| `swap` | `spread_amount` | i128 |
| `swap` | `referral_fee_amount` | i128 |

The same N-events-per-action pattern services Phoenix's
liquidity-management events (Task #27, contracts/pool/src/contract.rs
lines 346-355 / 501-508) and the per-pool stake contract
(contracts/stake/src/contract.rs lines 165-167 / 196-198):

| Action | Topic[0] | Events | Required fields |
| ------ | -------- | ------ | --------------- |
| swap | `swap` | 8 | sender, sell_token, offer_amount, actual received amount, buy_token, return_amount, spread_amount, referral_fee_amount |
| provide_liquidity | `provide_liquidity` | 5 | sender, token_a, token_a-amount, token_b, token_b-amount |
| withdraw_liquidity | `withdraw_liquidity` | 4 (+1 optional) | sender, shares_amount, return_amount_a, return_amount_b (+ `auto unbonded` when the withdrawal also unbonds) |
| bond | `bond` | 3 | user, token, amount |
| unbond | `unbond` | 3 | user, token, amount |

`provide_liquidity` and `withdraw_liquidity` write to the
`phoenix_liquidity` hypertable (migration 0044). `bond` and `unbond`
write to `phoenix_stake_events` (also 0044). The `withdraw_liquidity`
optional 5th `auto unbonded` event is recognised so it doesn't fall
into `ErrUnknownField`, but is intentionally discarded — the stake
contract's `unbond` event carries the same data through a more
authoritative channel.

To reconstruct each action we **must group its N events** by
`(ledger, tx_hash, op_index)` and assemble them into a single
record. This is the third event-correlation shape our consumer fleet
handles:

| Shape | Example | Events per trade |
| ----- | ------- | ---------------- |
| 1-event | Aquarius | one `trade` event carries everything |
| 2-event | Soroswap | `swap` + `sync` (Q1) |
| 8-event | **Phoenix** | one event per field |

## Mainnet addresses (verified on-chain)

| Contract | Address |
| --- | --- |
| Factory | `CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI` |
| Multihop | `CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G` |
| XLM SAC (Phoenix-specific) | `CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC` |

Pools (assumed stable):

| Pair | Pool |
| --- | --- |
| PHO / USDC | `CD5XNKK3B6BEF2N7ULNHHGAMOKZ7P6456BFNIHRF4WNTEDKBRWAE7IAA` |
| XLM / PHO | `CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH` |
| XLM / USDC | `CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX` |
| XLM / EURC | `CBISULYO5ZGS32WTNCBMEFCNKNSLFXCQ4Z3XHVDP4X4FLPSEALGSY3PS` |
| USDC / VEUR | `CDQLKNH3725BUP4HPKQKMM7OO62FDVXVTO7RCYPID527MZHJG2F3QBJW` |

VEUR (tokenised EUR) and EURC (Circle euro) both useful for FX
coverage. PHO is Phoenix's governance token.

## Quirks

### Q1 — 8-event correlation window

The decoder buffers by `(ledger, tx_hash, op_index)` and waits for
all 8 expected fields. Two natural edge cases:

- **Partial set at page boundary.** An RPC page ends between the
  5th and 6th event; the 6–8th events land in the next page. Our
  buffer persists across pages, so the trade reconstructs
  correctly.
- **Event emission order.** The contract emits events in a specific
  order but we do NOT rely on it — the decoder identifies each
  event by topic[1] (the field name) and populates the matching
  slot, regardless of arrival order.

Out-of-order arrival is unlikely on-chain but easy to synthesise in
tests, so we prove robustness there.

### Q2 — Key "actual received amount" has embedded spaces

Phoenix's field-name symbol literally contains spaces (`"actual
received amount"`). This is legal SCVal symbol syntax but unusual.
The decoder byte-matches the full SCVal blob — nothing special
required — but it's worth knowing when reading bug reports.

### Q3 — QuoteAmount is `return_amount`, NOT `actual received amount`

Verified against the pool contract's `do_swap` and confirmed
byte-for-byte against the mainnet lake:

- `offer_amount` — the base the taker sells (input).
- `actual received amount` — `balance_after − balance_before` of the
  **sell (input)** token, i.e. the amount the POOL received of
  `sell_token`. For fee-less tokens it is **byte-identical to
  `offer_amount`**; it is NOT an output amount.
- `return_amount` — `compute_swap.return_amount`, already **net of
  protocol commission + referral fee**, and is exactly the `buy_token`
  amount transferred to the sender.

So `canonical.Trade.QuoteAmount = return_amount` (the `buy_token` the
taker actually received). The earlier decoder used
`actual received amount` — which, because it equals `offer_amount`,
made every Phoenix trade `base_amount == quote_amount` and collapsed
all Phoenix prices to ~1:1. **Fixed 2026-07-07; a historical Phoenix
trade re-derive is required to correct rows written before the fix.**
`spread_amount` / `referral_fee_amount` are emitted but not surfaced
(no `Fee` field on `canonical.Trade`).

### Q4 — Multihop expands to N×8 events

A Phoenix multihop swap passes through N pools and emits 8 events
per pool — so 16 events for a 2-hop, 24 for a 3-hop. A router
multihop emits all N swaps within a **single** op_index (the router
op), so op_index alone does NOT separate them. The decode buffer
emits-and-clears each 8-field swap before the next, so each swap's
first-field `event_index` is distinct; we fan the per-swap trade
op_index out on that event_index (`canonical.FanoutOpIndex`) so the
N trades for one multihop don't collide on the trades PK
(ADR-0033, same as aquarius/comet/soroswap). See `RawSwap.EventIndex`
in `decode.go`.

### Q5 — Two on-wire swap shapes (String 8-event vs Symbol/Map single-event)

Phoenix pools upgrade in place ([contract-schema-evolution](../../../docs/architecture/contract-schema-evolution.md)),
and the newer pool WASM (first seen on mainnet 2026-07-02, pool
`CBENABXP…`) emits a swap as a **single** `ScvSymbol("swap")` event
whose body is an `ScvMap` of all 8 fields — Symbol keys spelled with
underscores (`actual_received_amount`), not the legacy spaced String
(`actual received amount`). Both shapes are decoded:

| Shape | topic[0] | Events/swap | Body | Decoder |
| --- | --- | --- | --- | --- |
| Legacy | `ScvString("swap")` (disc 14) | 8 (one per field) | scalar per event | `decodeSwap` (8-event buffer) |
| Map | `ScvSymbol("swap")` (disc 15) | 1 | `ScvMap` | `decodeSwapMap` (no buffer) |

`classifyAny` routes on the topic shape; both reconstruct the same
`canonical.Trade` (QuoteAmount = `return_amount`, per Q3). Map-schema
pools are gated via `MainnetMapPools`; because gating is by contract
identity, a curated String pool that upgrades to the Map shape in
place is already covered.

**On the `pool_stable` contract:** the current
`contracts/pool_stable/src/contract.rs` swap emits a 6-field String
*subset* (no `actual received amount`, no `referral_fee_amount`),
which would orphan under the 8-field buffer. **No pool with that
schema is deployed on mainnet** — the factory
`("create","liquidity_pool")` walk (13 pools, 2026-07-06) yields only
8-field String pools plus the new Map-schema pool. If a `pool_stable`
instance is ever deployed, add a 6-field completeness variant then;
until a real fixture exists we do not ship a speculative decoder.

## File layout (five-file convention)

| File | Purpose |
| --- | --- |
| `README.md` | this file |
| `events.go` | 8 field-name constants + their SCVal topic constants + mainnet addresses + errors |
| `decode.go` | 8-event correlation buffer + single-Trade emission, decoded via `internal/scval` |
| `consumer.go` | the in-memory 8-event correlation buffer + `Event` wrapper the dispatcher seam emits. (Historical name; does not implement the legacy `consumer.Source` orchestrator interface — production routing is via `dispatcher.Decoder`.) |
| `dispatcher_adapter.go` | topic-match + decode registration with `internal/dispatcher` — the production seam |
| `decode_test.go`, `source_test.go`, `real_fixture_test.go` | unit + happy-path-and-orphan + real-mainnet-fixture tests |

## Status

Production for volatile (constant-product) pools — swaps + liquidity
management + LP staking. The 8-event swap, 5-event
`provide_liquidity`, 4-event `withdraw_liquidity`, and 3-event
`bond`/`unbond` correlation buffers all decode against real
fixtures and write to `trades`, `phoenix_liquidity`, and
`phoenix_stake_events` respectively. The 5th `auto unbonded`
optional event on withdraws is recognised + discarded
(intentionally).

Both on-wire swap shapes are decoded (Q5): the legacy 8-event
`ScvString` schema (`decodeSwap`) and the newer single-event
`ScvSymbol("swap")` + `ScvMap` schema (`decodeSwapMap`, gated via
`MainnetMapPools`). Liquidity decoders cover both pool WASMs'
identical `provide_liquidity` / `withdraw_liquidity` String shapes.

Swap `QuoteAmount` is `return_amount` (the output the taker received),
corrected 2026-07-07 from the earlier `actual received amount` which
equals the input and produced `base == quote` (Q3). A historical
Phoenix trade re-derive is required to fix pre-2026-07-07 rows.

### Historical fill

Granular-coverage mission: once `soroban_events` (ADR-0029) has
been backfilled across the Soroban era, populate
`phoenix_liquidity` + `phoenix_stake_events` for the historical
range via `INSERT … SELECT FROM soroban_events WHERE topic_0_sym IN
('provide_liquidity','withdraw_liquidity','bond','unbond')`, fed
through the per-action correlation buffer the same way the live
ingest does. Pending the per-WASM-hash decoder audit log
([docs/operations/wasm-audits/phoenix.md](../../../docs/operations/wasm-audits/phoenix.md))
being extended to cover the liquidity + stake field sets — current
audit only enumerates the 8 swap-field strings; we need to confirm
both WASM hashes also include the literal `provide_liquidity` /
`withdraw_liquidity` / `bond` / `unbond` symbols.

## `withdraw_rewards` / `distribute_rewards` — HANDLED (ROADMAP #89, 2026-07-10)

A read-only lake topic census against the gated pool + stake-contract
set found two real topic[0] action names `classifyAny` didn't
recognize: `withdraw_rewards` (40 events) and `distribute_rewards`
(18 events) — a rewards-distribution surface distinct from
`bond`/`unbond`, closer kin to `phoenix_stake_events` than a new
table. Real-lake-bytes verified (ledgers 53588319 / 53587626, stake
contracts `CBRGNWGAC25…` / `CAF3UJ45ZQJ…`):

- `withdraw_rewards` — a **2-field-event** action (unlike the census's
  initial guess, no `amount` field was ever missing from the sample —
  every real instance across 10+ occurrences carries exactly `user` +
  `reward_token`, no amount): `("withdraw_rewards","user")` → Address
  (claimant), `("withdraw_rewards","reward_token")` → Address (the
  reward SAC). Decoded via a 2-field correlation buffer
  (`RawWithdrawRewards`, same pattern as `RawStake`).
- `distribute_rewards` — a **single-field, pool-wide** announcement:
  `("distribute_rewards","asset")` → Address (the distributed reward
  SAC). No `user` field on the wire (verified across every real
  sample) — decoded directly from the one event, no buffer needed.

Neither event carries an amount. The paid-out / distributed amount
surfaces on the reward token's own SEP-41 `transfer` event, emitted
as a **separate event in the same op** (verified on both real
samples: `event_index+1` immediately after the stake-contract
field-events) — a SAC contract event, not a stake-contract
field-event, so correlating it is out of scope here (would need a
cross-decoder join against `sep41_transfers` on `tx_hash`+`op_index`).

Both land in `phoenix_stake_events` (reusing the table, not adding
one): `lp_token` is REPURPOSED to carry the reward-token /
distributed-asset address (not an LP share token, for these two
actions); `user_addr` is NULL for `distribute_rewards`; `amount` is
NULL for both (migration 0098 dropped the two columns' `NOT NULL` and
widened the `action` CHECK — additive, old-binary-safe). Decoders:
`decodeWithdrawRewards` / `decodeDistributeRewards` in `decode.go`.
Real-lake-bytes golden tests: `rewards_test.go`.

Separately: a handful of rows in the same census showed an empty
decoded topic[0] alongside a populated topic[1] field name (`user`,
`reward_token`, `token_a`/`token_b`, `amount`, `token`, `asset`,
`shares_amount`, `return_amount_a`/`return_amount_b`, and three
migration-log strings) at non-trivial counts (up to ~5,300). This
looks like either a `contract_events_daily` 2-topic-only census
artifact or a genuinely wider topic arity (index > 2) this pass
didn't decode — flagged as ambiguous rather than asserted as a new
gap; a raw-table, multi-topic-index pull would resolve it.
