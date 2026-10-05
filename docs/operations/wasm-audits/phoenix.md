---
title: Phoenix WASM-history audit
last_verified: 2026-09-30
status: ratified — v2 per-instance walk complete; 2026-07-07 Map-schema addendum; 2026-09-30 lake lineage (14th pool; all 64 hashes string-checked)
source: phoenix
backfill_safe: true
---

# Phoenix WASM audit

Audit log for the `phoenix` source's `BackfillSafe` flag. Procedure: `README.md`.

> **2026-05-03 update — v2 per-instance walk complete.** The 2026-04-30
> wide-net r1 walk inventoried all 13 Phoenix contracts (1 factory + 1
> multihop + 11 pools) across 22 distinct WASM hashes: 5 factory + 3
> multihop + 14 pool variants, the most-iterated source we audit. The two
> pool hashes of the original audit (`13b158655e403969…`,
> `167ab414a226427d…`) are two of the 14; the timeline is in `Phase 2 results`
> below. Verdict: every pool WASM exposes the Phoenix pool API
> (`provide_liquidity`, `withdraw_liquidity`, `simulate_swap`,
> `query_pool_info`) and emits all 8 swap field strings (`sender`,
> `sell_token`, `offer_amount`, `actual received amount`, `buy_token`,
> `return_amount`, `spread_amount`, `referral_fee_amount`).
>
> **2026-05-01 update.** Hash citations cross-checked against the
> 2026-04-30 r1 walk; see [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md).

## Status

**Ratified 2026-04-29.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same PR as this audit. All
11 mainnet phoenix pools enumerated via the factory's `query_pools()`;
current WASMs fetched via `stellar contract fetch` against
mainnet.sorobanrpc.com. Two unique pool-WASM hashes, **both
decoder-compatible** by binary-string verification. The 5 factory + 3
multihop hashes are informational only (the decoder targets per-pool
swap-field events).

## Contracts under audit

From `internal/sources/phoenix/events.go` (verified 2026-04-23 against
Phoenix-Protocol-Group/phoenix-contracts deploy scripts):

| role | contract |
| --- | --- |
| Factory | `CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI` |
| Multihop | `CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G` |

Pools are deployed by the factory at runtime and share a factory-deployed
WASM hash, so one per-hash review covers all pools.

## Decoder expectations

From `internal/sources/phoenix/{events,decode}.go` at HEAD as of 2026-04-27.
Phoenix's event shape is the most unusual of our Soroban sources and the
decoder is correspondingly fragile.

### The 8-events-per-swap quirk (AGENTS.md "Phoenix emits 8 events per swap")

Verified against `phoenix-contracts/contracts/pool/src/contract.rs:1172-1185`.
A swap publishes **8 contract events**, one per field. Each has the same
2-element topic shape:

    topic[0] = ScvString("swap")
    topic[1] = ScvString(<field name>)
    body     = the field value (i128 amounts, Address tokens, etc.)

| field name | body type | meaning |
| --- | --- | --- |
| `sender` | Address | trader |
| `sell_token` | Address | base asset |
| `offer_amount` | i128 | base amount sold |
| `"actual received amount"` (with spaces) | i128 | received gross |
| `buy_token` | Address | quote asset |
| `return_amount` | i128 | quote amount delivered (net of fees) |
| `spread_amount` | i128 | slippage component |
| `referral_fee_amount` | i128 | optional referral cut |

A `RawSwap` is correlated by `(ledger, tx_hash, op_index)`; the buffer
waits for all 8 events. Fewer than 8 → `ErrIncompleteSwap`; the buffer's
eviction policy must drop these eventually.

### Why topic[0] / topic[1] are ScvString, not ScvSymbol

`"actual received amount"` has spaces (Phoenix Q2); Soroban Symbols are
identifier-shape only, so the contract emits all 8 literals as `ScvString`.
Both topics are `ScvString` even where the content is identifier-like (7 of
8).

Classification is **byte-equal** against pre-encoded `ScvString` constants.
Switching any field to `ScvSymbol` silently drops every event of that
field; losing one of the 8 means `RawSwap` never completes and **every swap
in the range is dropped**.

### Trade direction

`(sell_token, offer_amount)` → base, `(buy_token, return_amount)` → quote.
No `base_is_seller` flag; direction is authoritative from the topic
addresses.

## Failure modes specific to Phoenix

1. **Topic[0] string change** (`"swap"` → `"trade"`): drops every event.
2. **Any of the 8 field names change**, esp. `"actual received amount"`
   (underscores would orphan every swap).
