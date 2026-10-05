---
title: Band WASM-history audit
last_verified: 2026-05-03
status: ratified — v2 walk confirms single stable WASM
source: band
backfill_safe: true
---

# Band WASM audit

Audit log for the `band` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

> **2026-05-03 update — v2 walk confirms single stable WASM.**
> The 2026-04-30 wide-net r1 walk re-observed the
> StandardReference contract on `6cdb9a3cdeec01a1…` and produced
> **zero transitions** across the [50,457,424, 62,249,727]
> range. Combined with the contract's first-deploy ledger
> (L50,842,736, 2024-03-19), the WASM has been stable for the
> entire mainnet life. Bytes SHA-256-verified at
> `evidence/r1-walk-2026-05-01/wasm-bytes/6cdb9a3cdeec01a1…wasm`
> on r1.
>
> **2026-05-01 update.** Hashes cross-checked against the 2026-04-30 r1 walk;
> see [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md).

## Status

**Ratified 2026-04-29.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same PR as this
audit. The StandardReference contract shows **one stable WASM hash**
across the post-deploy window; no `update_contract` events. The live decoder's positional
op-args reader matches function signatures and Vec tuple order.

## Contracts under audit

| role | mainnet contract |
| --- | --- |
| StandardReference | `CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M` |

Configured via `cfg.Oracle.Band.StandardReferenceContract`
in `stellarindex.toml`.

## Decoder expectations — Band is structurally unique

Captured from `internal/sources/band/{events,decode}.go` at HEAD as
of 2026-04-29. Re-verified 2026-04-24 against pinned source.

Per AGENTS.md:

> **Band's Soroban contract emits zero events.** A conventional
> topic-match Decoder never fires on Band. We observe the
> `relay()` / `force_relay()` InvokeContract call instead via
> the dispatcher's `ContractCallDecoder` interface. Any
> future Soroban source that updates storage without publishing
> events plugs into the same hook — match by (contract_id,
> function_name), decode from op args.

So Band's audit differs from every other on-chain source:

- **No events to decode.** wasm-history's `LedgerEntryChange` walk
  still tracks WASM evolution; no event-shape audit.
- **The decoder operates on op args.** The audit reviews function
  signatures + arg shapes per WASM hash, not topic + body.
- **Failure modes are op-args-shaped.**

### Watched function signatures

Verified against `band-soroban/src/contract.rs:23-35`:

    relay(
        from:         Address,
        symbol_rates: Vec<(Symbol, u64)>,
        resolve_time: u64,
        request_id:   u64,
    )

    force_relay(
        symbol_rates: Vec<(Symbol, u64)>,
        resolve_time: u64,
        request_id:   u64,
    )

`force_relay` drops `from` — admin-only path, not relayer-gated. Both write one
`(Symbol, rate)` pair per entry to Band's `ref_data` storage.

### Decoder reads args by position

InvokeContract args are read **positionally** (no names); a reorder
silently produces wrong attribution.

| function | arg index | arg shape | what we extract |
| --- | --- | --- | --- |
| `relay` | 0 | Address | (currently ignored — relayer identity) |
| `relay` | 1 | Vec<(Symbol, u64)> | (symbol, rate) pairs |
| `relay` | 2 | u64 | resolve_time (UNIX seconds) |
| `relay` | 3 | u64 | request_id (currently ignored) |
| `force_relay` | 0 | Vec<(Symbol, u64)> | (symbol, rate) pairs |
| `force_relay` | 1 | u64 | resolve_time |
| `force_relay` | 2 | u64 | request_id |

### Rate scale + denomination

- Rates are `u64` at **E9 = 10^9** scale (per
  `band-soroban/src/constant.rs`). Every relayed rate uses this
  scale.
- Single-symbol rates from `relay` are **USD-denominated**
  (`get_ref_data(XYZ)` = XYZ in USD). Pair rates (
  `get_reference_data`) are computed on-read at E18; we **don't
  emit those** (storage state, not wire input).
- Timestamps: `resolve_time` is UNIX seconds (verified against
  `env.ledger().timestamp()` comparison in `ref_data.rs:56`).

### Symbol allow-lists

Symbols outside the fiat / crypto / RWA allow-lists are no longer skipped
(oracle capture-totality, PR-2, `docs/design/oracle-capture-totality-design.md`):
an unmapped symbol is recorded verbatim as a `raw:<symbol>` row (`canonical.AssetOracleRaw`)
at its own `symbol_rates[]` slot and counted by `stellarindex_source_unknown_symbols_total{source="band"}`.
Only `USD` (contract-rejected) and `rate == 0` are still skipped — list
in the discovery doc + the package's symbol_resolver.

