---
title: Aquarius WASM-history audit
last_verified: 2026-11-26
status: ratified — v2 per-cohort walk complete
source: aquarius
backfill_safe: true
---

# Aquarius WASM audit

Audit log for the `aquarius` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

> **2026-05-03 update — v2 per-cohort walk folded in.** The
> 2026-04-30 wide-net r1 walk inventoried all 313 mainnet
> Aquarius pools: `Phase 2 results — Cohort A` (168 never-upgraded
> pools on 3 WASMs) and `Phase 2 results — Cohort B` (145
> upgraded pools across a 5-WASM upgrade chain).
> Verdict: every WASM in both cohorts is built from the same
> `liquidity_pool_events` crate and emits the audited
> `(Symbol("trade"), tokenIn, tokenOut, user)` topic with the same
> 3-tuple body (shared-import topology + binary-string scans on every hash).
>
> **2026-05-01 update.** The three pool-template hashes cited
> below (`8875f0c770fb26d3…`, `ae0da5a84b15805c…`,
> `f1077e0b77da5e62…`) are **correct and currently active**:
> they govern 168 Aquarius pools that have never upgraded.
> An earlier draft of `r1-walk-2026-05-01.md` flagged them "stale"
> because the wasm-history walker only emits *transitions* — a walker
> artefact, not doc-rot.

## Status

**Ratified 2026-04-29.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same PR as this
audit. All 313 mainnet aquarius pool contracts enumerated via
the router's `get_pools_for_tokens_range()` view; current
WASMs fetched via `stellar contract fetch` against
mainnet.sorobanrpc.com. **Three unique pool-WASM hashes total**,
all using the shared `liquidity_pool_events::trade()`
emitter — all decoder-compatible by source-import topology +
binary-string verification.

## Contracts under audit

Captured from `internal/sources/aquarius/events.go` (verified
2026-04-23 against stellar.expert + Aquarius docs):

| role | contract |
| --- | --- |
| Router | `CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK` |

No single factory (unlike Soroswap); pool contracts
(volatile / stableswap / concentrated) are deployed
independently and emit the trade events. The Router is the
orchestration entry point. Pools are enumerable from on-chain history.

## Decoder expectations

Captured from `internal/sources/aquarius/{events,decode}.go` at
HEAD as of 2026-04-27.

### Topic structure (trade events)

    topic[0] = ScvSymbol("trade")
    topic[1] = ScvAddress(token_in)   — sold asset
    topic[2] = ScvAddress(token_out)  — bought asset
    topic[3] = ScvAddress(user)       — trader (often the router contract)

Ignored (not trades):

- `deposit_liquidity`
- `withdraw_liquidity`
- `update_reserves`

Classification is **byte-equal** against `TopicSymbolTrade`
(pre-encoded `ScvSymbol("trade")`); a rename to e.g.
`"swap"` would silently drop every trade event.

### Trade body

Verified against `aquarius-amm/liquidity_pool_events/src/lib.rs:122-150`
(soroban-sdk 25.0.2). A Rust tuple, serialized as **`ScvVec` of length 3**, positional:

    body = (
        in_amount  as i128,    // index 0 — sold amount
        out_amount as i128,    // index 1 — bought amount
        fee_amount as i128,    // index 2 — fee, currently unused
    )

**Load-bearing fragility** vs Soroswap: **positional** decoding (Vec), not
by-name (Map). A field reorder in an upgrade silently produces wrong
amounts with no parse error; the tuple order must be verified for every WASM hash.

| body slot | extracted by | invariant |
| --- | --- | --- |
| `[0]` (in_amount) | `scval.AsAmountFromI128` | i128, sign > 0 |
| `[1]` (out_amount) | same | same |
| `[2]` (fee_amount) | same; not used in trade output today | — |

Decoder rejects a non-positive first-two amount or a non-3-tuple
body (a 4-tuple fails the arity check: fail-loud).

### Pool-type orthogonality

