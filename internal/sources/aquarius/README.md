# Aquarius connector

Ingests Aquarius trade events from Soroban pool contracts. The live
decoder is topic-driven and stateless: token identities are carried in
the event topics, so the dispatcher adapter does not need router reads
or a pool-token cache. See the protocol verification page:
[`docs/protocols/aquarius.md`](../../../docs/protocols/aquarius.md).

## What this ingests

Aquarius has multiple pool shapes. The decoder handles the `trade`
event (pricing), the liquidity/reserves surface, the rewards-gauge
subsystem, and the router/pool governance surface (all three latter
groups are analytics — never VWAP inputs):

1. **Volatile** — constant-product pools (2 tokens).
2. **Stableswap** — stablecoin-oriented pools (2/3/4 tokens).
3. **Future variants** — any pool/event shape that does not match a
   known topic contract is rejected until explicitly audited and added.

The decoder emits one `canonical.Trade` per accepted `trade` event,
one `ReservesEvent` / `LiquidityEvent` per accepted reserves / liquidity
event, one `RewardsEvent` per accepted rewards-gauge event, and one
`AdminEvent` per accepted governance/upgrade event. Unlike Soroswap,
there is no swap+sync correlation buffer.

## Events we care about

| Event | Topic 0 | Carries | Lands in |
| --- | --- | --- | --- |
| `trade` | `trade` | `token_in`, `token_out`, `user` in topics; sold/bought/fee amounts in body | `trades` (source=aquarius) |
| `update_reserves` | `update_reserves` | POST-STATE reserve vector `Vec<i128>` in body (no token addresses — positional, canonical pool token order) | `aquarius_reserves` (fanned one row per token position) — the first real Aquarius TVL/liquidity-depth signal |
| `deposit_liquidity` | `deposit_liquidity` | per-token amounts + LP shares minted; token addresses in topics `[Symbol, token_0…token_{n-1}]`, body `Vec<i128>=[amount_0…amount_{n-1}, shares]` | `aquarius_liquidity` (action=deposit, fanned per token) |
| `withdraw_liquidity` | `withdraw_liquidity` | same wire shape as deposit; trailing body element is shares BURNED | `aquarius_liquidity` (action=withdraw, fanned per token) |
| rewards-gauge (12 kinds — see below) | `pool_state`, `claim_reward`, `set_rewards_config`, `position_update`, `deposit`, `claim_fees`, `rewards_gauge_claim`, `claim`, `rewards_gauge_schedule_reward`, `set_rewards_state`, `rewards_gauge_add`, `config_rewards` | per-kind — see `decode_rewards.go` | `aquarius_rewards_events` (migration 0099, one `event_kind` per row) |
| governance/upgrade (8 kinds — see below) | `apply_upgrade`, `commit_upgrade`, `set_privileged_addrs`, `apply_transfer_ownership`, `commit_transfer_ownership`, `enable_emergency_mode`, `disable_emergency_mode`, `pool_gauge_switch_token` | per-kind — see `decode_admin.go` | `aquarius_admin` (migration 0100, one `event_kind` per row) |

All four flow-event kinds (`trade`/`update_reserves`/
`deposit_liquidity`/`withdraw_liquidity`) plus the eleven pool-scoped
rewards-gauge kinds are gated IDENTICALLY on contract identity
(ADR-0035/0040, CS-026): they match only when emitted by a REGISTERED
Aquarius pool, so a look-alike cannot inject fabricated reserves any
more than fabricated trades. The governance/upgrade surface (plus the
router-scoped `config_rewards` rewards kind) gates on the CANONICAL
ROUTER trust root instead — see "Rewards-gauge + governance topics"
below. Reserves/liquidity/rewards/admin are ADDITIVE analytics —
Aquarius has no published price, so these rows never reach VWAP.

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
| `decode.go` | Aquarius `trade` -> `canonical.Trade`; `update_reserves`/`deposit_liquidity`/`withdraw_liquidity` -> `ReservesEvent`/`LiquidityEvent`; `classify()` — the closed-set topic dispatch every decode file below plugs into |
| `decode_rewards.go` | the twelve rewards-gauge kinds -> `RewardsEvent` (migration 0099) |
| `decode_admin.go` | the eight governance/upgrade kinds -> `AdminEvent` (migration 0100) |
| `dispatcher_adapter.go` | contract-identity-gated dispatcher-facing decoder |
| `consumer.go` | `consumer.Event` wrappers emitted after decode (`TradeEvent`, `ReservesEvent`, `LiquidityEvent`, `RewardsEvent`, `AdminEvent`) |
| `decode_test.go`, `adapter_test.go`, `source_test.go`, `topic_decoder_reject_test.go`, `real_fixture_test.go`, `liquidity_decode_test.go`, `decode_rewards_test.go`, `decode_admin_test.go`, `gate_rewards_admin_test.go` | unit + reject-path + real-fixture coverage |

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

## ✅ Rewards-gauge + governance topics — decoded (ROADMAP #89, closed 2026-07-10)