## Failure modes specific to Band

1. **`relay` / `force_relay` function rename** — the
   `(contract_id, function_name)` match key; a rename silently drops
   every Band update.
2. **Function signature reorder** — e.g. `relay(symbol_rates, from,
   resolve_time, request_id)`: index 0 read as the symbol_rates Vec
   fails on type mismatch; every call dropped under that WASM.
3. **New optional arg added** — e.g. `signer: Address` at index 4:
   trailing args are ignored, first 4 still extract
   (`TestDecodeRelay_TrailingArgIgnored`).
4. **Args swapped without signature change** (e.g. `(symbol, u64)`
   → `(u64, symbol)` in the inner Vec) — silently wrong
   attribution. **No automated detection** — every new WASM
   hash needs source review.
5. **Rate scale change E9 → E18** — silently mis-reports every
   price; caught only by cross-source divergence vs Reflector /
   Redstone.
6. **`u64` → `u128` rate type** — strict extraction errors per entry.
7. **Rate sign change `u64` → `i64`** — strict u64
   extraction errors per entry.
8. **`from` Address required for force_relay** (adding gating) —
   breaks the positional read (index 0 Address instead of Vec); per-call error.
9. **`get_ref_data` / `get_reference_data` semantics change** —
   no decoder impact (no pair rates emitted), but pair-rate API
   consumers would see different values. Out of scope.

## WASM timeline

Output from `stellarindex-ops wasm-history` over the post-Soroban
window — full archive on r1, walked 2026-04-29:

```json
[
  {
    "contract": "CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M",
    "ranges": [
      { "wasm_hash": "6cdb9a3cdeec01a1...",
        "from_ledger": 50842736, "to_ledger": 51931461 }
    ]
  }
]
```

The range appears only in the first worker's chunk
(`CreateContract` at L50,842,736, 2024-03-19); no later
`update_current_contract_wasm` — **one Band StandardReference WASM**
through walk-end at L59,301,651. Live ingest
from walk-end through r1's tip (L62,342,614 as of
2026-04-29): no further upgrade, 0 `ErrFunctionMismatch`
or type-extraction failures.

Soroban activated at L50,457,424 (2024-02-20); Band's first deploy
at L50,842,736 (2024-03-19) is the mainnet launch.

## Per-hash review findings

| hash (first 16) | role | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| `6cdb9a3cdeec01a1` | StandardReference | L50,842,736 → L59,301,651 (walk-end; still current per live ingest through r1 tip L62,342,614) | maintainer@2026-04-29 | matches current decoder |

### `6cdb9a3cdeec01a1` — StandardReference, single hash, no upgrade

- **Function signatures**: `relay(Address, Vec<(Symbol, u64)>, u64, u64)`
  and `force_relay(Vec<(Symbol, u64)>, u64, u64)` match the
  positional reader in `internal/sources/band/decode.go`. Source of truth:
  `band-soroban@<release>`; deployed hash `6cdb9a3c…` corresponds to it (no
  rebuild post-deploy).
- **Inner Vec tuple order**: `(Symbol, u64)` — verified against
  `band-soroban/src/contract.rs` and in
  `internal/sources/band/decode_test.go` golden fixtures (live mainnet calls).
- **Rate scale**: E9 confirmed against
  `band-soroban/src/constant.rs`; live decoder applies the same
  scale via `bandRateScale = 1e9` constant.
- No `update_current_contract_wasm` post-deploy rules out signature drift.
- Live ingest health: 0 `ErrFunctionMismatch` / 0 type-extraction
  failures observed in production metrics since the
  ContractCallDecoder hook landed (commit `ee0360da4`, "wire
  band + comet + redstone decoders", 2026-04 cutover).

## Decision

**`BackfillSafe: true`** — flipped in
`internal/sources/external/registry.go` in this PR.

Rationale:

- **One stable WASM hash** across the post-deploy window.
- Positional op-args reader matches the deployed function
  signatures (Phase-1 fixtures + production ingest health).
- No events, per-pair contracts or factory-template indirection, so no
  analog to Soroswap's pair-WASM caveat.

A future upgrade needs a per-hash entry + decoder verification; the flag
flips to `false` if the new WASM diverges and the decoder fix isn't shipped.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/band/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/contract-schema-evolution.md`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["band"].BackfillSafe`
- Upstream contract source: pinned in `VERSIONS.md`
- WASM-history walk JSON (full): `r1:/var/log/wasm-history-all.json`
