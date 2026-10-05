---
title: Soroswap Router WASM-history audit
last_verified: 2026-05-19
status: complete — BackfillSafe=true (audited 2026-05-19)
source: soroswap-router
backfill_safe: true
---

# Soroswap Router WASM audit

Audit log for the `soroswap-router` source's `BackfillSafe` flag. Procedure: `README.md`.

## Status

**Complete (2026-05-19) — `BackfillSafe: true`.** See "Audit result" below:
both decoder entry points are present in the single deployed hash, with zero
mid-life upgrades. `BackfillSafe` flipped `true` in
`internal/sources/external/registry.go`.

Sister source to `soroswap`: that decoder watches per-pair
`SoroswapPair("swap")` events; this one watches the router's `InvokeContract`
calls via `dispatcher.ContractCallDecoder` (the router emits no events).

## Contract under audit

| role   | contract                                                       |
|--------|----------------------------------------------------------------|
| Router | `CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH`     |

Cross-checked against `soroswap-core/public/mainnet.contracts.json` on
2026-04-23 (same source as the sister `soroswap.md` audit).

The 2026-04-30 r1 walk inventoried this router under a single WASM hash with
no mid-life upgrades observed in the walk window (see `r1-walk-2026-05-01.md`); the walk evidence is
itemised in the soroswap audit's Phase 2 results. We'll re-cite it here once
the per-hash decoder review for the router call shapes lands.

## Decoder expectations

From `internal/sources/soroswap_router/{events,decode}.go` at HEAD as of
2026-05-14. Any divergence in a deployed WASM hash is an audit finding.

### Function signatures matched

Only swap entry points are routed; admin / read-only methods are skipped.

```text
swap_exact_tokens_for_tokens(
    amount_in:        i128,
    amount_out_min:   i128,
    path:             Vec<Address>,
    to:               Address,
    deadline:         u64,
) -> Vec<i128>

swap_tokens_for_exact_tokens(
    amount_out:       i128,
    amount_in_max:    i128,
    path:             Vec<Address>,
    to:               Address,
    deadline:         u64,
) -> Vec<i128>
```

### Argument-position invariants

- `args[0]` and `args[1]` are i128 (parsed via `scval.AsAmountFromI128`, no
  truncation per ADR-0003).
- `args[2]` is `Vec<Address>` with `len ≥ 2` (router's own precondition).
- `args[3]` is `Address` (recipient).
- `args[4]` is `u64` (Unix-seconds deadline).

### Return value

Not parsed (`Vec<i128>` of realized per-hop amounts). Realized amounts come
from the per-pair `SoroswapPair("swap")` events in the same tx via the sister
`soroswap` decoder, joined on `tx_hash` at aggregator time.

## Audit result (2026-05-19) — PASS

Walk evidence: `evidence/r1-walk-2026-05-01/` + the recovered canonical
`merged.json` from the 2026-05-19 r1 wasm-history walk.

1. **Per-hash WASM identity.** Router `CAG5LRYQ...JDDH` resolved to a
   **single** WASM hash
   `4c3db3ebd2d6a2ab23de1f622eaabb39501539b4611b68622ec4e47f76c4ba07`
   across `[50_746_272 → 62_600_000]` (the walk's upper bound; `from_ledger`
   50,746,272 = first-deploy ledger, so its **entire on-chain life**). **Zero
   mid-life `update_contract` upgrades** — `merged.json` lists exactly one
   range.
2. **Both decoder entry points present.** The WASM bytes
   (`evidence/r1-walk-2026-05-01/wasm-bytes/4c3db3eb...07.wasm`, in-repo)
   export both `swap_exact_tokens_for_tokens` and
   `swap_tokens_for_exact_tokens` (export-name dump). No other swap entry
   points exist.
3. **No event surface to audit.** The router emits no contract events
   (`verify-decoder-wasm-match.py` reports an empty event-topic set for
   `soroswap-router`); the decoder is a `dispatcher.ContractCallDecoder`
   keyed on the two function names, so there is no topic/body schema to drift.

**Conclusion:** single immutable WASM over the contract's whole life, both
decoded functions present, no event schema to drift. `BackfillSafe` flipped
`true` in `internal/sources/external/registry.go` (2026-05-19).
`stellarindex-ops backfill --source=soroswap-router` is now unblocked.
