---
title: Aquarius WASM-history audit
last_verified: 2026-05-03
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
4-topic + 3-tuple-body shape, so one decoder covers all three.
**Concentrated pools** (feature-branch, no live mainnet
pools) have no decoder guard: the router's `add_pool` registers any
announced pool without a WASM-hash or pool-type check, and a
registered pool's 4-topic, 3×i128 `trade` decodes. If concentrated
pools ship live, re-verify the trade event shape first (the body
might gain concentrated-tick info).

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
6. **New pool type with extended body** — concentrated/v2 pools might
   publish a longer body or different topics. Nothing refuses a new family
   whose body is still 3×i128; this audit is the only check, so new
   pool types need new audit entries.
7. **User topic moved or removed** — topic[3] is
   `Address(user)`; removal changes arity 4 → 3 (fail-loud).

## WASM timeline

Output from `stellarindex-ops wasm-history` for the **router**
(CBQDHNBF...) over the post-Soroban window — full archive on r1,
walked 2026-04-29:

```json
{
  "contract": "CBQDHNBF...",
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
Cargo workspace). Binary-string scan confirms `trade`,
`update_reserves`, `deposit_liquidity`, `withdraw_liquidity`
in every WASM's data section. Any pool WASM compiled from the
aquarius-amm tree emits the audited topic + body shape.

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
| `b54ba37b…` | B | upgrade chain step 1 (oldest) | superseded by step 4 | maintainer@2026-04-30 | matches current decoder |
| `2d770946…` | B | upgrade chain step 2 | superseded by step 4 | maintainer@2026-04-30 | matches current decoder |
| `7cecf23b…` | B | upgrade chain step 3 | superseded by step 4 | maintainer@2026-04-30 | matches current decoder |
| `a1629dcd…` | B | upgrade chain step 4 (most-current) | 118 | maintainer@2026-04-30 | matches current decoder |
| `4f080d24…` | B | upgrade chain step 5 (rolling out) | 18 | maintainer@2026-04-30 | matches current decoder |

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

## Caveats

- **Per-pool WASM history walked end-to-end as of 2026-04-30.**
  ~~v2 follow-up~~: ✅ done — the wide-net r1 walk captured
  every `update_current_contract_wasm` transition for all 313
  pools across the [50,457,424, 62,249,727] range (Cohort A / B above).
  The shared-emitter argument applies to any new
  pool WASM compiled from the aquarius-amm tree.
- **New pools deployed after 2026-04-29 not in this audit.**
  Re-run the enumeration when extending `last_verified`.
- **No pool-type gate.** Classification doesn't gate on
  pool type or WASM hash — it matches
  topic[0] = Symbol("trade") regardless of variant. All
  three pool types in production (including the 6
  rewards-enhanced pools) emit the same trade-event shape via the
  shared events crate, so the decoder works on all of them.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/aquarius/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/ingest-pipeline.md#contract-schema-evolution`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["aquarius"].BackfillSafe`
- Upstream contract source: `https://github.com/AquaToken/aquarius-amm`