3. **Topic[1] ScvString → ScvSymbol** for the 7 spaceless fields: breaks
   byte-equal classification.
4. **i128 → u128** for an amount field (offer / actual / return / spread /
   referral): strict `AsAmountFromI128` errors; every swap dropped.
5. **9th event added**: ignored; swaps still emit, but any amount info in it is
   missed.
6. **Field removed (7 events)**: `RawSwap` never completes.
7. **Address field body type change** (e.g. ScvAddress → ScvBytes): extraction
   errors.
8. **The 8 events split across ops or txs**: Phoenix Q1 says `(ledger,
   tx_hash, op_index)` suffices; an upgrade splitting the publish breaks it.
   Needs per-WASM source review.

## WASM timeline

`stellarindex-ops wasm-history` over the post-Soroban window, full archive
on r1, walked 2026-04-29:

```json
[
  { "contract": "CB4SVAW... (factory)",
    "ranges": 5 distinct WASM hashes (factory upgrades — informational only) },
  { "contract": "CCLZRD4E... (multihop)",
    "ranges": 3 distinct WASM hashes (multihop upgrades — informational only) }
]
```

Factory + multihop hashes are **not decoder-relevant** (the decoder targets
per-pool swap-field events); the load-bearing audit is the per-pool contracts.

### Pool enumeration (decoder-relevant)

All 11 mainnet pools via factory `query_pools()` (2026-04-29, mainnet.sorobanrpc.com):

```
CBHCRSVX..., CBCZGGNO..., CBISULYO..., CDQLKNH3..., CBW5G5SO...,
CDMXKSLG..., CD5XNKK3..., CC6MJZN3..., CB5QUVK5..., CCKOC2LJ...,
CCUCE5H5...
```

Per-pool current WASM hashes (via `stellar contract fetch --id
<pool>` + sha256):

| pool count | WASM hash (first 16) |
| --- | --- |
| 10 pools | `167ab414a226427d` |
| 1 pool | `13b158655e403969` (CD5XNKK3...) |

## Per-hash review findings

| hash (first 16) | role | active pools | reviewer | finding |
| --- | --- | --- | --- | --- |
| `167ab414a226427d` | pool (dominant) | 10 of 11 | maintainer@2026-04-29 | all 8 field-name strings present; matches current decoder |
| `13b158655e403969` | pool (singleton) | 1 of 11 (CD5XNKK3) | maintainer@2026-04-29 | all 8 field-name strings present; identical contract interface to dominant; matches current decoder |

### Disassembly evidence

Both pool WASMs via `stellar contract info interface` + `strings`: the
interface diff is empty (same methods `swap`, `provide_liquidity`,
`withdraw_liquidity`, `query_*`, `simulate_*`; same types `Config`, `Asset`,
`ComputeSwap`, `PoolResponse`; size 37047 vs 36810 bytes is constants/build
metadata). Both contain, after the source path
`contracts/pool/src/contract.rs` (matches upstream
`Phoenix-Protocol-Group/phoenix-contracts`): `swap`, `sender`, `sell_token`,
`offer_amount`, `actual received amount`, `buy_token`, `return_amount`,
`spread_amount`, `referral_fee_amount`. The space-bearing `actual received
amount` (riskiest tripwire, Phoenix Q2) is verbatim in both.

## Phase 2 results — per-instance walk (executed 2026-04-30)

The wide-net r1 walk covered all 13 Phoenix contracts (of 540 watched).
**22 unique WASM hashes** across [50,457,424, 62,249,727]. Parameters: 8
parallel chunks, ~5h; watch list = factory + multihop + 11 pools from
`query_pools()`.

| Role | Contract count | Unique WASMs | Notes |
| --- | --- | --- | --- |
| Factory | 1 | 5 | Iterates the deploy-time pool template + admin surface; not decoder-relevant |
| Multihop | 1 | 3 | Aggregates pools for cross-pair routing; not decoder-relevant |
| Pool | 11 | 14 | The decoder-relevant set — all 14 emit the audited 8-event swap shape |

**Why 14 pool WASMs across 11 pools.** Pools upgrade in place via
`upgrade(env, new_wasm_hash)`; several moved through 2-3 WASMs. The walker
captured each chain (`update_current_contract_wasm` transitions); per-pool
timelines are in the walk's JSONL output (`/tmp/walk-checkpoint/` on r1).

