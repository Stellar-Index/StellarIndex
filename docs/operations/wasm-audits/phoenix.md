---
title: Phoenix WASM-history audit
last_verified: 2026-07-07
status: ratified — v2 per-instance walk complete; 2026-07-07 Map-schema addendum
source: phoenix
backfill_safe: true
---

# Phoenix WASM audit

Audit log for the `phoenix` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

> **2026-05-03 update — v2 per-instance walk complete.** The
> 2026-04-30 wide-net r1 walk inventoried all 13 Phoenix
> contracts (1 factory + 1 multihop + 11 pools) across 22
> distinct WASM hashes — Phoenix is the **most-iterated source**
> in our walk (5 factory variants + 3 multihop variants + 14
> pool variants). The two pool hashes cited in the original
> audit (`13b158655e403969…`, `167ab414a226427d…`) are **two
> of the 14 pool variants** captured in the walk; full
> per-instance timeline is in `Phase 2 results` below.
> Decoder-compatibility verdict: every pool WASM exposes the
> Phoenix pool API (`provide_liquidity`, `withdraw_liquidity`,
> `simulate_swap`, `query_pool_info`) and emits all 8 swap
> field strings (`sender`, `sell_token`, `offer_amount`,
> `actual received amount`, `buy_token`, `return_amount`,
> `spread_amount`, `referral_fee_amount`).
>
> **2026-05-01 update.** Hash citations in this file have been
> cross-checked against the 2026-04-30 r1 walk; see
> [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md) for the
> consolidated cross-source picture and current contract+WASM
> inventory.

## Status

**Ratified 2026-04-29.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same PR as this
audit. All 11 mainnet phoenix pool contracts enumerated via the
factory's `query_pools()` view; their current WASMs were fetched
via `stellar contract fetch` against mainnet.sorobanrpc.com. Two
unique pool-WASM hashes total, **both decoder-compatible** by
binary-string verification. The 5 factory + 3 multihop WASM
hashes from the wasm-history walk are informational only (factory
+ multihop events are NOT decoded; the decoder targets per-pool
swap-field events).

## Contracts under audit

Captured from `internal/sources/phoenix/events.go` (verified
2026-04-23 against Phoenix-Protocol-Group/phoenix-contracts deploy
scripts):

| role | contract |
| --- | --- |
| Factory | `CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI` |
| Multihop | `CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G` |

Pool contracts are deployed by the factory at runtime; per-instance
contracts emit the swap events. Audit covers the factory + multihop
WASM evolution; per-pool contracts share a factory-deployed WASM
hash so a single per-WASM-hash review covers all pools.

## Decoder expectations

Captured from `internal/sources/phoenix/{events,decode}.go` at HEAD
as of 2026-04-27. **Phoenix's event shape is the most unusual of
any of our Soroban sources** and the decoder is correspondingly
fragile.

### The 8-events-per-swap quirk (AGENTS.md "Phoenix emits 8 events per swap")

Verified against `phoenix-contracts/contracts/pool/src/contract.rs:1172-1185`.
A single Phoenix swap publishes **8 distinct contract events** — one
per field — instead of one event with all fields packed in the body.
Every event has the same 2-element topic shape:

    topic[0] = ScvString("swap")
    topic[1] = ScvString(<field name>)
    body     = the field value (i128 amounts, Address tokens, etc.)

The 8 field names (verified against the contract source):

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

A `RawSwap` is correlated by `(ledger, tx_hash, op_index)`; the
buffer waits for all 8 field events before emitting a trade. Fewer
than 8 → `ErrIncompleteSwap`; the buffer's eviction policy must
drop these eventually.

### Why topic[0] / topic[1] are ScvString, not ScvSymbol

Embedded spaces in `"actual received amount"` (Phoenix Q2) — Soroban
Symbols are identifier-shape only (no spaces), so the contract
emits all 8 string literals as `ScvString` rather than `ScvSymbol`.
Both topic[0] (`"swap"`) and topic[1] (the field name) come through
as `ScvString` even though their content is identifier-like in 7
of the 8 cases.

