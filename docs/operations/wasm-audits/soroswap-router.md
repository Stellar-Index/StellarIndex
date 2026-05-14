---
title: Soroswap Router WASM-history audit
last_verified: 2026-05-14
status: in_progress — decoder shipped, BackfillSafe still false
source: soroswap-router
backfill_safe: false
---

# Soroswap Router WASM audit

Audit log for the `soroswap-router` source's `BackfillSafe` flag.
See `README.md` for the full procedure.

## Status

**In progress (2026-05-14).** Decoder package
`internal/sources/soroswap_router/` shipped this session. Live
decoding starts from current ledger forward; `BackfillSafe`
remains `false` in `internal/sources/external/registry.go`
pending the per-WASM-hash walk below.

The router is a sister source to `soroswap` — same upstream
protocol, different vantage point. Where the existing `soroswap`
decoder watches per-pair `SoroswapPair("swap")` events, this
decoder watches the router's `InvokeContract` calls directly via
`dispatcher.ContractCallDecoder` (the router itself emits no
events; its work is calling down to per-pair contracts).

## Contract under audit

| role   | contract                                                       |
|--------|----------------------------------------------------------------|
| Router | `CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH`     |

Cross-checked against `soroswap-core/public/mainnet.contracts.json`
on 2026-04-23 (same source as the sister `soroswap.md` audit).

The 2026-04-30 r1 walk inventoried this router contract under a
single WASM hash with no mid-life upgrades observed in the walk
window (see `r1-walk-2026-05-01.md`). The walk evidence is
itemised in the soroswap audit's Phase 2 results; we'll re-cite it
here once the per-hash decoder review for the router-specific call
shapes lands.

## Decoder expectations

Captured from `internal/sources/soroswap_router/{events,decode}.go`
at HEAD as of 2026-05-14. Any divergence from these in a deployed
WASM hash is an audit finding.

### Function signatures matched

The decoder routes only swap entry points; admin / read-only
methods are intentionally skipped.

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

- `args[0]` and `args[1]` are i128 (parsed via
  `scval.AsAmountFromI128` — no truncation per ADR-0003).
- `args[2]` is `Vec<Address>` with `len ≥ 2` (router's own
  precondition).
- `args[3]` is `Address` (recipient).
- `args[4]` is `u64` (Unix-seconds deadline).

### Return value

The decoder does **not** parse the function's return value
(`Vec<i128>` of realized per-hop amounts). Realized amounts are
recovered from the per-pair `SoroswapPair("swap")` events in the
same tx by the sister `soroswap` decoder, joined on `tx_hash` at
aggregator time.

## Pending work to flip BackfillSafe → true

1. Cite per-hash WASM identity from the 2026-04-30 r1 walk
   evidence directory (`evidence/r1-walk-2026-05-01/`).
2. Disassemble the router WASM and confirm both function exports
   match the signatures above.
3. Walk historical `update_contract_op` events on this contract
   ID across the post-Soroban window — confirm zero or audit each
   prior hash.
4. Once Phase 1+2 land, flip `BackfillSafe: true` in
   `internal/sources/external/registry.go`.