**Decoder verdict per pool WASM.** All 14 pool WASMs contain all 8 swap-field
strings, including `actual received amount`; no silent rename across the
upgrade chain. **Byte presence is not runtime uniformity**: the pre-upgrade
pool WASM emitted only 7 of 8 fields per swap (no `ActualReceived`) for
ledgers 51,019,036 → 53,134,167 despite the string being in the binary — a
real emission gap, not a rename. The decoder handles it via
`phoenix.RawSwap.Decodable()` (reduced-field recovery, distinct from
`Complete()`'s post-upgrade 8-field check); see
`internal/sources/phoenix/decode.go`. WASM bytes SHA-256-verified at
`evidence/r1-walk-2026-05-01/wasm-bytes/<hash>.wasm` on r1 for all 22
hashes; disassembly (`wasm2wat` + `strings`) under
`evidence/r1-walk-2026-05-01/disasm/`.

**Factory + multihop iteration.** The 5 factory and 3 multihop variants are
informational; neither is on the trade-emission path. Backfill safety is
unaffected.

## Caveats

- **Pool-WASM history walked end-to-end as of 2026-04-30.** ~~v2
  follow-up~~: ✅ done — see `Phase 2 results` above.
- **New pools deployed after 2026-04-29 are not in this audit.** They should
  use one of the known hashes (template), but if the factory's pool-WASM-hash
  setting changed, the next run of this audit (extending `last_verified`)
  would catch any new hash.

## 2026-07-07 addendum — new Map-body swap schema + QuoteAmount correction

Two findings from the 2026-07-06 lake audit (factory
`("create","liquidity_pool")` walk over `stellar.contract_events` — **13
pools**, up from 11 in the 2026-04 walk).

### New pool WASM: single-event Map-body swap (`CBENABXP…`)

A 13th pool,
`CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S` (factory-created
2026-07-02, right after a factory `("Factory","Updated Config")` event),
runs a NEWER WASM whose swap shape differs from the audited 8-event String
schema:

- topic[0] is `ScvSymbol("swap")` (disc 15), **not** `ScvString("swap")`
  (disc 14).
- ONE event per swap; body is an `ScvMap` with 8 Symbol keys
  (underscore-spelled): `sender`, `sell_token`, `offer_amount`, `buy_token`,
  `return_amount`, `actual_received_amount`, `referral_fee_amount`,
  `spread_amount`. Failure-mode #2's underscore variant is exactly this
  schema — a distinct shape, not a corruption.

Decoded via `decodeSwapMap` (map-field-name lookup, no correlation buffer),
gated via `MainnetMapPools`. Real fixture: ledger 63307899, tx `3cb06db3…`,
event_index 3 (golden test `internal/sources/phoenix/mapswap_test.go`).

**WASM hash:** captured 2026-09-30 from `stellar.contract_instance_changes`
(two hashes; the pool was upgraded 49,433 ledgers after create — see the
addendum below). The decode is field-name driven, but the BackfillSafe trail
needs the bytes string-checked before this pool contributes to any
historical backfill range. The first hash also emits the `blend_pool`
settings events (see the event-shapes section below).

### QuoteAmount field-mapping correction (ALL pools)

The decoder mapped `canonical.Trade.QuoteAmount = actual received amount`.
Per `do_swap`, that field is the INPUT the pool received of `sell_token`
(`balance_after − balance_before`), byte-identical to `offer_amount` — NOT
an output. Every Phoenix trade therefore had `base_amount == quote_amount`
(live: 237,387/237,387 served rows). Corrected to `return_amount` (the
net-of-fees `buy_token` amount sent to the taker) 2026-07-07; both schemas
use `return_amount`. **Requires a historical Phoenix trade re-derive** to fix
pre-fix rows.

## 2026-09-19 addendum — Factory create event (audit finding F048)

**Status (2026-09-19): the decoder DOES admit pools from the factory's
create event**, gated on `reg.IsFactory(emitter)` plus both `ScvString`
topics plus an `Address` body. What this does and does NOT buy is in "The
trust this extends". The operator seed (`phoenix.MainnetPools` /
`MainnetMapPools` / `MainnetStakeContracts`, plus the `protocol_contracts`
warm) remains as cold-start warm root and override. **Pools only:** the
factory does not announce stake contracts, so those are admitted by the seed
or the warm alone.

### What real lake captures settle

Fixtures: `test/fixtures/phoenix/factory-create/` (four create events,
2024-05-07 and 2026-07-02), pinned by
`test/controlwiring/phoenix_factory_create_fixture_test.go`.

- The create events **are in the lake** from ledger 51,572,026; any "the
  factory's creation events predate the lake" statement in the tree is false.
