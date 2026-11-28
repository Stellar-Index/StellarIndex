---
title: Comet WASM-history audit
last_verified: 2026-11-15
status: ratified — Blend and Comet V1.1 lineages
source: comet
backfill_safe: true
v2_audit: blend.md
---

# Comet WASM audit

Audit log for the `comet` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

> **2026-11-15 update:** the curated set now also includes BLNT/USDC and
> BLNT/XLM pools. Both run the released Comet V1.1 artifact, whose SHA-256 and event interface
> were verified against the v1.1.0 source release. The historical Blend pool remains on its
> separately audited `8abc2891…` lineage.
>
> **2026-05-01 update.** Hash citations cross-checked against the 2026-04-30 r1
> walk; see [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md).

## Status

**Ratified 2026-04-29; extended 2026-11-15.** `BackfillSafe` is `true` in
`internal/sources/external/registry.go`. Blend's historical backstop pool has WASM hash
`8abc28913035c074...`; the BLNT/USDC and BLNT/XLM pools have released Comet V1.1 hash
`d735c3395f595101...`. Decoder compatibility for both lineages was confirmed through interface
inspection and artifact/source verification.

## Contracts under audit

Comet has **no known deployed mainnet factory** (Balancer-v1-style library).
Upstream ships a factory emitting `("LOG", "NEW_POOL")` with the new pool's
address (`factory.rs` in the vendored source); if a mainnet one appears,
factory anchoring per ADR-0035 becomes available. AGENTS.md flagged:

> **Comet uses a shared `("POOL", <event>)` topic across every pool
> contract**, not a per-protocol namespace. The decoder matches by
> topic bytes, not pool contract ID — any pubnet contract that
> deploys Balancer-v1 Comet code will look identical on the wire.

**Correction (2026-07-08 gate, noted 2026-08-03):** since ADR-0035 identity
gating, `Matches()` requires the emitting contract to be in the curated gated
set (`comet.MainnetGatedSet`), so a topic-squatting deployment no longer
attributes; the quote above describes the PRE-gate decoder.

With no factory to enumerate, the audit unit is the **WASM-hash + event-shape
pair**: any pool emitting `("POOL", "swap")` with the expected body decodes
correctly.

| role | contract | WASM hash |
| --- | --- | --- |
| Blend BLND/USDC backstop | `CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM` | `8abc28913035c07411ed5d134e6bfeab4723d97ddd4d1a22a0605d35c94d1a36` |
| BLNT/USDC Comet V1.1 pool | `CDQMIKIHKA5UY3FQJ2UDTLNX3GOPD3GPNZWSPTOBVOHEUMQJQXOPC7Y3` | `d735c3395f59510172cf5cf838823a1389b97cfec6bf24e580e9bd77d2b3e687` |
| BLNT/XLM Comet V1.1 pool | `CA5HX3D47PQZWC2E7VWXCKH4534P2NOJEOIXAWU47MI4QV2FQUYY3FLC` | `d735c3395f59510172cf5cf838823a1389b97cfec6bf24e580e9bd77d2b3e687` |

For the historical Blend pool:

- The mainnet snapshot at L55,261,759 in
  `.discovery-repos/blend-contracts/test-suites/src/mainnet-55261759-snapshot.json`
  has a single `Comet Pool Token` ledger entry, matching this contract.
- Current `stellar contract fetch --id <pool>` returns the same WASM hash: no upgrade since.

For the BLNT pools, the checked-in deployment artifact hashes exactly to the published Comet V1.1
value above. The v1.1.0 source interface exposes the same `SwapEvent`, `JoinEvent`, `ExitEvent`,
`DepositEvent`, and `WithdrawEvent` fields consumed by the decoder.

## Decoder expectations

From `internal/sources/comet/{events,decode}.go` at HEAD 2026-04-27.

### Topic structure (all five POOL events)

Verified 2026-04-23 against `comet-contracts/src/c_pool/event.rs` and
`call_logic/pool.rs:21,184-191`; re-verified 2026-05-26 for the
join/exit/deposit/withdraw additions (920725666):

    topic[0] = ScvSymbol("POOL")
    topic[1] = ScvSymbol("<kind>")
              // "swap" | "join_pool" | "exit_pool" | "deposit" | "withdraw"

    swap body     = ScvMap {
        "caller": Address, "token_in": Address, "token_out": Address,
        "token_amount_in": i128, "token_amount_out": i128,
    }
    join_pool / deposit body = ScvMap {
        "caller": Address, "token_in": Address, "token_amount_in": i128,
    }
    exit_pool body = ScvMap {
        "caller": Address, "token_out": Address, "token_amount_out": i128,
    }
    withdraw body  = ScvMap {
        "caller": Address, "token_out": Address,
        "token_amount_out": i128, "pool_amount_in": i128,
    }