Classification is **byte-equal** against pre-encoded `ScvString`
constants. A switch from `ScvString` → `ScvSymbol` for any field
silently drops every event of that field — and dropping even one
of the 8 means `RawSwap` never completes, and **every swap in the
range gets dropped**.

### Trade direction

Computed from `(sell_token, offer_amount)` → base, `(buy_token,
return_amount)` → quote. No `base_is_seller` flag; direction is
authoritative from the topic addresses.

## Failure modes specific to Phoenix

Drawing the generic checklist into Phoenix-specific tripwires:

1. **Topic[0] string change** — `"swap"` → `"trade"` (or any
   variant) silently drops every event of every field. Catastrophic.
2. **Any of the 8 field name string spellings change** — the
   correlation layer expects all 8; even one missing causes the
   `RawSwap` to never complete. Special attention needed for the
   space-bearing `"actual received amount"` — typo / canonicalisation
   (e.g. underscores) would orphan every swap.
3. **Topic[1] type change ScvString → ScvSymbol for fields without
   spaces** — possible if Phoenix later refactors to use Symbols
   for the 7 spaceless fields. Byte-equal classification breaks.
4. **i128 → u128 amount type swap** for any of the 4 i128 fields
   (offer / actual / return / spread / referral) — strict
   `AsAmountFromI128` errors out per event; the swap never
   completes; every swap dropped.
5. **Field added (9th event)** — the buffer waits for all 8 and
   emits when complete. A 9th event would be ignored (not in the
   matched set), so swaps would still emit on the 8 we recognise.
   But if the 9th event carries amount info that should affect
   accounting, we'd silently miss it.
6. **Field removed (7 events per swap)** — `RawSwap` never
   completes; every swap dropped.
7. **Body type for an Address field changes** (e.g. ScvAddress →
   ScvBytes) — decoder errors on extraction; swap never completes.
8. **The 8 events for a single swap arrive across multiple ops or
   txs (correlation key invalidated)** — Phoenix Q1 specifies
   `(ledger, tx_hash, op_index)` is sufficient; if a contract
   upgrade splits the publish across two ops, correlation breaks.
   Requires per-WASM source review.

## WASM timeline

Output from `stellarindex-ops wasm-history` over the post-Soroban
window — full archive on r1, walked 2026-04-29:

```json
[
  { "contract": "CB4SVAW... (factory)",
    "ranges": 5 distinct WASM hashes (factory upgrades — informational only) },
  { "contract": "CCLZRD4E... (multihop)",
    "ranges": 3 distinct WASM hashes (multihop upgrades — informational only) }
]
```

The factory + multihop hashes are **not decoder-relevant**: the
decoder targets per-pool swap-field events, not factory
pair-creation or multihop coordination events. They're captured
here for completeness but the load-bearing audit is on the per-pool
contracts.

### Pool enumeration (decoder-relevant)

All 11 mainnet pools enumerated via factory's `query_pools()` view
(2026-04-29 against mainnet.sorobanrpc.com):

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

Both pool WASMs were fetched and analyzed via `stellar contract
info interface` + `strings`:

1. **Contract interface diff is empty.** The two WASMs have
   identical public method signatures (`swap`, `provide_liquidity`,
   `withdraw_liquidity`, `query_*`, `simulate_*`, etc.) and
   identical contract types (`Config`, `Asset`, `ComputeSwap`,
   `PoolResponse`, etc.). The binary differences (37047 vs 36810
   bytes) are constants / build metadata, not interface.
2. **All 8 expected field-name strings appear in both binaries.**
   The decoder requires 8 string topics per swap (AGENTS.md
   "Phoenix emits 8 events per swap"); both WASMs contain the
   concatenated source path
   `contracts/pool/src/contract.rs` followed by exactly:
   `swap`, `sender`, `sell_token`, `offer_amount`, `actual
   received amount`, `buy_token`, `return_amount`, `spread_amount`,
   `referral_fee_amount`. Critically, the space-bearing
   `actual received amount` literal is preserved verbatim in
   both — that string is the riskiest tripwire (Phoenix Q2)
   and it's stable across both pool WASMs.
3. **Source-of-truth alignment.** The binary string
   `contracts/pool/src/contract.rs` matches the upstream
   `Phoenix-Protocol-Group/phoenix-contracts` repo path that the
   audit doc references. Both binaries were built from this same
   source tree.