- `topic[0]`/`topic[1]` are `ScvString` `("create","liquidity_pool")`, so
  the lake's `topic_0_sym` is **empty** for these rows while the Postgres
  landing zone fills it (`extract.go` uses `GetSym`, the landing zone
  `tryDecodeSymbolOrString`). A ClickHouse creation walk keyed on
  `topic_0_sym` (`gatedPrefilter`, `compute_completeness.go`) matched
  nothing for phoenix. FIXED 2026-09-19: `topic0Predicate`
  (`internal/storage/clickhouse/event_reader.go`) matches the name in EITHER
  encoding — the column, or the `ScvString` blob in `topics_xdr[1]` —
  widening a prefilter without changing what `topic_0_sym` means for
  existing rows (no re-extract).
- The body is **one contract `Address`, the pool**. Stakes are never
  announced, so they stay operator-seeded.
- Shape identical in 2024 and 2026; the four announced addresses are pools
  the curated seed already lists, at the ledgers its comments cite.

### The security properties, and how they were established

Auto-admission trusts the event body. That is sound only if:

1. `create_liquidity_pool` is restricted to an allow-list, enforced in that
   function, not merely stored; and
2. the published address is the one the factory **deployed**, not a
   caller-passed value.

`defindex` self-registration was removed on 2026-08-25 because its create
body carried caller-supplied addresses (a permissionless registry-poisoning
vector). Honest history proves nothing about what a hostile caller can make
the factory publish, so the properties were read off the source.

**Verified 2026-09-19 from public upstream source**
(`Phoenix-Protocol-Group/phoenix-contracts`, `contracts/factory/src/contract.rs`
+ `utils.rs`), read raw at `main`, `v2.0.0`, `v1.1.0` and `v1.0.0`; all four
agree:

1. **Allow-listed creators only.** `create_liquidity_pool` opens with
   `sender.require_auth()`, then
   `if !get_config(&env).whitelisted_accounts.contains(sender) {
   panic_with_error!(.., NotAuthorized) }`. The allow-list changes only under
   the admin's auth.
2. **The address is factory-DEPLOYED, never caller-supplied.**
   `lp_contract_address` is the RETURN of
   `env.deployer().with_current_contract(salt).deploy_v2(<wasm hash from
   the factory's own config>, init_args)` (v1.x: `deploy_lp_contract` →
   `…with_current_contract(salt).deploy(lp_wasm_hash)`), with
   `salt = sha256(token_a ‖ token_b)` (Blend pools prefix a type byte).
   **The function takes no pool-address parameter** — what `defindex`'s
   announcement could not promise.
3. **The event publishes exactly that address**, and
   `("create", "liquidity_pool")` is the ONLY `("create", …)` publish in the
   factory. It is emitted from inside the factory, so the emitting contract
   id IS the factory; gating on `reg.IsFactory(emitter)` cannot be satisfied
   by a foreign emitter.
4. **The stake contract is not announced.** The factory passes
   `stake_wasm_hash` into the pool's init args and the POOL deploys its stake.
5. **There is no `create_liquidity_pool_v2`** at any of the four versions —
   see the residual below, which contradicts the in-repo disassembly.

### The trust this extends

- **The source is not the installed WASM.** The factory is admin-upgradeable
  (`update_current_contract_wasm` under `admin.require_auth()`), and the
  installed bytes were not hashed against a build (see the residual).
  Admission trusts **the Phoenix factory admin**: a malicious upgrade could
  publish an arbitrary address. The curated seed already extended this trust
  by hand; it is now automatic, hence the seed stays the override.
- **Identity trust, not price trust.** A whitelisted creator picks
  `token_a` / `token_b`, so an admitted pool may trade a worthless asset.
  Admission decides whose events are *attributed to Phoenix*; served prices
  are the downstream pricing guards' problem.
- **Pools only** (point 4); stakes remain operator-seeded.

Worth having, not done here: an alert on a factory WASM upgrade.

### Residual: the installed bytes were not hashed

The source review did not identify WHICH build is installed at
`CB4SVAWJ…CKMI`. Evidence from the in-repo walk
(`evidence/r1-walk-2026-05-01/disasm/*.json`, all five factory variants),
and its limits:

| factory WASM (first 16) | observed ledgers | allow-list surface in exports / strings |
| --- | --- | --- |
| `e1464afcf0c7c01e` | 51,572,016 – 51,937,331 | `update_whitelisted_accounts`, `whitelisted_accounts` |
| `96c6a73863de6e33` | 53,134,143 – 53,417,239 and 53,649,587 – 54,517,224 | `update_whitelisted_accounts`, `whitelisted_accounts` |
| `2bbb91c58cb8432f` | 54,517,225 – 54,517,363 | same, plus `create_liquidity_pool_v2`, `remove_pool` |
| `721badb85470a81d` | 54,517,364 – 54,897,147 | same, plus `create_liquidity_pool_v2`, `remove_pool` |
| `c54ba54bd9e37503` | 57,406,830 – 57,856,963 | **no** `update_whitelisted_accounts`; `whitelisted_to_add` / `whitelisted_to_remove` under `update_config`; `__constructor`, `propose_admin` |

- An allow-list **exists as state** in every known variant, consistent with
  the source. Export and string tables cannot show it is *checked* inside
  `create_liquidity_pool`; that comes from the source.
- **The walk is stale.** Its last factory range ends at ledger 57,856,963;
  the newest captured create is at 63,293,708, preceded by a
  `("Factory","Updated Config")` at 63,293,663. Every variant exports
  `update`, so the factory may run a sixth WASM the walk never saw. Observed
  ranges also have holes (51,937,331 → 53,134,143, 53,417,239 → 53,649,587
  and 54,897,147 → 57,406,830).
- **One disagreement.** Two variants (`2bbb91c5…`, `721badb8…`, ledgers
  54,517,225 – 54,897,147) export `create_liquidity_pool_v2`, which exists in
  NONE of the four upstream versions read: either the builds pre-date a
  rename or are off the tags checked. The gate does not depend on which (it
  keys on emitter = factory and the topic pair, and
  `("create","liquidity_pool")` is the only `("create", …)` publish in every
  source version seen), but a `_v2` entry point publishing the same topics
  from an unread build is the shape this residual covers, so the follow-up
  below is not optional.

### What shipped, and what is still owed

Shipped (2026-09-19):

- `internal/sources/phoenix`: `classifyAny` gained `actionCreatePool` for the
  exact pair `("create","liquidity_pool")`; any other `("create", …)` stays
  `actionUnknown` and fail-closes into a recognition gap. `Matches` gates
  that action on `reg.IsFactory`, never `reg.Has` (a curated pool passes
  `reg.Has` and would be the strongest forger). `Decode` seeds the announced
  pool with the factory as provenance, before taking the correlation-buffer
  lock.
- `internal/storage/clickhouse`: the topic[0] prefilter matches both
  encodings, so the lake walk keyed on `"create"` returns rows. Phoenix's
  reconcile-catalogue entry now sets `factories` + `creationSym` so
  `gatedPrefilter` runs.
- Tests: `TestK023_PhoenixFactoryCreateEventIsAdmissible` and
  `…FromForeignEmitterIsNotAdmitted` GRADUATED out of `-tags k023evidence`
  into the default suite (`test/controlwiring/phoenix_factory_admission_test.go`);
  `test/integration/contract_events_string_topic_prefilter_test.go` proves
  lake → prefilter → decoder → gate against a real ClickHouse.

Still owed:

1. `stellar contract fetch --id CB4SVAWJ…` against mainnet, sha256, and
   record the installed factory hash here, closing the residual. An admin
   upgrade can invalidate either property later, so verification is per
   installed hash, same as the pool WASMs; an alert on a factory WASM upgrade
   is the durable form.
2. Re-derive the history this unblocks: pools announced since ledger
   51,572,026 that the curated seed never got have unattributed lake events.
   `seed-protocol-contracts` for phoenix ENUMERATES them (it was inert
   before); `projected-rebuild -source phoenix … -write` re-derives them.
   Phoenix is `BackfillSafe: true`.

Rejected: "recognise without admitting" is not a stop-gap. `ch-recognition`
tests ONE exemplar per `(contract_id, topic_0_sym)` shape through the decoder
chain's `Matches`, and the factory's String-topic events all share the empty
`topic_0_sym`, so they are one shape. When its exemplar is a create event, a
decoder that matched and dropped it would report the factory as recognised
while admitting nothing — muting the one audit that can surface a new pool
today.

## Event shapes beyond swap and liquidity

A per-WASM census of every event the gated set emits found unclassified
shapes; all are decoded now, pinned by real rows in
`test/fixtures/phoenix/event-shapes/`:

- stake `create_distribution_flow` → `asset` (Address, no user);
- stake migration steps `("Stake: Migration: ", "Start of migration for user: " | "Query for user completed: ")`
  and `("Stake", "Migration for user completed and stored: ")` → user Address;