Volatile / stableswap / concentrated pools all publish the same
4-topic + 3-tuple `trade` body, so one trade decoder covers all three.
**Concentrated pools are live**: 63 router `add_pool` events, the first at
ledger 62,339,683, announce type `concentrated` (census 2026-11-26). On
`CABOSWXYBLE2UJKYWITFRICR37D4UOB75CNXIH7VI5ZKKL526GFHXAUI` the one
`trade` (ledger 65,678,793) is `[sold, bought, fee]` and moves
`update_reserves` accordingly, but `deposit_liquidity` and
`withdraw_liquidity` are `[liquidity, amount0, amount1]` rather than the
classic `[amount0, amount1, shares]` with the same arity. The decoder
therefore decodes liquidity by the pool's layout from `add_pool` and
refuses a pool whose layout is unknown; see
[docs/protocols/aquarius.md](../../protocols/aquarius.md#concentrated-liquidity-pools)
and the 2026-11-26 event-shape check below.

## Failure modes specific to Aquarius

1. **Topic[0] symbol rename** — `"trade"` → `"swap"` silently drops
   every trade. Verify each WASM emits topic[0] = `"trade"`.
2. **Topic[1]/topic[2] order swap** (sold ↔ bought) — direction
   inverts; undetectable by the decoder, needs per-WASM source review.
3. **Body tuple field reorder** (`(out, in, fee)` instead of `(in, out, fee)`) — arity passes,
   positional extraction gives wrong amounts. **No automated detection possible.**
4. **Body tuple length change** — 4-tuple or 2-tuple trips the arity
   check; every trade dropped (fail-loud).
5. **i128 → u128 amount type swap** — `scval.AsAmountFromI128` is
   strict; errors per trade. Unlikely.
6. **New pool type with a reordered or extended body** — concentrated
   pools reorder `deposit_liquidity` / `withdraw_liquidity` at the same arity (above). Liquidity
   decoding is keyed on the pool's announced layout, but `trade` and
   `update_reserves` are not, so a new pool type still needs a new audit
   entry.
7. **User topic moved or removed** — topic[3] is
   `Address(user)`; removal changes arity 4 → 3 (fail-loud).

## WASM timeline

Output from `stellarindex-ops wasm-history` for the **router**
(CBQDHNBF…) over the post-Soroban window — full archive on r1,
walked 2026-04-29:

```json
{
  "contract": "CBQDHNBF…",
  "ranges": 6 distinct WASM hashes (router upgrades — informational only)
}
```

The **router's 6 hashes are not decoder-relevant**: the decoder
targets `Symbol("trade")` events emitted by per-pool contracts,
not the router's own `Symbol("swap")` events (multi-token / multi-pool
aggregation shape). The router's interface evolution
(governance fields, upgrade-flow methods, protocol-fee admin) is
recorded for completeness only.

### Pool enumeration (decoder-relevant)

All mainnet pools enumerated via router's `get_pools_for_tokens_range(start, end)`
view (paginated 20 token-sets per call; 287
token-sets total) on 2026-04-29 against mainnet.sorobanrpc.com:

- **313 unique pool addresses** across 287 token-sets.
- Per-pool current WASM hashes via `stellar contract
  fetch --id <pool>` + sha256.

### Per-pool WASM uniqueness

Three unique WASM hashes total across all 313 pools:

| pool count | WASM hash (first 16) | pool type (per binary strings) |
| --- | --- | --- |
| 267 (85%) | `ae0da5a84b15805c` | volatile / `StandardLiquidityPool` (`constant_product`) |
| 40 (13%) | `f1077e0b77da5e62` | `StableswapLiquidityPool` |
| 6 (2%) | `8875f0c770fb26d3` | rewards-enhanced variant |

## Phase 2 results — Cohort A: never-upgraded pools (168 instances)

Resolved via Soroban-RPC current-state on 2026-04-30 (the walker
emits transitions only; these contracts have none). Three
unique WASM hashes, all currently active:

| Hash (first 16) | Pool count | Pool type (per binary strings) |
| --- | --- | --- |
| `ae0da5a84b15805c` | 149 | volatile / `ConstantProductLiquidityPool` |
| `f1077e0b77da5e62` | 13 | `StableswapLiquidityPool` |
| `8875f0c770fb26d3` | 6 | rewards-enhanced volatile variant |