Classification is **byte-equal** against pre-encoded `ScvSymbol` constants.
Since 920725666 the decoder claims all five POOL kinds: `swap` →
`canonical.Trade` (table `trades`), the other four → `LiquidityEvent` (table
`comet_liquidity`, migration 0042). Any other `(POOL, *)` topic (e.g. a future
`set_controller`) is rejected in `Matches` and lands in the dispatcher's GLOBAL
unmatched tally; comet has no per-source orphan reporter, so
`source_orphan_events_total{source="comet"}` never populates (correction
2026-08-03; the alerting signal is the ADR-0033 recognition audit, which
reports a gated-pool event no decoder matches as a recognition gap).

The Soroban port emits **only** these five topics under `POOL`. The
EVM-Balancer-v1 admin events (`bind` / `rebind` / `unbind` / `finalize` /
`gulp` / `set_swap_fee` / `set_controller` / `set_public_swap`) are NOT in the
Stellar port (function absent or storage-only; verified 2026-05-26 against
`contracts/src/c_pool/{comet.rs, token_utility.rs}`). BPT (pool-share)
`transfer` events use the SEP-41 surface and are claimed by
`internal/sources/sep41_supply` when the pool is in scope.

### Body extraction

Every field is pulled **by name** (Map-keyed), like Soroswap (vs Aquarius's
positional Vec): new fields don't break extraction; rename/removal does.

| event | field | extracted by | invariant |
| --- | --- | --- | --- |
| swap | `token_in` | `scval.AsAddressStrkey` | valid Soroban Address |
| swap | `token_out` | same | same |
| swap | `token_amount_in` | `scval.AsAmountFromI128` | i128, sign > 0 |
| swap | `token_amount_out` | same | same |
| swap | `caller` | `scval.AsAddressStrkey` | extracted but not used in trade today |
| join_pool / deposit | `token_in` | `scval.AsAddressStrkey` | valid Soroban Address |
| join_pool / deposit | `token_amount_in` | `scval.AsAmountFromI128` | i128, sign > 0 |
| join_pool / deposit | `caller` | `scval.AsAddressStrkey` | stored as the LP user |
| exit_pool | `token_out` | `scval.AsAddressStrkey` | valid Soroban Address |
| exit_pool | `token_amount_out` | `scval.AsAmountFromI128` | i128, sign > 0 |
| exit_pool | `caller` | `scval.AsAddressStrkey` | stored as the LP user |
| withdraw | `token_out` / `token_amount_out` / `caller` | as above | as above |
| withdraw | `pool_amount_in` | `scval.AsAmountFromI128` | i128, sign > 0 (BPT burned) |

Decoder rejects with `ErrNonPositiveAmounts` if any required amount is zero /
negative. Swap direction: `(token_in, token_amount_in) → base`,
`(token_out, token_amount_out) → quote`. Liquidity events stamp
`direction = 'add'` (join_pool / deposit) or `'remove'` (exit_pool / withdraw).

## Failure modes specific to Comet

1. **Topic[0] symbol change** (`"POOL"` → other namespace, e.g. `"COMET_POOL"`): silently drops every event.
2. **Topic[1] symbol change** (`"swap"` → `"trade"`): silently drops every trade; same for each of `join_pool` / `exit_pool` / `deposit` / `withdraw` since 920725666 (drops that kind from `comet_liquidity`). New variants (e.g. `(POOL, set_controller)`) fall through `Matches` into the global unmatched tally (no per-source orphan series, correction 2026-08-03) and surface via the ADR-0033 recognition audit.
3. **Body field rename** (`token_in` → `tokenIn`, `token_amount_in` → `amount_in`): field-not-found per event; every swap dropped under that WASM.
4. **Body field removal**: same effect.
5. **Body field type change** (i128 → u128, Address → bytes): strict extraction errors per event.
6. **Body shape change Map → Vec**: errors at the Map-cast; every swap dropped.
7. **A new pool architecture at the same WASM lineage emitting a different swap shape.** No central factory, so a "Comet v2" would only be found via Hubble cross-check (count diff) or per-pool WASM review. **Unique-to-Comet failure mode**; elsewhere a factory upgrade is the source of truth.

## WASM timeline

Two verified lineages are active in the curated set:

- **Pool contract**: `CAS3FL6T...` (deployed by the Blend backstop-bootstrapper in Blend's mainnet rollout).
- **WASM hash**: `8abc28913035c074...` in the L55,261,759 Blend mainnet snapshot; the pool's contract-data entry was last modified at L51,499,546 (first instantiated by Blend's deploy).
- **Current state (2026-04-29)**: `stellar contract fetch --id` returns the same hash → no `update_current_contract_wasm` events between L51,499,546 and r1's current tip.
- **BLNT/USDC and BLNT/XLM Comet V1.1 pools**: `CDQMIKIH…` and `CA5HX3D4…` were created at ledgers
  65,134,030 and 65,134,032 with released V1.1 hash `d735c339…`.

The original wasm-history walk did not watch this contract (Comet wasn't in
the original 13-contract list; no factory known). It was audited via
`stellar contract fetch --id`. The Comet V1.1 lineage was added from the exact released artifact,
deployment-record, and interface evidence.

## Storage layout (LP valuation)

Read from the upstream source of both releases (CometDEX/comet-contracts-v1 `v1.0.0` =
`ef4cbfad0a35202ad267c14d163d2f362995a8d3`, `v1.1.0` = `b3a2d827ed6c158341f8e67ea91eb3b0395d3871`;
`storage_types.rs`, `metadata.rs` and `balance.rs` are byte-identical between them) and checked against the three
pools' entries at the lake tip (`test/fixtures/comet_storage/`). A unit variant of `DataKey` is keyed
`Vec[Symbol(<Variant>)]`.

| datum | key | durability | value | written by |
| --- | --- | --- | --- | --- |
| per-token record | `Vec[Symbol("AllRecordData")]` | persistent | `Map<Address, {balance: i128, weight: i128, scalar: i128, index: u32}>` | init, every join/exit/swap/deposit/withdraw, v1.0 `gulp` |
| LP supply | `Vec[Symbol("TotalShares")]` | persistent | `i128`, 7 decimals | mint/burn of LP shares |
| swap fee, controller, freeze | `SwapFee`, `Controller`, `Freeze` inside the instance | instance | `i128` at 1e7, `Address`, `bool` (absent = false) | init, `set_controller`, `set_freeze_status` |

- `weight` is at 1e7 scale and `init` asserts the weights sum to 1e7, so they are already normalised. Weights, fee,
  tokens and scalars cannot change after init. `scalar` = 10^(18 − decimals): 10^11 for every curated token.
- No total-weight key exists. `PublicSwap`, `Finalize` and `Factory` are declared but never written.
- v1.0 (`8abc2891…`) has a permissionless `gulp` that sets a record to the token balance and emits no event, and it
  accepts self-pair swaps. v1.1 (`d735c339…`) has neither. A record can therefore change with no POOL event, so only
  entry history, never an event replay, reproduces the balances.

The lending-state writer (`deploy/clickhouse/lending_state.sql`) captures the three pools' `AllRecordData`, `TotalShares` and
instance entries into `stellar.lending_state_hourly` (protocol `comet`) as exact keys. LP holder `Balance` keys and the
token contracts' balances are not captured. `internal/cometlp` decodes them strictly (an unknown field or key is an
error) and refuses a contract outside the curated set or an instance whose WASM is not the pool's audited hash.

**Valuation** (`internal/cometlp`, integers of 1e-7 quote token or USD, every division rounded down):

- Floor: the quote leg `exit_pool` pays for B shares, with the contract's own two floors (`c_math.rs`
  `calc_exit_ratio` and `calc_exit_withdrawal_amount`, the same in both releases): `ratio = ⌊B·1e18/S⌋`,
  `out = ⌊b_q·ratio/1e18⌋`. It is never above the real redemption, and at most `⌊b_q·B/S⌋`. `exit_pool` reverts
  (`ErrMathApprox`) when any leg's output is 0, so a floor of 0 means the shares cannot be exited at that state. It is
  a lower bound of the sampled state, not of fair value: one swap moves an hour's sample at a cost of about
  `2·fee·Δ`.
- Implied `⌊b_q·B·1e7/(S·w_q)⌋`: the quote leg divided by its weight. It carries no information beyond the floor.
- In USD the floor and implied use a lower bound of the quote price (`min(1, P)` for a dollar stablecoin); the marked
  value and the divergence check below use the market price. A missing or non-positive quote price is `unpriced`.
- Marked `⌊(b_q·P_q + b_x·P_x)·B/S⌋`, served only while `w_x·|ρ − 1| ≤ 2/100` with `ρ = P_x/(p_pool·P_q)`; otherwise
  implied, `approximate`, `pool_off_market`. `P_x` must exclude this pool's trades.