- factory `("Factory","Updated Config")` → Void, gated on the factory only;
- pool `("blend_pool", set_delegate | set_min_trading_a | set_min_trading_b)`
  → Address or i128;
- Map-body `provide_liquidity` / `withdraw_liquidity` (Symbol topic, one event).

The earliest stake WASMs (`9e398ab7ca651b4277df9ed390f4e012465c21447bad21ed6a0e9c6b975fd532`
from 51,572,026, then `a757fd97e5a67f586c5aadf6ba8d17a9d3df872ccae093e564c2b3de143c05f3`)
publish an unbond as `("unbond","user")` followed by `("bond","token")` and
`("bond","amount")`. The correlation buffer continues the open unbond with
those two fields instead of opening a bond.

## 2026-09-30 addendum — WASM lineage captured from the lake

Every hash here comes from `stellar.contract_instance_changes` on r1
(ClickHouse, `--port 9300`), which records the executable each contract
instance points at on every instance change. It answers the "PENDING operator
capture" and "installed bytes were not hashed" residuals without an RPC
fetch, and is complete where the 2026-05-01 `wasm-history` walk was sampled:
the walk's three factory-range "holes" are not holes; each hash runs
contiguously from its install ledger to the next.

```sql
SELECT toString(contract_hash), wasm_hash, min(ledger_seq), max(ledger_seq), count()
FROM stellar.contract_instance_changes
WHERE contract_hash IN (<lower-hex of the 32-byte contract id>)
GROUP BY 1, 2 ORDER BY 1, 3
```

(31 registry contracts + the factory's two newest children; 152 rows; every
registry contract is present.)

### Factory and multihop
Factory (`CB4SVAWJ…`):

| wasm_hash | installed at ledger | superseded at ledger |
| --- | --- | --- |
| `e1464afcf0c7c01e4306e3eb9d16f653500fbaa31c3cce0afd675449d58cea14` | 51,572,016 | 53,134,143 |
| `96c6a73863de6e331d8103898f421bb7af719b87bbd4ac14d5a16f6b2662546b` | 53,134,143 | 54,517,225 |
| `2bbb91c58cb8432fd40446e80188a25e7c07dc2edc853e885d65cafbbbf19581` | 54,517,225 | 54,517,364 |
| `721badb85470a81d9d0a1c72dd0debb37f5f268419d4b94d125e546cc32d350e` | 54,517,364 | 57,406,830 |
| `c54ba54bd9e37503a641fa661126cd858faf57738c1e903b84f72ac505016393` | 57,406,830 | 63,266,299 |
| `56638944de087f45f3c9fd441204747ab91cc95331d4bde92811bec61bd12722` | 63,266,299 | 63,293,457 |
| `b64fa5b9e3f0074a772d10743c1951bc6c1a716d0f7521f205eb0b516b15cfab` | 63,293,457 | 64,028,476 |
| `8fbd78ede40e9d722259f85e0511b347397c0ffcca4c8fa580a73d9d079da7f1` | 64,028,476 | **current** |

Multihop (`CCLZRD4E…`):

| wasm_hash | installed at ledger | superseded at ledger |
| --- | --- | --- |
| `60332ba12801eda65874e9afdbfdf8d114f56011fe7d1f902103cd7767c547d4` | 51,572,024 | 53,134,204 |
| `18336805466bbd05bc388610d36de73994f5b9061bb199c02418a073fcf4b281` | 53,134,204 | 57,406,861 |
| `77bdc0a993960faa2b0dac7b43a5160d6ed745ee787573c2f0ad8d00d4806366` | 57,406,861 | 63,266,697 |
| `b2ffbefaafad05d4a2bde16b8fcf8b3d71c22a422caf19fe6a3fb744fb52fa20` | 63,266,697 | 63,293,527 |
| `67ba2a36d61df77053d628f4f6bea50d300caa972fe0d9c539ace9636369f484` | 63,293,527 | **current** |

Map-schema pool (`CBENABXP…`):

| wasm_hash | installed at ledger | superseded at ledger |
| --- | --- | --- |
| `f74d87d72381b4a5c787eb8b16a2b861aed6c3146583703ceb003d5befe9d338` | 63,293,708 | 63,343,141 |
| `6fe099b64855bcba2b7fc6f4cd9b0d8e6cc98743eac6cd44042fc7ef0c22e3f7` | 63,343,141 | **current** |

