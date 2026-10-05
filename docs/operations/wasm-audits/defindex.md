---
title: DeFindex WASM-history audit
last_verified: 2026-07-06
status: complete — BackfillSafe=true (audited 2026-05-19, live-verified post-rc.58 deploy)
source: defindex
backfill_safe: true
---

# DeFindex WASM audit

Audit log for the `defindex` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

## Status
**BLOCKED — audit FAIL (2026-05-19).** The per-WASM-hash walk +
disassembly **failed**: the deployed mainnet vault WASM does not emit the
events the `internal/sources/defindex/` decoder matches on. `BackfillSafe`
stays `false`; **live defindex decoding is almost certainly producing
nothing** (see "Audit result"). Unblocking requires re-deriving the
decoder from the *actually-deployed* contract, not the paltalabs
tag-`1.0.0` reference. Tracked as Task #28.

DeFindex is a yield-aggregator vault system from
[paltalabs/defindex](https://github.com/paltalabs/defindex). Vaults route
user capital into yield protocols (currently Blend) via per-vault
`Strategy` contracts. We capture vault `deposit` / `withdraw` events for
flow attribution; vaults emit no price-discovery trades and never
contribute to VWAP.
## Contracts under audit

From `internal/sources/defindex/events.go` (cross-checked against
`paltalabs/defindex` tag `1.0.0` on 2026-05-14):

| role | contract / hash |
| --- | --- |
| Factory | `CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI` |
| USDC autocompound vault | `CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP` |
| EURC autocompound vault | `CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI` |
| XLM autocompound vault | `CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z` |
| Vault WASM hash (paltalabs tag 1.0.0 — **NOT what's deployed**) | `0f3073517cbfacbfd482bc166cff38a0e7abeab9b7ee77334abab45880fb8f3a` |
| Vault WASM hash (**actually deployed on mainnet**, walk-confirmed) | `11329c2469455f5a3815af1383c0cdddb69215b1668a17ef097516cde85da988` |
| BlendStrategy WASM hash (tag 1.0.0 ref) | `65ee2e1b32ff39a6c8f8572dd0d6d2db7952be6d54c740bfb1d6eab6dd209dc0` |

Deployed vault WASM `11329c24...988` is shared by all three Phase-A
vaults (same template, different assets / Blend pools): 2026-05-19 r1
wasm-history walk, single hash, **zero mid-life upgrades**. **Critically,
it is NOT the `0f3073...8f3a` hash** the decoder + this doc were first
written against (paltalabs tag `1.0.0`, a different version than mainnet).
See "Audit result".

## Decoder expectations

From `internal/sources/defindex/{events,decode}.go` at HEAD as of
2026-05-14. Any divergence in a deployed WASM hash is an audit finding.

### Topic structure

Vault events have a 2-element topic:

```text
topic[0] = ScvString("DeFindexVault")    — 13 chars, exceeds symbol_short!'s 9-char cap
topic[1] = ScvSymbol(event_name)
  — Phase-A decodes:
    "deposit"   → user-facing flow into the vault
    "withdraw"  → user-facing flow out of the vault
    "dfees"     → per-token fee distribution (defindex_fees)
    "rescue", "paused", "unpaused", "nreceiver",
    "nmanager", "nemanager", "rbmanager"
                → admin events (defindex_admin_events)
  — Recognised, not decoded:
    "rebalance" (multiplexed body — discriminate by
                 `rebalance_method` field inside body)
```

### Body shapes

`deposit` and `withdraw` bodies are `ScvMap` keyed by field-name `Symbol`
(decode-by-name per docs/architecture/ingest-pipeline.md#contract-schema-evolution).
Phase-A pulls only the user-facing dimensions:

| event | body fields decoded |
| --- | --- |
| `deposit` | `depositor: Address`, `amounts: Vec<i128>`, `df_tokens_minted: i128` |
| `withdraw` | `withdrawer: Address`, `amounts_withdrawn: Vec<i128>`, `df_tokens_burned: i128` |
| `rescue` | `caller: Address`, `strategy_address: Address`, `amount_withdrawn: i128` |
| `paused` / `unpaused` | `caller: Address`, `strategy_address: Address` |
| `nreceiver` | `caller: Address`, `new_fee_receiver: Address` |
| `nmanager` | `new_manager: Address` |
| `nemanager` | `new_emergency_manager: Address` |
| `rbmanager` | `new_rebalance_manager: Address` |

Admin shapes come from lake samples pinned in
`test/fixtures/defindex/vault-admin-2026-09-30/`; every listed field is
required, so a new WASM that renames one fails loudly as malformed. The
samples do not record each emitter's WASM hash. Before a
`projector-replay -source defindex` over admin history, decode every
admin event in the lake with this decoder, require zero malformed, and
record the vault WASM hashes seen.

The body also carries `total_supply_before` and
`total_managed_funds_before` (NAV reconstruction); ignored at Phase A.
`amounts` is a vec (multi-asset vaults); the Phase-A trio (USDC / EURC /
XLM) are single-asset (length 1), but the decoder doesn't hardcode that.

### Surprising gotchas (catalogued during the upstream research)

1. **Topic[0] is `ScvString`, not `ScvSymbol`** (as Soroswap's
   `"SoroswapPair"` / `"SoroswapFactory"`); see the
   `internal/sources/defindex/events.go` `scval.MustEncodeString` call.
2. **Factory `create` event body lacks the new vault address**
   (`apps/contracts/factory/src/lib.rs:205-231` at tag 1.0.0): `create_vault_internal` returns it but the body
   carries only `roles / vault_fee / assets`. Phase-B follow-up: plumb the
   InvokeContract op return value via `events.Event.OpArgs` (as Band /
   Redstone).
3. **Four rebalance event bodies share one topic.** `unwind`, `invest`,
   `SwapExactIn`, `SwapExactOut` all publish on
   `("DeFindexVault","rebalance")`; discriminate by the
   `rebalance_method` Symbol field in the body. Not needed at Phase A.
4. **Strategy events fire from the strategy contract, not the vault.** A
   tx emitting a vault `deposit` also emits `("BlendStrategy","deposit")`
   from the per-vault strategy, then a Blend `("Pool","supply")`;
   correlated by `tx_hash` + `op_index`. Phase A decodes only the vault layer.
5. **`from` on strategy events is the vault address**, not the end-user;
   attribution needs the vault event in the same tx (Phase B).

## Audit result (2026-05-19) — FAIL

Walk: the recovered canonical `merged.json` from the 2026-05-19 r1 wasm-history walk.

1. **WASM identity (passed).** Factory `CDKFHFJI...NFKI` first-deploy
   `L57,056,338`; vaults `CDB2WMKQ...` L57,056,388 / `CC5CE6MW...`
   L57,056,390 / `CDPWNUW7...` L57,056,392 all run a **single shared**
   WASM `11329c24...988`, **zero mid-life upgrades**. Staggered deploy
   ledgers are genuine first-deploy points, not the walk's lower bound.
   (`sourceGenesisLedger["defindex"]` corrected to the factory's
   `57_056_338`; see `internal/api/v1/diagnostics_ingestion.go`.)

2. **Decoder ↔ deployed-WASM check (FAILED).** Vault WASM `11329c24...988`
   was extracted from galexie (sha256-verified) and scanned. The decoder
   (`internal/sources/defindex/`) and "Decoder expectations" require
   topic[0] = `ScvString("DeFindexVault")` and `ScvMap` bodies keyed
   `depositor` / `amounts` / `df_tokens_minted` (deposit) and `withdrawer` /
   `amounts_withdrawn` / `df_tokens_burned` (withdraw).

   In the verified deployed bytes:
   - `deposit` and `withdraw` appear.
   - **`DeFindexVault` is ABSENT.** 13 chars: cannot be a packed
     `SymbolSmall` (9-char cap) or built at runtime; a published topic
     would put the literal in the WASM.
   - **Every documented body field is ABSENT** (`depositor`,
     `amounts`, `df_tokens_minted`, `withdrawer`,
     `amounts_withdrawn`, `df_tokens_burned`).

3. **Live corroboration.** `aggregator_exposures` (defindex's only sink
   table) is **empty (0 rows)** on r1 though the vaults are live since
   `L57,056,388`.

**Root cause:** decoder + this doc were written against
`paltalabs/defindex` tag `1.0.0` (vault hash `0f3073...8f3a`). Mainnet
runs a *different* version (`11329c24...988`) with different
deposit/withdraw topic + body schema.

## Resolution (2026-05-19) — decoder re-derived from real on-chain

1. **Disassembly.** `wasm2wat` of `11329c24...988` shows **Blend strategy
   code** (`BlendStrategy`, `blend_pool_address`, `harvest`, `keeper`,
   `__constructor`; no `DeFindexVault` / vault strings). The three curated
   "vault" addresses are strategy contracts.
2. **Real schema on-chain.** `stellarindex-ops scan-soroban-events`
   (commit `57781f59`) against galexie LCM showed:
   - `("BlendStrategy","deposit")` body `ScvMap{from:Address,
     amount:i128}` (e.g. L57,056,389; 27/40 in a recent window)
   - `("BlendStrategy","withdraw")` body `ScvMap{from:Address,
     amount:i128}` (13/40 in a recent window)
   `from` is an account *or* contract strkey; `scval.AsAddressStrkey`
   renders both.
3. **Decoder rewritten** (`internal/sources/defindex/{events,
   decode,dispatcher_adapter,consumer}.go`): topic[0] ==
   `ScvString("BlendStrategy")`, topic[1] ∈ {deposit,withdraw}, body
   decode-by-name `{from, amount}`, **dispatched by topic across every
   emitter** (comet/aquarius shared-emitter topology). Tests regenerated
   from the real schema; `go test -race` green. The fictional
   `MainnetVault*` / `MainnetVaultWASMHash` / factory consts were deleted.

## Audit closure (2026-05-19) — PASS, `BackfillSafe: true`

1. **Live-verify on r1: PASS.** Post-rc.58 deploy, the indexer emits
   `defindex strategy flow` INFO lines on real traffic (9 events in a
   90-min window); topic dispatch `("BlendStrategy", deposit|withdraw)`
   matches the deployed contract's emissions.
2. **WASM re-audit vs `11329c24...988`: PASS.** `wasm2wat` data-section
   scan confirms every required symbol: `BlendStrategy` (topic[0] string,
   the 13-char literal whose ABSENCE diagnosed the tag-1.0.0 fiction),
   `deposit`, `withdraw`, `from`, `amount`. Single shared hash across all
   3 vaults' lives, zero mid-life upgrades (2026-05-19 walk's `merged.json`).

## Phase-B extension (2026-05-21) — vault-wrapper layer added

The 2026-05-19 audit closed against the strategy WASM and three named
"fixed-strategy" contracts. A 2026-05-21 cross-check vs Soroban-RPC
`getEvents` showed that was half the coverage: every defindex flow goes
through TWO contracts.

**The two layers (now both decoded):**

| layer | topic[0] | contract WASM | `from` / `user` field | purpose |
| --- | --- | --- | --- | --- |
| strategy | `BlendStrategy` | `11329c24…988` | vault contract C-strkey | underlying capital movement |
| vault wrapper | `DeFindexVault` | `ae3409a4…468b` (initial) / `07097f83…84b0` (upgraded) | end-user G-strkey (occasionally aggregator C-strkey) | user-facing entry point |

**Why the original audit missed the vault layer:**

1. **`mainnet.contracts.json` lists strategy addresses, not vaults.** The
   manifest is organised around products; vault wrappers (`CCA2ZJP5…`,
   `CBNKCU3H…`, plus ~100 more spawned by factory `CDKFHFJI…NFKI`) are
   deployed on demand, one per user investment, and aren't listed.
2. **The walk that found `11329c24…988` only walked manifest addresses.**
   Walking the factory's `create` events was a flagged Phase-B follow-up
   not executed.
3. **`From` on strategy events was documented as "may be contract
   address," not "is always."** Every strategy-layer event has a vault
   contract as `from`; users always go through a vault wrapper.

**Cross-check that surfaced the gap:**

| ledger window | RPC events | indexer journal | coverage |
| --- | --- | --- | --- |
| pre-rc.63 (before walker fix 1b1e46a09 deployed 10:45 CEST 2026-05-21) | 78 | 11 | 14% |
| post-rc.63 (walker active) | 15 | 15 | 100% (strategy-layer events only) |
| total in 12-hour audit window | 93 | 26 | 27% |

The post-rc.63 100% covers *only strategy-layer events that fire as
sub-invocations*; vault-layer `DeFindexVault` events weren't filtered into
the dispatcher (topic prefix not listed).

**Phase B addition (decoder revision, 2026-05-21):**

- Added `PrefixVault = "DeFindexVault"` to `events.go` beside `PrefixStrategy`.
- Added `classifyVault()` + `decodeVaultFlow()` to `decode.go`, decoding
  `{depositor|withdrawer, amounts|amounts_withdrawn,
  df_tokens_minted|df_tokens_burned, total_*_before}`; the
  `total_*_before` fields are intentionally ignored at Phase B (NAV later).
- `Decoder.Matches` true for either prefix; `Decode` emits `Event`
  (strategy) or `VaultEvent` (vault).
- Sink logs distinct `msg` tags (`"defindex strategy flow"` /
  `"defindex vault flow"`).
- Topic-based dispatch (no address hardcoding) decodes every current AND
  future vault wrapper the factory spawns (shared-emitter topology).

**Phase-B follow-up (BACKLOG #58, 2026-07-06):** harvest / rebalance /
the eight admin topics now **drop cleanly**: `Decode` returns `(nil, nil)`
for every recognised-but-unmodelled topic, like factory events, instead of
the old `ErrUnknownEvent` path (which counted normal upstream traffic
against the decode-error counter). `defindex.DecodeRebalanceMethod` reads
the `rebalance_method` discriminator and `RebalanceMethod.Known()`
classifies the four documented methods. **No new `consumer.Event` type,
hypertable, or projector wiring was added**, deliberately: per-method
payloads are unmodelled (below; discriminator read by `DecodeRebalanceMethod`).

**Still out of scope (Phase C+), blocked on real on-chain samples:**

- Body decode for `("BlendStrategy","harvest")`: body never observed
  on-chain, NOT modelled (no invented layouts). Recognised + clean-dropped.
- Per-method payload decode for `("DeFindexVault","rebalance")`: the four
  bodies (`unwind` / `invest` / `SwapExactIn` / `SwapExactOut`) are
  unmodelled. Lake samples exist only for `rebalance_method = "invest"`
  (`asset_investments`, `report` fields); none for the other three.
- Body decode for `("DeFindexFactory","create"|"n_fee")` vault-spawn
  events (topic classified per EVERY-event policy, F-0018 closed
  2026-05-28; `Decode` returns `(nil, nil)` on a factory match). The
  useful signal (new wrapper address) needs `events.Event.OpArgs` from the
  InvokeContract op (Surprising-gotcha #2), as Band's `relay()` and
  Redstone's `write_prices()`.
- A typed `defindex_flows` hypertable (today the counter is the only
  after-the-fact record, so historical recovery needs a re-backfill, not SQL).

`BackfillSafe: true` flipped in `internal/sources/external/registry.go`.
`stellarindex-ops backfill --source=defindex` is now unblocked. Per
AGENTS.md's "Soroban DeFi contracts upgrade in place" rule, any future
`update_contract` on the strategy contracts must trigger a new audit
(re-check the new hash's data section).

The *factory* `b0fe36b2...0e` (first-deploy `L57,056,338`) needs no
decoder (dispatch is by strategy topic). Code-upload predates the walk
window; walk-confirmed single-hash, zero upgrades over its observed life.
