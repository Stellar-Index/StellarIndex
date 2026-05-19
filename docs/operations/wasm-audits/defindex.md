---
title: DeFindex WASM-history audit
last_verified: 2026-05-19
status: BLOCKED — audit FAIL, decoder ↔ deployed-WASM mismatch (BackfillSafe stays false)
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

## Pending work to flip BackfillSafe → true

`BackfillSafe` **stays `false`**; `ratesengine-ops backfill
--source=defindex` remains gated. To unblock (Task #28):

1. Disassemble the **deployed** vault WASM `11329c24...988`
   (`wasm2wat` / capture a real on-chain defindex deposit/withdraw
   from LCM) to determine the topic strings + body field names it
   *actually* emits.
2. Rewrite `internal/sources/defindex/{events,decode}.go` to
   match the deployed contract (decode-by-name per
   `docs/architecture/contract-schema-evolution.md`); verify live
   decoding produces rows in `aggregator_exposures`.
3. Re-run this audit against `11329c24...988`; only then flip
   `BackfillSafe: true`.
4. Separately: confirm whether the *factory* `b0fe36b2...0e`
   (first-deploy `L57,056,338`, code-upload ledger < the walk
   window) needs its own decoder review — Phase A decodes vault
   events only, so it is not Phase-A-critical, but note it.