The factory has run **eight** builds, not five; `56638944…`, `b64fa5b9…` and
`8fbd78ed…` post-date the walk. Each of the factory's two
`("Factory","Updated Config")` events (63,293,663 and 64,028,582) follows a
factory upgrade by a few hundred ledgers (63,293,457 and 64,028,476) and
precedes a pool create (63,293,708 and 64,030,567).

### The `create_liquidity_pool_v2` residual — closed

The two builds exporting `create_liquidity_pool_v2` were installed for
ledgers 54,517,225 – 57,406,830. The factory's complete event history is 17
events: `initialize` ×1, `("Factory","Updated Config")` ×2 and
`("create","liquidity_pool")` ×14, at ledgers 51,572,026, 51,572,030,
51,572,101, 51,927,948, 53,853,219, 53,853,220, 53,955,603, **54,517,368**,
54,953,243, 54,953,245, 54,953,247, 54,953,248, 63,293,708 and 64,030,567.
Exactly one create falls in the `_v2` window — CD5XNKK3… at 54,517,368, four
ledgers after `721badb8…` was installed — and it published the same
`("create","liquidity_pool")` pair with a one-Address body, so whichever
entry point produced it, the gate saw it, and the pool it announced runs the
audited `13b158655e40…`. No `("create", …)` with another second topic exists
in the factory's history.

### Pools and stakes

- The 2026-04-29 `query_pools()` snapshot (10 × `167ab414…` + CD5XNKK3 on
  `13b15865…`) is still current for those 11 pools; the last upgrade on any
  was at 57,406,699.
- `CAZ6W4WH…` (the legacy pool added 2026-08-18) moved to
  `df98000b665b7aac…` at 54,515,539 — the ledger its swap activity ends at —
  and, with its stake `CDP6DT2Y…`, to `e5563daf8d18b213…` at
  63,767,534/63,767,536. A pool and stake sharing one executable and one
  event since (the upgrade ledger) reads as a retirement stub, not a new
  schema. Ten other stakes moved to `753c2154fb97b006…` at 63,770,079 –
  63,770,103.
- Stake contracts have run 25 distinct hashes (16 on `CAIR3UPW…` alone). None
  has had its `bond`/`unbond` field set string-verified — the standing
  INV-1381 gap, now with an exact install ledger per hash to extract from.

`f74d87d7…` was the template the factory deployed at create; the pool was
upgraded 49,433 ledgers later. Both were **unaudited bytes** at this point:
the decode is field-name driven (`decodeSwapMap`) and the golden fixture at
ledger 63,307,899 is from the `f74d87d7…` era. Neither hash may anchor a
historical backfill range until its strings are checked.

### A 14th pool the registry does not know — `CCPPPTDW…`

`CCPPPTDWJIWXQUQ2CN64S5JYQ7GYWVZIT7YWUUTH75HKIZX53Z2CE3XI`, factory-created
at ledger 64,030,567 (tx `f802e60d…`), 105 ledgers after the factory moved to
`8fbd78ed…` and 1,985 after its second `Updated Config`:

| wasm_hash | installed at ledger | superseded at ledger |
| --- | --- | --- |
| `13b158655e40396957537bf1c528c6542b315930c1c9e0df640f57293c8af2ca` | 64,030,567 | 64,030,672 |
| `d54d01e0d09005bd7d5267ce9e913a8c52163bfeb82252545351b76e86b41561` | 64,030,672 | **current** |

Deployed from the audited CD5XNKK3 template, then upgraded 105 ledgers later
to a hash nothing had read. Its 57 lake events to 64,607,591 are all
`ScvString`-topic with the audited field names exactly:
`("initialize","XYK LP token_a"/"token_b")`, `("toggle_trading",
"enabled")` at 64,030,690, `provide_liquidity` ×2 (5 fields each) and
`withdraw_liquidity` ×11 (4 fields each). **No swap yet.** It is in neither
`MainnetPools` nor `MainnetMapPools`, and r1's `protocol_contracts` holds only
the 16 curated stakes for phoenix (no pool at all), so its events are a
recognition gap: 0 served `phoenix_liquidity` rows for it — and 0 for
`CBENABXP…` too, whose liquidity schema this audit has not looked at.

What this changes:

1. `seed-protocol-contracts -source phoenix -write` on r1 is the owed deploy
   precondition from "What shipped"; the dry run walks the Postgres landing
   zone, whose retained window holds both recent creates as
   `topic_0_sym = 'create'`.
2. `d54d01e0…` (and `6fe099b6…`, `f74d87d7…`) needed the same string check as
   the two audited pool hashes before phoenix's next `projected-rebuild -write` covers their ledgers — done in the next section; all three pass.
