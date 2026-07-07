---
title: DeFindex WASM-history audit
last_verified: 2026-07-06
status: complete — BackfillSafe=true (audited 2026-05-19, live-verified post-rc.58 deploy)
source: defindex
backfill_safe: false
---

# DeFindex WASM audit

Audit log for the `defindex` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

## Status

**BLOCKED — audit FAIL (2026-05-19).** The per-WASM-hash walk +
disassembly landed and the audit **failed**: the deployed mainnet
vault WASM does not emit the events the
`internal/sources/defindex/` decoder matches on. `BackfillSafe`
stays `false`; **live defindex decoding is almost certainly
producing nothing** (see "Audit result" below). Unblocking
requires re-deriving the decoder from the *actually-deployed*
contract, not the paltalabs tag-`1.0.0` reference it was written
against. Tracked as Task #28.

DeFindex is a yield-aggregator vault system from
[paltalabs/defindex](https://github.com/paltalabs/defindex).
Vaults hold user-deposited capital and route it into underlying
yield protocols (currently Blend) via per-vault `Strategy`
contracts. We capture vault `deposit` / `withdraw` events for
flow attribution; the vaults do **not** emit price-discovery
trades and never contribute to VWAP.

## Contracts under audit

Captured from `internal/sources/defindex/events.go` (cross-checked
against `paltalabs/defindex` tag `1.0.0` on 2026-05-14):

| role | contract / hash |
| --- | --- |
| Factory | `CDKFHFJIET3A73A2YN4KV7NSV32S6YGQMUFH3DNJXLBWL4SKEGVRNFKI` |
| USDC autocompound vault | `CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP` |
| EURC autocompound vault | `CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI` |
| XLM autocompound vault | `CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z` |
| Vault WASM hash (paltalabs tag 1.0.0 — **NOT what's deployed**) | `0f3073517cbfacbfd482bc166cff38a0e7abeab9b7ee77334abab45880fb8f3a` |
| Vault WASM hash (**actually deployed on mainnet**, walk-confirmed) | `11329c2469455f5a3815af1383c0cdddb69215b1668a17ef097516cde85da988` |
| BlendStrategy WASM hash (tag 1.0.0 ref) | `65ee2e1b32ff39a6c8f8572dd0d6d2db7952be6d54c740bfb1d6eab6dd209dc0` |

The deployed vault WASM hash `11329c24...988` is shared by all
three Phase-A vaults (same template, different underlying
assets / Blend pools) — confirmed by the 2026-05-19 r1
wasm-history walk, single hash, **zero mid-life upgrades** over
each vault's entire life. **Critically, this is NOT the
`0f3073...8f3a` hash the decoder + this doc were originally
written against** (that hash came from `paltalabs/defindex` tag
`1.0.0` — a different contract version than mainnet runs). See
"Audit result" for why this matters.

## Decoder expectations

Captured from `internal/sources/defindex/{events,decode}.go` at
HEAD as of 2026-05-14. Any divergence from these in a deployed
WASM hash is an audit finding.

### Topic structure

Vault events have a 2-element topic:

```text
topic[0] = ScvString("DeFindexVault")    — 13 chars, exceeds symbol_short!'s 9-char cap
topic[1] = ScvSymbol(event_name)
  — Phase-A decodes:
    "deposit"   → user-facing flow into the vault
    "withdraw"  → user-facing flow out of the vault
  — Phase-B follow-ups (not yet decoded):
    "rescue", "paused", "unpaused", "nreceiver",
    "nmanager", "nemanager", "rbmanager", "dfees",
    "rebalance" (multiplexed body — discriminate by
                 `rebalance_method` Symbol field inside body)
```

### Body shapes

Both `deposit` and `withdraw` bodies are `ScvMap` keyed by
field-name `Symbol` (decode-by-name per
docs/architecture/contract-schema-evolution.md). Phase-A pulls
only the user-facing dimensions:

| event | body fields decoded |
| --- | --- |
| `deposit` | `depositor: Address`, `amounts: Vec<i128>`, `df_tokens_minted: i128` |
| `withdraw` | `withdrawer: Address`, `amounts_withdrawn: Vec<i128>`, `df_tokens_burned: i128` |

The body also carries `total_supply_before` and
`total_managed_funds_before` (for accurate NAV reconstruction);
we ignore these at Phase A.

`amounts` is a vec because DeFindex supports multi-asset vaults.
The Phase-A trio (USDC / EURC / XLM autocompound) are all
single-asset, so the vec has length 1 in practice — but the
decoder doesn't hardcode that.

### Surprising gotchas (catalogued during the upstream research)

1. **Topic[0] is `ScvString`, not `ScvSymbol`.** Same encoding
   pattern as Soroswap (`"SoroswapPair"` / `"SoroswapFactory"`).
   Confirmed via the `internal/sources/defindex/events.go`
   `scval.MustEncodeString` call.
2. **Factory `create` event body lacks the new vault address.**
   Captured in `apps/contracts/factory/src/lib.rs:205-231` at tag
   1.0.0 — `create_vault_internal` returns the vault address but
   the event body only carries `roles / vault_fee / assets`.
   Phase-B follow-up: plumb the InvokeContract op return value via
   `events.Event.OpArgs` (same pattern Band / Redstone use).
3. **Four distinct rebalance event bodies share one topic.** All
   four (`unwind`, `invest`, `SwapExactIn`, `SwapExactOut`)
   publish on `("DeFindexVault","rebalance")`. Discriminate by
   the `rebalance_method` Symbol field inside the body. Not
   needed at Phase A but worth noting before any future
   rebalance-decode work.
4. **Strategy events fire from the strategy contract, not the
   vault.** The same tx that emits a vault `deposit` will also
   emit a `("BlendStrategy","deposit")` from the per-vault strategy
   contract — and from there a Blend `("Pool","supply")`. All
   three are correlated by `tx_hash` + `op_index`. Phase A only
   decodes the vault layer.
5. **`from` field on strategy events is the vault address**, not
   the end-user. End-user attribution requires correlating with
   the vault event in the same tx (Phase B).

## Audit result (2026-05-19) — FAIL

Walk: the recovered canonical `merged.json` from the 2026-05-19
r1 wasm-history walk.

1. **WASM identity (this part passed).** Factory
   `CDKFHFJI...NFKI` first-deploy `L57,056,338`; the three vaults
   (`CDB2WMKQ...` L57,056,388 / `CC5CE6MW...` L57,056,390 /
   `CDPWNUW7...` L57,056,392) all run a **single shared** WASM
   hash `11329c24...988` with **zero mid-life upgrades** over
   their entire lives. The staggered deploy ledgers confirm these
   are genuine first-deploy points, not the walk window's lower
   bound. (`sourceGenesisLedger["defindex"]` corrected to the
   factory's `57_056_338` accordingly — see
   `internal/api/v1/diagnostics_ingestion.go`.)

2. **Decoder ↔ deployed-WASM check (this part FAILED).** The
   vault WASM `11329c24...988` was extracted from galexie
   (sha256-verified) and its bytes scanned. The decoder
   (`internal/sources/defindex/`) and the "Decoder expectations"
   section above require topic[0] = `ScvString("DeFindexVault")`
   and `ScvMap` bodies keyed `depositor` / `amounts` /
   `df_tokens_minted` (deposit) and `withdrawer` /
   `amounts_withdrawn` / `df_tokens_burned` (withdraw).

   In the verified deployed bytes:
   - `deposit` and `withdraw` appear.
   - **`DeFindexVault` is ABSENT.** It is 13 chars — it cannot be
     a packed `SymbolSmall` (9-char cap) and cannot be
     reconstructed at runtime; if the contract published that
     topic the literal would be in the WASM. It is not.
   - **Every documented body field is ABSENT** (`depositor`,
     `amounts`, `df_tokens_minted`, `withdrawer`,
     `amounts_withdrawn`, `df_tokens_burned`).

3. **Live corroboration.** `aggregator_exposures` (defindex's
   only sink table) is **empty (0 rows)** on r1 despite the
   vaults being live since `L57,056,388` — consistent with a
   decoder that never matches the deployed contract's events.

**Root cause:** the decoder + this doc were written against
`paltalabs/defindex` tag `1.0.0` (vault hash `0f3073...8f3a`).
Mainnet runs a *different* version (`11329c24...988`) whose
deposit/withdraw event topic + body schema differ. Decoding by
name against the wrong reference yields no matches.

## Resolution (2026-05-19) — decoder re-derived from real on-chain

The mismatch was diagnosed and the decoder rewritten:

1. **Disassembly.** `wasm2wat` of the verified deployed WASM
   `11329c24...988` showed it is **Blend strategy code**
   (`BlendStrategy`, `blend_pool_address`, `harvest`, `keeper`,
   `__constructor`; no `DeFindexVault` / vault strings). The three
   curated "vault" addresses are strategy contracts.
2. **Real schema captured on-chain.** `stellarindex-ops
   scan-soroban-events` (the new in-infra event dumper, commit
   `57781f59`) against galexie LCM showed the contracts emit:
   - `("BlendStrategy","deposit")` body `ScvMap{from:Address,
     amount:i128}` (e.g. L57,056,389; 27/40 in a recent window)
   - `("BlendStrategy","withdraw")` body `ScvMap{from:Address,
     amount:i128}` (13/40 in a recent window)
   `from` is an account *or* contract strkey;
   `scval.AsAddressStrkey` renders both.
3. **Decoder rewritten** (`internal/sources/defindex/{events,
   decode,dispatcher_adapter,consumer}.go`): topic[0] ==
   `ScvString("BlendStrategy")`, topic[1] ∈ {deposit,withdraw},
   body decode-by-name `{from, amount}`, **dispatched by topic
   across every emitter** (not the old 3-contract set) —
   comet/aquarius shared-emitter topology. Tests regenerated from
   the real schema; `go test -race` green. The fictional
   `MainnetVault*` / `MainnetVaultWASMHash` / factory consts were
   deleted.

## Audit closure (2026-05-19) — PASS, `BackfillSafe: true`

Both gating steps satisfied:

1. **Live-verify on r1 — PASS.** Post-rc.58 deploy, the indexer
   emits `defindex strategy flow` INFO log lines against real
   on-chain traffic (sample: 9 events in a 90-min window,
   accumulating steadily). The rewritten decoder's topic-based
   dispatch (`("BlendStrategy", deposit|withdraw)`) matches the
   deployed contract's actual emissions.
2. **WASM re-audit vs `11329c24...988` — PASS.** `wasm2wat`
   data-section scan of the verified deployed bytes confirms every
   required symbol present: `BlendStrategy` (topic[0] string —
   the 13-char literal whose ABSENCE in the previous audit
   diagnosed the tag-1.0.0 fiction), `deposit`, `withdraw`,
   `from`, `amount`. Decoder ↔ deployed-WASM byte-correspondence
   established for the strategy code (`11329c24...988`, single
   shared hash across all 3 vaults' on-chain lives, zero mid-life
   upgrades from the 2026-05-19 walk's `merged.json`).

## Phase-B extension (2026-05-21) — vault-wrapper layer added

The 2026-05-19 audit closed against the strategy WASM and the
three named "fixed-strategy" vault contracts. A 2026-05-21
cross-check vs Soroban-RPC `getEvents` revealed that closure was
half the coverage: every defindex event flow goes through TWO
contracts, and the strategy layer only sees half of them.

**The two layers (now both decoded):**

| layer | topic[0] | contract WASM | `from` / `user` field | purpose |
| --- | --- | --- | --- | --- |
| strategy | `BlendStrategy` | `11329c24…988` | vault contract C-strkey | underlying capital movement |
| vault wrapper | `DeFindexVault` | `ae3409a4…468b` (initial) / `07097f83…84b0` (upgraded) | end-user G-strkey (occasionally aggregator C-strkey) | user-facing entry point |

**Why the original audit missed the vault layer:**

1. **`mainnet.contracts.json` lists strategy addresses, not vault
   addresses.** The protocol team's public deployment manifest is
   organised around investment products ("USDC blend autocompound
   strategy") — each entry is a strategy contract. The vault
   wrappers (`CCA2ZJP5…`, `CBNKCU3H…`, plus ~100 more spawned by
   the factory `CDKFHFJI…NFKI`) are deployed on demand, one per
   user investment, and don't appear in the manifest.
2. **The walk that found `11329c24…988` only walked the manifest
   addresses.** Walking the factory's `create` events (each spawns
   a vault wrapper) was a Phase-B follow-up the original audit
   flagged but didn't execute.
3. **`From` field on strategy events was documented as "may be
   contract address," not "is always contract address."** In
   practice every strategy-layer event has a vault contract as
   `from` — there is no direct-to-strategy user flow path in the
   deployed protocol. The user always interacts with a vault
   wrapper, which then delegates to a strategy.

**Cross-check that surfaced the gap:**

| ledger window | RPC events | indexer journal | coverage |
| --- | --- | --- | --- |
| pre-rc.63 (before walker fix #48 deployed 10:45 CEST 2026-05-21) | 78 | 11 | 14% |
| post-rc.63 (walker active) | 15 | 15 | 100% (strategy-layer events only) |
| total in 12-hour audit window | 93 | 26 | 27% |

The post-rc.63 100% above is *only across strategy-layer events
that fire as sub-invocations*. Vault-layer `DeFindexVault` events
weren't even being filtered into the dispatcher — the decoder
didn't list that topic prefix.

**Phase B addition (decoder revision, 2026-05-21):**

- Added `PrefixVault = "DeFindexVault"` to `events.go` alongside
  the existing `PrefixStrategy`.
- Added `classifyVault()` + `decodeVaultFlow()` to `decode.go`,
  decoding the `{depositor|withdrawer, amounts|amounts_withdrawn,
  df_tokens_minted|df_tokens_burned, total_*_before}` schema. The
  `total_*_before` fields are intentionally ignored at Phase B
  (NAV reconstruction is later scope).
- `Decoder.Matches` returns true for either topic-prefix; `Decode`
  routes to the appropriate decoder and emits `Event` (strategy
  layer) or `VaultEvent` (vault layer).
- Sink logs both with distinct `msg` tags
  (`"defindex strategy flow"` / `"defindex vault flow"`) so
  operators can grep either layer independently.
- Topic-based dispatch (no contract address hardcoding) means
  every current AND future vault wrapper the factory spawns gets
  decoded automatically — same shared-emitter topology as
  comet/aquarius and the strategy layer.

**Phase-B follow-up (BACKLOG #58, 2026-07-06):** harvest / rebalance /
the eight admin topics now **drop cleanly** — `Decode` returns
`(nil, nil)` for every recognised-but-unmodelled topic, exactly like
factory events, instead of the old `ErrUnknownEvent` path (which
counted normal upstream yield/admin traffic against the source's
decode-error counter). The four-way `rebalance_method` discriminator
also gained decode scaffolding: `defindex.DecodeRebalanceMethod` reads
the one documented discriminator field and `RebalanceMethod.Known()`
classifies the four documented methods. **No new `consumer.Event` type,
hypertable, or projector wiring was added** — that's deliberate: the
per-method payloads are still unmodelled (see below).

**Still out of scope (Phase C+) — blocked on real on-chain samples:**

- Body decode for `("BlendStrategy","harvest")` strategy-layer yield
  events — the harvest body has never been observed on-chain and is
  NOT modelled (inventing field layouts is forbidden). Recognised +
  clean-dropped today.
- Per-method payload decode for `("DeFindexVault","rebalance")` — the
  `rebalance_method` discriminator is now read
  (`DecodeRebalanceMethod`), but the four per-method bodies (`unwind`
  / `invest` / `SwapExactIn` / `SwapExactOut`) are unmodelled: the r1
  lake has **zero** rebalance emits as of 2026-07-06, so the exact
  wire spelling of the method Symbols and their body layouts are
  unconfirmed. Blocked until a real sample lands.
- Body decode for `("DeFindexVault", rescue|paused|unpaused|nreceiver|
  nmanager|nemanager|rbmanager|dfees)` admin events — bodies not
  documented / not observed. Recognised + clean-dropped today.
- Body decode for `("DeFindexFactory","create"|"n_fee")` vault-spawn
  events (topic now classified per EVERY-event policy — F-0018 closed
  2026-05-28 — `Decode` returns `(nil, nil)` on a factory match
  rather than `ErrUnknownEvent`). The actually-useful signal (new
  wrapper address) needs `events.Event.OpArgs` from the
  InvokeContract op since the event body itself doesn't carry the
  new address (Surprising-gotcha #2). Same plumbing pattern Band's
  `relay()` and Redstone's `write_prices()` use.
- A typed `defindex_flows` hypertable so events become
  audit-queryable post-decode (currently the counter is the only
  after-the-fact record — which is why historical recovery
  requires a re-backfill rather than a SQL query).

`BackfillSafe: true` flipped in
`internal/sources/external/registry.go`.
`stellarindex-ops backfill --source=defindex` is now unblocked.
Per CLAUDE.md's "Soroban DeFi contracts upgrade in place" rule,
any future `update_contract` on the strategy contracts must
trigger a new audit cycle (re-check the new hash's data section).

The *factory* `b0fe36b2...0e` (first-deploy `L57,056,338`) needs
no decoder — dispatch is by the strategy topic, not factory
events. Code-upload predates the wasm-history walk window, but
walk-confirmed single-hash zero-upgrades over its observed life.
