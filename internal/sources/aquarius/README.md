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

## ⚠️ Known gap — rewards-gauge + governance topics are unclassified (ROADMAP #89, 2026-07-10)

A read-only ClickHouse-lake topic census against the full gated set
(332 pools + the router `CBQDHNBF…`) found **20 real, distinct
topics `classify()` does not recognize** — none error loudly today;
they simply never reach a decoder and aren't counted anywhere. This
is the largest gap found in the ROADMAP #89 residual sweep. Exact
counts (lifetime, via `stellar.contract_events`, the authoritative
raw table — NOT the `contract_events_daily` fast-path, which
undercounts, see caveat below):

**Rewards-gauge subsystem — wholly unimplemented (no table exists):**

| Topic | Count |
| --- | ---: |
| `pool_state` | 339,712 |
| `claim_reward` | 263,673 |
| `set_rewards_config` | 47,530 |
| `position_update` | 12,403 |
| `deposit` (bare — distinct from `deposit_liquidity`) | 7,213 |
| `claim_fees` | 5,056 |
| `rewards_gauge_claim` | 1,121 |
| `claim` (bare — distinct from `claim_protocol_fee`) | 168 |
| `rewards_gauge_schedule_reward` | 64 |
| `set_rewards_state` | 25 |
| `rewards_gauge_add` | 12 |

**Governance / admin — wholly unimplemented (no table exists):**

| Topic | Count |
| --- | ---: |
| `apply_upgrade` (router) | 706 |
| `commit_upgrade` (router) | 705 |
| `set_privileged_addrs` | 173 |
| `apply_transfer_ownership` | 48 |
| `commit_transfer_ownership` | 48 |
| `enable_emergency_mode` | 35 |
| `disable_emergency_mode` | 35 |
| `pool_gauge_switch_token` | 31 |

**Out of scope (SEP-41 token layer, not this protocol's own event
surface — same exclusion `comet`'s README documents for BPT
`transfer`):** `transfer` (4,035,808), `approve` (17,623), `mint`
(1,665), `burn` (23) — these belong to `internal/sources/sep41_supply`
/ `sep41_transfers`, re-decoding them here would double-count.

This is squarely "deep work" per ROADMAP #89's carve-out — the
rewards-gauge topics alone imply a new user-position / gauge-state
table (`aquarius_rewards`?), and the governance topics need their own
admin-event table (mirroring `blend_admin`'s pattern). **Not
implemented this session** — flagged for a follow-up ADR/migration
+ a dedicated `add-onchain-source`-style wiring pass, not a mechanical
`classify()` addition. All other census topics (`trade`,
`deposit_liquidity`, `withdraw_liquidity`, `update_reserves`,
`reserves_sync`, `set_protocol_fee`, `claim_protocol_fee`,
`kill_deposit`/`unkill_deposit`/`kill_swap`/`unkill_swap`/
`kill_claim`/`unkill_claim`, `add_pool`) ARE handled.

**Data-quality caveat:** `stellar.contract_events_daily` (the fast
pre-aggregated path `/v1/protocols/{name}` reads) undercounts by
~43% relative to the raw `stellar.contract_events` table for at
least this source (cross-checked on the `trade` topic: 2,286,665 via
the daily rollup vs 4,035,808 via the raw table, same exact contract
set). The counts above are from the raw table. This looks like an
incomplete one-time historical backfill of `contract_events_daily`
(see its DDL comment in `deploy/clickhouse/tier1_schema.sql`) — an
operational finding outside this audit's scope, flagged here because
it means any consumer of the fast rollup (including the protocol
detail page) is currently under-reporting event volume for
long-lived sources.