All three disassemble cleanly to expose the Aquarius pool API
(`deposit`, `estimate_swap`, `get_reserves`, `get_pools_plane`,
`get_rewards_info`). Bytes preserved at
`evidence/r1-walk-2026-05-01/wasm-bytes/{ae0da5a8…,f1077e0b…,8875f0c7…}.wasm`
on r1.

## Phase 2 results — Cohort B: upgraded pools (145 instances)

From walker transitions in the r1 walk over
ledgers [50,457,424, 62,249,727], in deployment order:

| Order | Hash (first 16) | Pool count (snapshot 2026-04-30) | First observation |
| --- | --- | --- | --- |
| 1 (oldest) | `b54ba37b…` | 97 (now downstream-superseded) | ~L52,700,000 |
| 2 | `2d770946…` | 70 (now downstream-superseded) | mid-2024 |
| 3 | `7cecf23b…` | 36 (now downstream-superseded) | further iteration |
| 4 (most-current) | `a1629dcd…` | 118 (current dominant variant) | L58M+ |
| 5 (rolling out) | `4f080d24…` | 18 (rolling out) | L58M+ |

"Pool count" is cumulative (pools that ever held the hash), so rows
overlap. **At any snapshot, every pool is on exactly one of these five hashes.**
WASM bytes SHA-256-verified at
`evidence/r1-walk-2026-05-01/wasm-bytes/{b54ba37b…,2d770946…,7cecf23b…,a1629dcd…,4f080d24…}.wasm`
on r1; per-pool transition timeline in the walk's
JSONL output (`/tmp/walk-checkpoint/` on r1).

**Decoder verdict per hash.** All five Cohort B WASMs are
built from the same `liquidity_pool_events` crate as Cohort A
(`strings` + topology grep against the upstream
Cargo workspace). A byte search of the committed WASMs finds `trade`,
`deposit_liquidity` and `withdraw_liquidity` in all five, but
`update_reserves` only in `a1629dcd…`: `b54ba37b…`, `2d770946…`,
`7cecf23b…` and `4f080d24…` cannot emit it. A replay over pools while they
ran those four hashes writes trade and liquidity rows but no
`aquarius_reserves` rows; that gap is a property of the builds, not a decoder
or projector fault. Their trade and liquidity body shapes have not been
lake-checked (no fixture under `test/fixtures/aquarius/<hash>/`).