## Phase 2 results — per-instance walk (executed 2026-04-30)

The wide-net r1 walk covered all 13 Phoenix contracts as part
of its 540-contract watch list. **22 unique WASM hashes**
observed across the [50,457,424, 62,249,727] range — the
most-iterated source we audit. Walk parameters:

- **Workers**: 8 parallel chunks; **runtime**: ~5h.
- **Watch list**: factory + multihop + 11 pool instances
  enumerated from factory `query_pools()` view.

**Per-role findings:**

| Role | Contract count | Unique WASMs | Notes |
| --- | --- | --- | --- |
| Factory | 1 | 5 | Iterates the deploy-time pool template + admin surface; not decoder-relevant |
| Multihop | 1 | 3 | Aggregates pools for cross-pair routing; not decoder-relevant |
| Pool | 11 | 14 | The decoder-relevant set — all 14 emit the audited 8-event swap shape |

**Why 14 pool WASMs across 11 pools.** Pool contracts upgrade
in place via `upgrade(env, new_wasm_hash)`. Several pools have
moved through 2-3 WASMs over the walk window as Phoenix
operators iterated the LP API. The walker captured the full
upgrade chain (`update_current_contract_wasm` transitions per
pool); per-pool transition timelines preserved in the walk's
JSONL output (`/tmp/walk-checkpoint/` on r1).

**Decoder verdict per pool WASM.** Every one of the 14 pool
WASMs was binary-scanned for the 8 swap-field strings the
decoder requires. **All 14 contain all 8 strings**, including
the space-bearing `actual received amount` literal (the
riskiest tripwire per Phoenix Q2). No silent rename across the
upgrade chain. WASM bytes preserved + SHA-256-verified at
`evidence/r1-walk-2026-05-01/wasm-bytes/<hash>.wasm` on r1 for
all 22 hashes; disassembly artifacts (`wasm2wat` + `strings`)
preserved alongside under `evidence/r1-walk-2026-05-01/disasm/`.

**Factory + multihop iteration.** The 5 factory variants and 3
multihop variants are informational — neither contributes to the
trade-emission path the decoder consumes. The factory's pool
template + admin surface evolved alongside the pool API; the
multihop's coordination interface evolved with it. Captured
here for completeness; backfill safety is unaffected.

## Caveats

- **Pool-WASM history walked end-to-end as of 2026-04-30.**
  ~~v2 follow-up~~: ✅ done — see `Phase 2 results` above.
- **New pools deployed after 2026-04-29 are not in this audit.**
  When the factory deploys a new pool, that pool's WASM should be
  one of the two known hashes (deployed-from-template), but if the
  factory has been upgraded with a new pool-WASM-hash setting since
  this audit, the next run of this audit (extending
  `last_verified`) would catch any new hash.

## 2026-07-07 addendum — new Map-body swap schema + QuoteAmount correction

Two findings from the 2026-07-06 lake audit (factory
`("create","liquidity_pool")` walk over `stellar.contract_events` —
**13 pools**, up from the 11 in the 2026-04 walk).

### New pool WASM: single-event Map-body swap (`CBENABXP…`)

A 13th pool,
`CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S`
(factory-created 2026-07-02, immediately after a factory
`("Factory","Updated Config")` event), runs a NEWER pool WASM whose
swap event shape differs from the audited 8-event String schema:

- topic[0] is `ScvSymbol("swap")` (disc 15) — **not** the audited
  `ScvString("swap")` (disc 14).
- ONE event per swap; the body is an `ScvMap` with 8 Symbol keys
  (underscore-spelled): `sender`, `sell_token`, `offer_amount`,
  `buy_token`, `return_amount`, `actual_received_amount`,
  `referral_fee_amount`, `spread_amount`. Note failure-mode #2 above
  (underscore variant of `actual received amount`) is exactly what
  this schema does — it is a distinct shape, not a corruption of the
  String schema.

Decoded via `decodeSwapMap` (map-field-name lookup, no correlation
buffer) and gated via `MainnetMapPools`. Real fixture: ledger
63307899, tx `3cb06db3…`, event_index 3 (golden test
`internal/sources/phoenix/mapswap_test.go`).