- Frozen: floor only (`pool_frozen`). A positive value below one unit is `0` with `lp_value_below_precision`, which is
  the drained Blend pool's case (0.4351707 USDC against 1.86e20 LP).
- Daily: the lower median (sorted element `⌊(n − 1)/2⌋`) of the hour-close values.

## Per-hash review findings

Rows are per unique WASM hash, so deployments sharing the same hash are grouped.

| hash (first 16) | role | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| `8abc28913035c074` | Blend backstop pool (CAS3FL6T...) | L51,499,546 (first deploy) → r1 current tip (no upgrade) | maintainer@2026-04-29 | matches current decoder |
| `d735c3395f595101` | BLNT/USDC and BLNT/XLM Comet V1.1 pools | L65,134,030 / L65,134,032 → current | maintainer@2026-11-15 | exact V1.1 release artifact; event ABI matches current decoder |

### `8abc28913035c074` — Blend backstop pool, single hash

**Disassembly evidence:**

1. **Contract interface** (`stellar contract info interface`): `swap_exact_amount_in`,
   `swap_exact_amount_out`, `join_pool`, `exit_pool`, plus the `ALLOWANCE` /
   `Balance` / `SwapFee` etc. SEP-41 surface (pool tokens are LP shares).
   Matches the `c_pool/call_logic/pool.rs` source the decoder was verified
   against on 2026-04-23.
2. **Binary strings** include `SwapEvent`, `DepositEvent`, `WithdrawEvent`, and
   the body field names `caller`, `token_in`, `token_out`, `token_amount_in`,
   `token_amount_out` (concatenated literal in the data section); the decoder
   pulls exactly these 5 by name.
3. **Topic encoding**: `POOL` is a 4-char Soroban small symbol (u64 constant in
   the WASM); `swap` likewise. Both match the decoder's pre-computed
   `TopicSymbolPool` / `TopicSymbolSwap` constants (verified 2026-04-23 via fixture capture).

### `d735c3395f595101` — BLNT/USDC and BLNT/XLM Comet V1.1 pools

1. **Artifact identity:** the deployed artifact's SHA-256 is
   `d735c3395f59510172cf5cf838823a1389b97cfec6bf24e580e9bd77d2b3e687`, matching the
   Comet V1.1 release artifact.
2. **Source identity:** the verified source checkout is Comet V1.1 tag `v1.1.0` at commit
   `b3a2d827ed6c158341f8e67ea91eb3b0395d3871`.
3. **Interface compatibility:** `stellar contract info interface` exposes the five event structs
   and named fields decoded by this source, including all swap and liquidity variants.

## Caveats

- **Pool-of-pools enumeration is structural, not exhaustive.** A new pool using
  the SAME canonical WASM (what the Comet contracts repo publishes; no factory)
  runs the same decoder path and yields compatible trades. A different
  Balancer-v1 port emitting the same `POOL`/`swap` topic with a different body
  is the "Comet v2" problem in Failure modes; the decoder fails loud
  (`ErrMalformedPayload` per event). **BackfillSafe applies to any range up to
  the last audit verification.** (Pre-gate this also required no
  topic-squatting deployment in the range; since the 2026-07-08 ADR-0035
  identity gate, attribution is anchored to the curated pool set, so squatting
  cannot attribute; correction 2026-08-03.)
- **No automated re-verification when new Comet pools deploy.** Operators should
  monitor `comet`-source trade volume and distinct-contract-count; a sudden
  surge could indicate an unaudited pool.

## Decision

**`BackfillSafe: true`** — flipped in
`internal/sources/external/registry.go` in this PR.

Rationale:

- Every curated deployment runs one of two audited hashes: historical Blend `8abc2891…` or
  released Comet V1.1 `d735c339…`.
- WASM bytes and interfaces preserve all five decoded event schemas.
- No upgrade was observed for the historical pool; the BLNT/USDC and BLNT/XLM pool deployments were bound to the exact
  V1.1 artifact at creation.
- The topic-based decoder is robust to future pools using the canonical WASM; a non-canonical contract emitting `("POOL", "swap")` with a different body fails extraction (`ErrMalformedPayload`) rather than mis-attributing.
- Live ingest health: 0 `ErrMalformedPayload` rate spikes against the comet source.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/comet/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/ingest-pipeline.md#contract-schema-evolution`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["comet"].BackfillSafe`
- Upstream contract source: local checkout under
  `.discovery-repos/comet-contracts/`