**`CAVLP5DH…` is not the Aquarius router.** The walk's input list
mislabelled it; it is a Reflector testnet oracle, absent on mainnet
(hence `ranges: null`), per
[walker-investigation-2026-05-01.md §2](walker-investigation-2026-05-01.md).
The real router (`CBQDHNBF…`) is walked under [WASM timeline](#wasm-timeline);
nothing is outstanding.

## Per-hash review findings

| hash (first 16) | cohort | role | active pools (2026-04-30) | reviewer | finding |
| --- | --- | --- | --- | --- | --- |
| `ae0da5a84b15805c` | A | volatile pool (dominant never-upgraded) | 149 | maintainer@2026-04-29 | matches current decoder |
| `f1077e0b77da5e62` | A | stableswap pool | 13 | maintainer@2026-04-29 | matches current decoder |
| `8875f0c770fb26d3` | A | rewards-enhanced variant | 6 | maintainer@2026-04-29 | matches current decoder |
| `b54ba37b…` | B | upgrade chain step 1 (oldest) | superseded by step 4 | maintainer@2026-04-30 | trade/liquidity topics present; no `update_reserves` (no reserve rows) |
| `2d770946…` | B | upgrade chain step 2 | superseded by step 4 | maintainer@2026-04-30 | trade/liquidity topics present; no `update_reserves` (no reserve rows) |
| `7cecf23b…` | B | upgrade chain step 3 | superseded by step 4 | maintainer@2026-04-30 | trade/liquidity topics present; no `update_reserves` (no reserve rows) |
| `a1629dcd…` | B | upgrade chain step 4 (most-current) | 118 | maintainer@2026-04-30 | matches current decoder |
| `4f080d24…` | B | upgrade chain step 5 (rolling out; ordering after step 4 unverified, and it lacks `update_reserves` that step 4 has) | 18 | maintainer@2026-04-30 | trade/liquidity topics present; no `update_reserves` (no reserve rows) |

### Source-of-truth: shared event emitter

All three pool types — `liquidity_pool` (volatile),
`liquidity_pool_stableswap`, `liquidity_pool_concentrated` — `use
liquidity_pool_events::Events as PoolEvents` and dispatch to
`LiquidityPoolEvents::trade()` in
`liquidity_pool_events/src/lib.rs:122`, the SOLE
emitter of `Symbol("trade")` events in the aquarius
codebase:

    topic = (Symbol::new(e, "trade"), token_in, token_out, user)
    body  = (in_amount as i128, out_amount as i128, fee_amount as i128)

Source-import topology verified across all three pool-type packages on 2026-04-29.

### Binary-string verification

Each of the 3 pool WASMs scanned for the 4 event-name
strings:

| WASM | `trade` | `update_reserves` | `deposit_liquidity` | `withdraw_liquidity` |
| --- | --- | --- | --- | --- |
| `ae0da5a84b15805c` | ✓ | ✓ | ✓ | ✓ |
| `f1077e0b77da5e62` | ✓ | ✓ | ✓ | ✓ |
| `8875f0c770fb26d3` | ✓ | ✓ | ✓ | ✓ |

All 3 include `trade` + the 3 non-trade event
names in their data sections.

## Per-hash check — 22 more hashes, 2026-10-08

`projected-rebuild -source aquarius` refused on 22 hashes that registry
contracts ran and `audited_wasm.json` did not list: 12 pool builds and 10
builds of the router (`CBQDHNBF…`). Bytes came from
`stellar.ledger_entries_current` (`entry_type = 'contract_code'`, key =
LedgerKey CONTRACT_CODE + hash, base64), the same query as the phoenix audit;
each blob was byte-searched on r1 for the literals the decoder watches
(`internal/sources/aquarius/events.go`):

- pool: `trade`, `deposit_liquidity`, `withdraw_liquidity`, `update_reserves`;
- router: `config_rewards`, `set_rewards_config`.

All 10 router builds carry both router literals (35 – 47 KB). 8 pool builds
carry all four pool literals. 4 pool builds (`29eb1047…`, `2e6f1dae…`,
`64ef0bc6…`, `f5bdd7c4…`) carry `trade`, `deposit_liquidity` and
`withdraw_liquidity` but not `update_reserves`. Those 4 were checked against
the lake instead: every successful event their contracts emitted while
running the build (`stellar.contract_events`, grouped by topic and body
length) has the audited shape. `trade` is 4 topics with a 3×i128 body
(101,631 events); `deposit_liquidity` and `withdraw_liquidity` carry N+1
amounts for N token topics, which `decodeLiquidity` requires and fails loud
on otherwise. They never emit `update_reserves`, so a replay writes no
reserve rows for their windows; nothing is misread.

| hash | role | contracts | first ledger |
| --- | --- | --- | --- |
| `2c2b3c97…` | router | 1 | 52,728,375 |
| `a67ad771…` | router | 1 | 53,552,355 |
| `26c49501…` | router | 1 | 55,363,489 |
| `8cf10d14…` | router | 1 | 56,505,099 |
| `b04880df…` | router | 1 | 57,711,543 |
| `2491b9b6…` | router | 1 | 58,357,191 |
| `8844a760…` | router | 1 | 58,786,777 |
| `1c64fa2c…` | router | 1 | 62,338,092 |
| `06f4207b…` | router | 1 | 62,891,025 |
| `c99539b0…` | router | 1 | 64,735,058 |
| `bb793740…` | pool | 27 | 57,697,718 |
| `3c030c6e…` | pool | 40 | 58,786,852 |
| `baabbdd1…` | pool | 267 | 58,786,917 |
| `1d728cc0…` | pool | 13 | 62,433,116 |
| `12fca5a7…` | pool | 37 | 62,875,836 |
| `d691135a…` | pool | 275 | 64,735,740 |
| `22dff724…` | pool | 45 | 64,735,965 |
| `155a17b9…` | pool | 41 | 64,736,662 |
| `f5bdd7c4…` | pool (no `update_reserves`) | 3 | 52,730,973 |
| `29eb1047…` | pool (no `update_reserves`) | 11 | 53,552,612 |
| `64ef0bc6…` | pool (no `update_reserves`) | 19 | 55,363,569 |
| `2e6f1dae…` | pool (no `update_reserves`) | 25 | 56,505,116 |

A string check does not see topic order or body shape. All 22 are in
`audited_wasm.json`.

## Per-hash check — router upgrade, 2026-11-26

The router (`CBQDHNBF…`) was upgraded in place to a new build, which the daily
`stellarindex-ops wasm-drift` run reported as drift. Same method as the
2026-10-08 check above, read-only against the production lake.

| hash | role | contracts | first ledger |
| --- | --- | --- | --- |
| `2f8e7d7f…` | router | 1 | 65,663,788 |

`first ledger` is the ledger the router began running the build, here its
`apply_upgrade` event. The contract_code entry was created earlier, at ledger
65,526,037 (2026-11-17), and the router emitted `commit_upgrade` at ledger
65,560,332.

- Bytes: the blob in `stellar.ledger_entries_current` (`entry_type =
  'contract_code'`, base64 length 67,804) was byte-searched for the router
  literals the decoder watches: `config_rewards` and `set_rewards_config` are
  both present.
- Shapes: every event the router emitted over ledgers 65,500,000 – 65,678,466,
  a window spanning the upgrade, has exactly one (topic count, body length)
  shape per topic, so no shape changed across the upgrade.

| topic | topics | body length | events |
| --- | --- | --- | --- |
| `swap` | 3 | 232 | 62,896 |
| `claim` | 3 | 152 | 322 |
| `deposit` | 3 | 168 | 191 |
| `withdraw` | 3 | 168 | 37 |
| `config_rewards` | 2 | 112 | 1,082 |
| `add_pool` | 2 | 188 | 6 |
| `commit_upgrade` | 1 | 72 | 1 |
| `apply_upgrade` | 1 | 72 | 1 |

A second byte-search of the same blob for the other router literals
`add_pool`, `apply_upgrade`, `commit_upgrade`, `pool_gauge_switch_token`,
`set_privileged_addrs`, `commit_transfer_ownership`,
`apply_transfer_ownership` and `emergency` found all of them present.

Fixtures: `test/fixtures/aquarius/2f8e7d7f…/` holds four router events emitted
after `apply_upgrade`: two `add_pool` (ledgers 65,675,280 and 65,675,307) and
two `config_rewards` (ledger 65,667,281, event indexes 1 and 3), plus the two
pool-side `set_rewards_config` events of that transaction described below.
`TestRouterUpgradeFixtures_addPool` and `TestRouterUpgradeFixtures_configRewards`
(`internal/sources/aquarius/router_upgrade_fixture_test.go`) run the decoder
over them and compare against values hand-decoded from the raw XDR, listed
below, so the expected values do not come from the decoder.

Byte offsets in the base64-decoded XDR (big-endian words):

| event | field | offset | hand-decoded value |
| --- | --- | --- | --- |
| `add_pool` 65,675,280 | body Vec header: type `0x10`, present, count | 0 – 11 | count 4 |
| | body[0] pool address (type `0x12`, kind 1, 32 bytes) | 12 – 51 | `CBQ6AGQYE5HCDK7VIDUH6756IFPBTLF2HNQH57DAFMOHK3CVDH4CAMXO` |
| | body[1] symbol | after address | `concentrated` |
| | topic[1] Vec of 2 addresses | 12, 52 | `CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK`, `CB3YA656OYIHU57657I5KGSBRHE5I3OZU4VFC22PYAOANFZHEWNYGAGP` |
| `add_pool` 65,675,307 | body[0] pool address | 12 – 51 | `CABOSWXYBLE2UJKYWITFRICR37D4UOB75CNXIH7VI5ZKKL526GFHXAUI` |
| | body[1] symbol | after address | `concentrated` |
| | topic[1] Vec of 2 addresses | 12, 52 | `CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK`, `CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF` |
| `config_rewards` index 3 | body[0] pool address | 12 – 51 | `CDRRS2SLIP7THFGUZVB4QL2RAM2ZHSDMAD2GL5GEXTKK5AY3DQUX3Y7N` |
| | body[1] u128 (type 9, hi at 56, lo at 64) | 52 – 71 | hi 0, lo 3,316,077 |
| | body[2] u64 (type 5, value at 76) | 72 – 83 | 1,795,666,520 |
| `config_rewards` index 1 | body[0] pool address | 12 – 51 | `CAAJYIESEIKI7KZV67MBSXPCRGKH5WY354WUS55H4UNBD35OSKYPZA7Q` |
| | body[1] u128 | 52 – 71 | hi 0, lo 3,831,913 |
| | body[2] u64 | 72 – 83 | 1,795,666,520 |

The two `add_pool` events announce pools of type `concentrated`; the decoder
registers each announced address in its contract-identity gate with the
`concentrated` liquidity layout and emits no event. The two announced pools are not
`CBTOQ7X7TLDBRHJW4UYUDR7WR4NAYESZA7FYUZVLAN2JLQG5ZXWJJIG6`.

Pool-side anchor: in the same transaction (ledger 65,667,281, `9ec5f48b…99cf`)
each router `config_rewards` is preceded by the pool's own `set_rewards_config`,
emitted by the pool contract rather than the router: event index 0 from
`CAAJYIES…ZA7Q` before router event 1, and event index 2 from `CDRRS2SL…3Y7N`
before router event 3. Both are fixtures
(`set_rewards_config_65667281_9ec5f48b4c10_{0,2}.json`). The pool body is
`Vec[u64 expires_at, u128 amount]` (u64 at offset 12 with its value at 16,
u128 at 24 with hi at 28 and lo at 36, 44 bytes); the router body is
`Vec[address pool, u128 amount, u64 expires_at]`. Corresponding fields:

| field | pool `set_rewards_config` | router `config_rewards` |
| --- | --- | --- |
| pool | emitting contract | body[0] address |
| amount | u128 body[1] | u128 body[1] |
| expires_at | u64 body[0] | u64 body[2] |

Hand-decoded, they match exactly with the same unit, no scaling: pool
`CAAJYIES…` amount 3,831,913 and `CDRRS2SL…` amount 3,316,077, both expiring at
1,795,666,520 (`0x6b07b258`). The test decodes both events with the decoder and
asserts the pool, amount and `expires_at` of each pair are equal, and equal to
the hand-decoded values. The `add_pool` events carry no amount.

This meets the README rule for the `config_rewards` amount: the anchor is a
different contract's event in the same transaction, so a scale or sign change
in the router's u128 alone would break the equality. It does not rule out the
same change in the pool and router together, and it is not a token transfer.

This is a string, shape and six-event decode check only; the decoder was not
re-run against a replay. The hash is in `audited_wasm.json`.

## Event-shape check — concentrated pools on `155a17b9…`, 2026-11-26

`155a17b9929ffb1f9e84bd6ef5c00a4d613c1ab5f4ad4c502d84515250cc2907` was already
in `audited_wasm.json` from the 2026-10-08 string check (41 contracts then).
On 2026-11-26 it runs on 79 contracts (`stellar.contract_instance_changes`):
all 63 router-announced concentrated pools plus 16 the router never
announced. No concentrated pool runs another hash today. Over the 63 pools'
instance history they ran four builds, all in `audited_wasm.json`:

| hash | pools | first – last instance change |
| --- | ---: | --- |
| `8875f0c7…` | 9 | 62,339,683 – 62,475,443 |
| `1d728cc0…` | 13 | 62,433,116 – 62,877,765 |
| `12fca5a7…` | 37 | 62,875,836 – 64,799,639 |
| `155a17b9…` | 63 | 64,736,662 – 65,680,336 |

All four emit the concentrated body order: 400 deposit transactions sampled
from 62,341,446 – 64,577,704 (the three earlier builds) have body[0] equal
to the same-tx `position_update` delta, and 300 withdraws from
62,341,437 – 62,413,470 have body[0] equal to minus it.

**Correction, 2026-11-26.** The 2026-04-29 sign-off of `8875f0c7…`
("rewards-enhanced variant", "matches current decoder", Per-hash review
findings above) and the 2026-10-08 string check of `1d728cc0…`, `12fca5a7…`
and `155a17b9…` passed these builds for a decoder that read every
`deposit_liquidity` / `withdraw_liquidity` body as `[amount_0, amount_1,
shares]`. For these concentrated builds that was wrong: the body is
`[liquidity, amount0, amount1]`. A string check cannot see body order, and the
2026-10-08 lake check of N+1 amounts covered only the four builds without
`update_reserves`. The evidence now is the 400 deposits and 300 withdraws
above plus the fixtures below.

Byte search of the `155a17b9…` blob (base64 length 147,132), 2026-11-26:
every listed literal is present — `position_update`, `pool_state`,
`claim_reward`, `claim_fees`, `set_rewards_config`, `concentrated`, `tick`,
`sqrt_price`, `deposit_liquidity`, `withdraw_liquidity`, `update_reserves`,
`trade`.

Fixtures: `test/fixtures/aquarius/155a17b9…/` holds every `deposit_liquidity`
the two pools emitted (four), each with the same-transaction
`update_reserves`, `position_update` and `pool_state`, and `CABOSWXY…`'s one
`trade` with its `update_reserves` and `pool_state`; the router's third
concentrated `add_pool` (65,675,330) is under `2f8e7d7f…/`.
`TestConcentratedDeposit_fixturesCrossCheckAgainstSameTxEvents` asserts, per
deposit transaction, that body[0] equals the `position_update` delta and that
`update_reserves` equals the running sums of body[1] and body[2];
`TestConcentratedTrade_reserveMovesByBoughtAmount` asserts the trade lowers
reserve[0] by exactly the bought amount. The trade (fee 13 on 12,635 sold, the
pool's `add_pool` announcing 10 bps) is too small to tell the gross and net fee
conventions apart, so it is not in the fee census.

First-build fixtures: `test/fixtures/aquarius/8875f0c7…/` holds three real
`withdraw_liquidity` transactions with their `update_reserves` and
`position_update` — single-sided token1 (62,341,437 `CCV2X5TH…`), single-sided
token0 (62,342,234 `CA4HTZNY…`) and two-sided (62,342,237 `CA4HTZNY…`) — and
`CCV2X5TH…`'s deposit at 62,341,446. All four are before 62,433,116, the
first instance change to any other build, so they ran `8875f0c7…`.
`TestConcentratedDeposit_firstBuildCrossCheck` asserts the deposit's body[0]
equals its `position_update` delta and that reserves rise from the 62,341,437
withdraw by exactly its amounts. No fixture pins `1d728cc0…` or `12fca5a7…`:
the sampled rows do not say which build each pool ran at a given ledger.
`TestConcentratedWithdraw_fixturesCrossCheckAgainstSameTxEvents` asserts the
hand-decoded values and body[0] == −(`position_update` delta);
`TestConcentratedWithdraw_reservesFallByAmounts` asserts `CA4HTZNY…`'s
reserves fall between its two consecutive withdraws by exactly the second
one's body[1] and body[2]. The same body[0] == −delta relation holds in all
300 withdraw transactions sampled from 62,341,437 – 62,413,470.

Not established: what the 16 unannounced contracts on this hash are.

## Caveats

- **Per-pool WASM history walked end-to-end as of 2026-04-30.**
  ~~v2 follow-up~~: ✅ done — the wide-net r1 walk captured
  every `update_current_contract_wasm` transition for all 313
  pools across the [50,457,424, 62,249,727] range (Cohort A / B above).
  The shared-emitter argument applies to any new
  pool WASM compiled from the aquarius-amm tree.
- **New pools deployed after 2026-04-29 not in this audit.**
  Re-run the enumeration when extending `last_verified`.
- **Pool type gates liquidity decoding only.** Classification doesn't gate on
  pool type or WASM hash — it matches
  topic[0] = Symbol("trade") regardless of variant. All
  pool types in production (including the 6
  rewards-enhanced pools) emit the same trade-event shape, as does the one
  `trade` observed on a concentrated pool. `deposit_liquidity` / `withdraw_liquidity` are decoded
  by the pool's layout (Pool-type orthogonality, above).

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/aquarius/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/ingest-pipeline.md#contract-schema-evolution`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["aquarius"].BackfillSafe`
- Upstream contract source: `https://github.com/AquaToken/aquarius-amm`