The 2026-07-10 topic census (a read-only ClickHouse-lake scan against
the full gated set: 332 pools + the router `CBQDHNBF…`) found 19
real, distinct topics `classify()` did not recognize, plus a 20th
(`config_rewards`, the router-side companion to `set_rewards_config`
— see below) — none errored loudly; they simply never reached a
decoder and weren't counted anywhere. All 20 are now decoded. Every
wire shape below is reverse-engineered directly from real r1
ClickHouse lake bytes (`stellar.contract_events`, captured
2026-07-10) — **not** a cloned Rust source: AquaToken's
`soroban-amm` contract-source repository (the one this file's `trade`
decoder cites) is no longer publicly reachable (its GitHub org shows
zero public repositories as of this audit). Business-meaning field
names beyond what the bytes themselves prove are marked BEST-EFFORT
in `decode_rewards.go` / `decode_admin.go`'s per-function doc
comments — treat them as informative, not authoritative, until the
Aquarius team confirms or the source becomes available again.

**Rewards-gauge subsystem (12 kinds, `aquarius_rewards_events`,
migration 0099)** — lifetime counts at capture time:

| Topic | Count | Table columns |
| --- | ---: | --- |
| `pool_state` | 339,712 | `attributes: {accumulator (u256), checkpoint (i32), value (i128)}` |
| `claim_reward` | 263,673 | `user_address`, `amount`, `attributes.reward_token` |
| `set_rewards_config` | 47,530 | `amount`, `attributes.expires_at` |
| `position_update` | 12,403 | `user_address`, `attributes: {range_from, range_to, delta (SIGNED i128)}` |
| `deposit` (bare — distinct from `deposit_liquidity`) | 7,213 | `user_address`, `attributes: {ref_address, amount_0, amount_1}` |
| `claim_fees` | 5,056 | `user_address` (topic[1], NOT last — the one exception), `attributes: {token_a, token_b, amount_a, amount_b}` |
| `rewards_gauge_claim` | 1,121 | `user_address`, `amount` (u128), `attributes.reward_token` |
| `claim` (bare — distinct from `claim_protocol_fee`) | 168 | `user_address`, `amount` — body is a BARE i128, not a Vec |
| `rewards_gauge_schedule_reward` | 64 | `amount`, `attributes: {reward_token, starts_at, ends_at}` |
| `set_rewards_state` | 25 | `user_address` (admin/manager), `attributes.enabled` (bool) |
| `rewards_gauge_add` | 12 | `attributes: {address_0, address_1}` |
| `config_rewards` (router-side companion to `set_rewards_config`; NOT in the original census, folded in as the 12th kind) | 52,722+ | `amount`, `attributes: {pool, expires_at, refs}` — router-scoped |

**Governance / admin (8 kinds, `aquarius_admin`, migration 0100)** —
lifetime counts at capture time:

| Topic | Count | Table columns |
| --- | ---: | --- |
| `apply_upgrade` | 706 | `target` = hex(new wasm hash) |
| `commit_upgrade` | 705 | `target` = hex(proposed wasm hash), fires before the matching `apply_upgrade` |
| `set_privileged_addrs` | 173 | `attributes: {addr_0, addr_1, addr_2, addr_list}` |
| `apply_transfer_ownership` | 48 | `target` = new role-holder address, `attributes.role` (e.g. `"EmergencyAdmin"`) |
| `commit_transfer_ownership` | 48 | same shape, fires before the matching `apply_transfer_ownership` |
| `enable_emergency_mode` | 35 | bare marker, `Void` body |
| `disable_emergency_mode` | 35 | bare marker, `Void` body |
| `pool_gauge_switch_token` | 31 | `target` = new gauge reward token; 100% router-scoped |

**Gating.** The 11 pool-scoped rewards kinds gate identically to
`trade`/liquidity/reserves (`d.reg.Has`); `config_rewards` and the
8 governance kinds gate on the canonical router trust root
(`d.reg.IsFactory`). Real lake bytes show several governance kinds
also emitted by the FLAGGED parallel router deployment
(`CA7RQDMM…`, see "Verification 2026-07-05" in
`docs/protocols/aquarius.md`) and a small family of as-yet-unidentified
sibling contracts (e.g. `CDWVENDOPYZ…`, `CAEYKKJ5LT…`) that co-occur
with it — those fail-closed exactly like `CA7RQDMM`'s trade events
already do (visible ADR-0033 recognition gap, not silent
mis-attribution), pending Aquarius-team confirmation of what that
contract family is.

**Out of scope (SEP-41 token layer, not this protocol's own event
surface — same exclusion `comet`'s README documents for BPT
`transfer`):** `transfer` (4,035,808), `approve` (17,623), `mint`
(1,665), `burn` (23) — these belong to `internal/sources/sep41_supply`
/ `sep41_transfers`, re-decoding them here would double-count.

Every other census topic (`trade`, `deposit_liquidity`,
`withdraw_liquidity`, `update_reserves`, `reserves_sync`,
`set_protocol_fee`, `claim_protocol_fee`,
`kill_deposit`/`unkill_deposit`/`kill_swap`/`unkill_swap`/
`kill_claim`/`unkill_claim`, `add_pool`) was already handled.

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