**WASM hash: PENDING operator capture.** Run
`stellar contract fetch --id CBENABXP…` + sha256 against mainnet RPC
and record it here. The decode is field-name driven (safe against the
exact hash), but the BackfillSafe audit trail needs the hash before
this pool contributes to any historical backfill range. This audit
could not capture it from the lake — `contract_events` stores events,
not the ContractInstance WASM reference.

### QuoteAmount field-mapping correction (ALL pools)

The audit found the decoder mapped `canonical.Trade.QuoteAmount =
actual received amount`. Per `do_swap`, that field is the INPUT the
pool received of `sell_token` (`balance_after − balance_before`),
byte-identical to `offer_amount` — NOT an output. Every Phoenix trade
therefore had `base_amount == quote_amount` (confirmed live:
237,387/237,387 served rows). Corrected to `return_amount` (the
net-of-fees `buy_token` amount transferred to the taker) 2026-07-07.
Both swap schemas use `return_amount`. **Requires a historical Phoenix
trade re-derive** to fix pre-fix rows.

## 2026-09-19 addendum — Factory create event (audit finding F048)

**Status (2026-09-19): the decoder DOES admit pools from the factory's
create event**, gated on `reg.IsFactory(emitter)` plus both `ScvString`
topics plus an `Address` body. The blocker below was cleared by reading
the upstream factory source; what that does and does NOT buy is spelled
out in "The trust this extends". The operator seed
(`phoenix.MainnetPools` / `MainnetMapPools` / `MainnetStakeContracts`,
plus the `protocol_contracts` warm) remains, as the cold-start warm root
and as the override. **Pools only:** the factory does not announce stake
contracts, so those are still admitted by the seed or the warm alone.

### What real lake captures settle

Fixtures: `test/fixtures/phoenix/factory-create/` (four create events,
2024-05-07 and 2026-07-02), pinned by
`test/controlwiring/phoenix_factory_create_fixture_test.go`.

- The create events **are in the lake** from ledger 51,572,026. Every
  "the factory's creation events predate the lake" statement in the tree
  is false (the 2026-07-07 addendum above already walked them).
- `topic[0]`/`topic[1]` are `ScvString` `("create","liquidity_pool")`,
  so the lake's `topic_0_sym` is **empty** for these rows while the
  Postgres landing zone fills it (`extract.go` uses `GetSym`, the landing
  zone `tryDecodeSymbolOrString`). A ClickHouse creation walk keyed on
  `topic_0_sym` (`gatedPrefilter`, `compute_completeness.go`) matched
  nothing for phoenix. FIXED 2026-09-19: `topic0Predicate`
  (`internal/storage/clickhouse/event_reader.go`) matches the requested
  name in EITHER encoding — the column, or the `ScvString` blob in
  `topics_xdr[1]` — which widens a prefilter without changing what
  `topic_0_sym` means for rows already written (no re-extract implied).
- The body is **one contract `Address`, the pool**. The stake contract
  is never announced, so this event can admit pools and never stakes.
  Stakes stay operator-seeded whatever happens to pools.
- Shape identical in 2024 and 2026. The four announced addresses are
  pools the curated seed already lists, at the ledgers its comments cite.

### The security properties, and how they were established

Auto-admission trusts the event body. That is only sound if:

1. `create_liquidity_pool` is restricted to an allow-list, and the
   restriction is enforced in that function, not merely stored; and
2. the published address is the one the factory **deployed**, not a
   value the caller passed in.

`defindex` self-registration was removed on 2026-08-25 because its
create body carried caller-supplied addresses — a permissionless
registry-poisoning vector. Honest history proves nothing here: four
well-formed past events say nothing about what a hostile caller can
make the factory publish. So the properties were read off the source,
not inferred from the captures.

**Verified 2026-09-19 from the public upstream source**
(`Phoenix-Protocol-Group/phoenix-contracts`,
`contracts/factory/src/contract.rs` + `utils.rs`), fetched raw and read
directly at `main`, `v2.0.0`, `v1.1.0` and `v1.0.0`. All four agree:

1. **Allow-listed creators only.** `create_liquidity_pool` opens with
   `sender.require_auth()`, then
   `if !get_config(&env).whitelisted_accounts.contains(sender) {
   panic_with_error!(.., NotAuthorized) }`. The allow-list itself changes
   only under the admin's auth.
2. **The address is factory-DEPLOYED, never caller-supplied.**
   `lp_contract_address` is the RETURN of
   `env.deployer().with_current_contract(salt).deploy_v2(<wasm hash from
   the factory's own config>, init_args)` (v1.x: `deploy_lp_contract` →
   `…with_current_contract(salt).deploy(lp_wasm_hash)`), with
   `salt = sha256(token_a ‖ token_b)` (Blend pools prefix a type byte).
   **The function takes no pool-address parameter at all** — this is
   precisely what `defindex`'s announcement could not promise.
3. **The event publishes exactly that address**, and
   `("create", "liquidity_pool")` is the ONLY `("create", …)` publish in
   the factory. Emitted from inside the factory, so the event's emitting
   contract id IS the factory — which is why gating on
   `reg.IsFactory(emitter)` cannot be satisfied by a foreign emitter.
4. **The stake contract is not announced by the factory.** The factory
   passes `stake_wasm_hash` into the pool's init args and the POOL
   deploys its stake contract.
5. **There is no `create_liquidity_pool_v2`** at any of the four
   versions — see the residual below, which contradicts the in-repo
   disassembly.

### The trust this extends

Say it plainly, because auto-admission makes it automatic rather than
deliberate:

- **The source is not the installed WASM.** The factory is
  admin-upgradeable (`update_current_contract_wasm` under
  `admin.require_auth()`), and the currently installed bytes were not
  hashed against a build (see the residual below). Admission therefore
  trusts **the Phoenix factory admin**: a malicious upgrade could
  publish an arbitrary address. This is the same trust the curated seed
  already extended to Phoenix pools by hand — it is now automatic, and
  it is the reason the operator seed stays the override.
- **It is identity trust, not price trust.** A whitelisted creator picks
  `token_a` / `token_b`, so an admitted pool may trade a worthless
  asset. Admission decides whose events are *attributed to Phoenix*;
  what reaches a served price is the downstream pricing guards' problem.
- **Pools only.** Stake contracts are out of this event's reach
  whatever happens to pools (point 4), so they remain operator-seeded.

Worth having, not done here: an alert on a factory WASM upgrade.

### Residual: the installed bytes were not hashed

The source review above did not identify WHICH build is installed at
`CB4SVAWJ…CKMI`. Evidence from the in-repo walk
(`evidence/r1-walk-2026-05-01/disasm/*.json`, all five factory
variants), and its limits:

| factory WASM (first 16) | observed ledgers | allow-list surface in exports / strings |
| --- | --- | --- |
| `e1464afcf0c7c01e` | 51,572,016 – 51,937,331 | `update_whitelisted_accounts`, `whitelisted_accounts` |
| `96c6a73863de6e33` | 53,134,143 – 53,417,239 and 53,649,587 – 54,517,224 | `update_whitelisted_accounts`, `whitelisted_accounts` |
| `2bbb91c58cb8432f` | 54,517,225 – 54,517,363 | same, plus `create_liquidity_pool_v2`, `remove_pool` |
| `721badb85470a81d` | 54,517,364 – 54,897,147 | same, plus `create_liquidity_pool_v2`, `remove_pool` |
| `c54ba54bd9e37503` | 57,406,830 – 57,856,963 | **no** `update_whitelisted_accounts`; `whitelisted_to_add` / `whitelisted_to_remove` under `update_config`; `__constructor`, `propose_admin` |

- This shows an allow-list **exists as state** in every known variant,
  consistent with the source. An export table and a string table still
  cannot show the list is *checked* inside `create_liquidity_pool` —
  that comes from the source, above, not from here.
- **The walk is stale.** Its last observed factory range ends at ledger
  57,856,963; the newest captured create is at 63,293,708, preceded by a
  `("Factory","Updated Config")` at 63,293,663. Every variant exports
  `update`, so the factory may be running a sixth WASM this walk never
  saw. The observed ranges also have holes (51,937,331 → 53,134,143,
  53,417,239 → 53,649,587 and 54,897,147 → 57,406,830).