3. The durable form of item 1 in "Still owed" is a lake query: a registry
   contract whose newest `contract_instance_changes.wasm_hash` is not in its
   audit log is drift; that check can run on a timer.

### Per-hash string check — all 64 hashes, 2026-09-30

Bytes for every hash above (and every stake hash) came from the lake:
`stellar.ledger_entries_current` keeps every `contract_code` entry ever
uploaded (the query behind `GET /v1/contracts/{id}/wasm`), so `SELECT
entry_xdr … WHERE entry_type = 'contract_code' AND key_xdr IN (<LedgerKey
CONTRACT_CODE + hash, base64>)` returned all 64 in one call, 2.2 MB. Each blob
was byte-searched for the literals the decoder watches
(`internal/sources/phoenix/events.go`), per role:

| role | hashes | all literals present | exceptions |
| --- | --- | --- | --- |
| pool (String schema) | 15 | 12, incl. `18e40185…`, `167ab414…`, `13b15865…` and the unread `d54d01e0…` (CCPPPTDW…) | `ac63334c…` (51,572,026 – 53,134,1xx, every first-era pool) lacks `actual received amount` — the known 7-of-8 era; `df98000b…` (23 KB) and `e5563daf…` (17.7 KB) lack every swap and liquidity literal — retirement stubs (CAZ6W4WH…) |
| pool (Map schema) | 2 | 2 — `f74d87d7…`, `6fe099b6…` carry all 8 underscore keys + `provide_liquidity`/`withdraw_liquidity` | — |
| stake | 34 | 32 — `bond`, `unbond`, `user`, `token`, `amount`, `withdraw_rewards`, `distribute_rewards`, `reward_token`, `asset` | `753c2154…` is **364 bytes** (CABWEFVX…, CAIR3UPW…, CB2S5X4H… since 63,770,079 – 63,770,103; no event since) — tombstone; `78bee632…` (6.7 KB, CBBUVHCE…'s only hash) has `bond` and nothing else — see below |
| factory | 8 | 8 — `create` + `liquidity_pool` in every build | `create_liquidity_pool_v2`/`remove_pool` exist only in `2bbb91c5…` and `721badb8…`; `update_config`/`Updated Config` from `c54ba54b…` on; `update_whitelisted_accounts` gone from `c54ba54b…` on — all three as the 2026-09-19 disassembly said |
| multihop | 5 | n/a (no expected topics) | — |

Consequences:

- Every pool hash that has ever carried a swap decodes with the current
  decoder, including the three unread that morning. The BackfillSafe trail
  now covers `CBENABXP…` and `CCPPPTDW…` for their whole lives.
- **`CBBUVHCE…` is not a Phoenix stake contract.** It entered
  `MainnetStakeContracts` from the 2026-05-01 lake-activity snapshot ("bond
  ×10"), but its only WASM has none of the stake field names and its lake
  events are `("bond","created"/"live"/"settconf"/"settled"/"expired")` (10
  events, 61,356,019 – 61,375,797) — a bond instrument, not
  `("bond", user|token|amount)`. It fail-closes today (0 served
  `phoenix_stake_events` rows, `actionUnknown`), so the data is right but
  the trust root is wrong: remove it from the curated set and from
  `protocol_contracts` (the 2026-09-30 seed upserted it from the curated
  list).
- Three stakes are tombstoned at 63,770,079 – 63,770,103 and one pool + stake
  at 63,767,534/536. They stay in the registry; a re-audit finding a new hash
  on them should expect a stub, not a schema.

## Decision

**`BackfillSafe: true`** — flipped in `internal/sources/external/registry.go`
in this PR.

- All 11 deployed mainnet pools enumerated and audited; 2 unique WASM hashes,
  both with all 8 field literals and identical interfaces.
- The strict 8-event correlation works identically on both hashes for the
  CURRENT era. The pre-upgrade WASM emitted 7 of 8 fields (no
  `ActualReceived`); `RawSwap.Decodable()` is the reduced-field recovery path,
  distinct from `Complete()`.
- Live ingest: 0 `ErrIncompleteSwap` / `ErrMalformedPayload` rate spikes.
- Caveats are v2-follow-up scope; binary-string verification is the primary
  safety claim.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/phoenix/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/contract-schema-evolution.md`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["phoenix"].BackfillSafe`
- Upstream contract source: `https://github.com/Phoenix-Protocol-Group/phoenix-contracts`