- **One disagreement worth naming.** Two disassembled variants
  (`2bbb91c5…`, `721badb8…`, ledgers 54,517,225 – 54,897,147) export
  `create_liquidity_pool_v2`, which exists in NONE of the four upstream
  versions read. Either those builds pre-date a rename, or they are off
  the tags that were checked. The admission gate does not depend on
  which: it keys on the emitter being the factory and on the topic pair,
  and `("create","liquidity_pool")` is the only `("create", …)` publish
  in every source version seen. But a `_v2` entry point that publishes
  the same topics from a build nobody has read is exactly the shape this
  residual covers, and it is the reason the follow-up below is worth
  doing rather than optional.

### What shipped, and what is still owed

Shipped (2026-09-19):

- `internal/sources/phoenix`: `classifyAny` gained `actionCreatePool`
  for the exact pair `("create","liquidity_pool")` — any other
  `("create", …)` stays `actionUnknown` and fail-closes into a
  recognition gap. `Matches` gates that action on `reg.IsFactory`, never
  `reg.Has` (a curated pool already passes `reg.Has` and would otherwise
  be the strongest forger). `Decode` seeds the announced pool with the
  factory as provenance, before taking the correlation-buffer lock.
- `internal/storage/clickhouse`: the topic[0] prefilter matches both
  on-wire encodings, so the lake walk keyed on `"create"` returns rows
  (see the note on `topic_0_sym` above). Phoenix's reconcile-catalogue
  entry now sets `factories` + `creationSym` so `gatedPrefilter` runs.
- Tests: `TestK023_PhoenixFactoryCreateEventIsAdmissible` and
  `…FromForeignEmitterIsNotAdmitted` GRADUATED out of `-tags
  k023evidence` into the default suite
  (`test/controlwiring/phoenix_factory_admission_test.go`), and
  `test/integration/contract_events_string_topic_prefilter_test.go`
  proves the lake → prefilter → decoder → gate loop against a real
  ClickHouse.

Still owed, and not done here:

1. `stellar contract fetch --id CB4SVAWJ…` against mainnet, sha256, and
   record the currently installed factory hash here — closing the
   residual above. An admin upgrade can invalidate either property
   later, so this verification is per installed hash, the same standing
   caveat as the pool WASMs above. An alert on a factory WASM upgrade is
   the durable form of it.
2. Re-derive the history this unblocks. Pools announced since ledger
   51,572,026 that the curated seed never got have lake events that were
   never attributed. `seed-protocol-contracts` for phoenix ENUMERATES
   them (it was inert before this change); `projected-rebuild -source
   phoenix … -write` re-derives them. Phoenix is `BackfillSafe: true`.

One thing NOT to do, recorded because it was considered and rejected:
"recognise without admitting" is not a stop-gap. `ch-recognition` tests
ONE exemplar per `(contract_id, topic_0_sym)` shape through the decoder
chain's `Matches`, and the factory's String-topic events all share the
empty `topic_0_sym`, so they are a single shape. Whenever its exemplar
is a create event, a decoder that matched it and dropped it would report
the factory as recognised while admitting nothing — muting the one audit
that can surface a new pool today, with the control still inert.

## Decision

**`BackfillSafe: true`** — flipped in
`internal/sources/external/registry.go` in this PR.

Rationale:

- All 11 currently-deployed mainnet pools enumerated and audited.
- 2 unique WASM hashes; both contain all 8 expected event-field
  string literals; both have identical contract interfaces.
- Decoder's strict "all 8 events per swap" correlation works
  identically against both hashes.
- Live ingest from production health: 0 `ErrIncompleteSwap` /
  `ErrMalformedPayload` rate spikes — empirical confirmation that
  the current decoder + current pool WASMs work in production.
- Caveats above are all v2-follow-up scope; the load-bearing
  evidence (binary-string verification of the 8 field literals)
  is the audit's primary safety claim.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/phoenix/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/contract-schema-evolution.md`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["phoenix"].BackfillSafe`
- Upstream contract source: `https://github.com/Phoenix-Protocol-Group/phoenix-contracts`
