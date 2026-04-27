---
title: Band WASM-history audit
last_verified: 2026-04-27
status: pending — scaffolding only; per-hash review in follow-up PR
source: band
backfill_safe: false
---

# Band WASM audit

Audit log for the `band` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

## Status

**Scaffolded 2026-04-27.** Captures the contracts, decoder
expectations, and Band-specific failure modes. The actual
`wasm-history` walk + per-hash review lands in a follow-up PR.

`BackfillSafe` stays `false` for `band` until that follow-up
finishes.

## Contracts under audit

| role | mainnet contract (operator config) |
| --- | --- |
| StandardReference | `cfg.Oracle.Band.StandardReferenceContract` |

Concrete address lives in the operator's `ratesengine.toml` and
Phase-1 discovery doc; not hard-coded in the decoder.

## Decoder expectations — Band is structurally unique

Captured from `internal/sources/band/{events,decode}.go` at HEAD as
of 2026-04-27. Re-verified 2026-04-24 against pinned source.

Per CLAUDE.md:

> **Band's Soroban contract emits zero events.** A conventional
> topic-match Decoder never fires on Band. We observe the
> `relay()` / `force_relay()` InvokeContract call instead via
> the dispatcher's `ContractCallDecoder` interface (PR 168). Any
> future Soroban source that updates storage without publishing
> events plugs into the same hook — match by (contract_id,
> function_name), decode from op args.

So Band's audit is **fundamentally different** from every other
on-chain source's:

- **There are no events to decode.** wasm-history's
  `LedgerEntryChange` walk still works for tracking the contract's
  WASM evolution, but there's no event-shape audit because there
  are no events.
- **The decoder operates on op args**, not event bodies. The
  audit reviews function signatures + arg shapes against each WASM
  hash, not topic + body.
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

`force_relay` drops the `from` arg — admin-only path, not gated by
the relayer check. Both produce the same logical output: one
`(Symbol, rate)` pair per entry written to Band's `ref_data`
storage.

### Decoder reads args by position

The decoder reads InvokeContract args **positionally** — there's no
named-arg shape to extract by name. A reorder of args silently
produces wrong attribution.

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
- Single-symbol rates from `relay` calls are **USD-denominated** —
  `get_ref_data(XYZ)` returns XYZ priced in USD. Pair rates (
  `get_reference_data`) are computed on-read at E18 — we **don't
  emit those** because they're a function of storage state, not
  wire input.
- Timestamps: `resolve_time` is UNIX seconds (verified against
  `env.ledger().timestamp()` comparison in `ref_data.rs:56`).

### Symbol allow-lists

The decoder skips symbols not on its fiat-or-crypto allow-list with
`ErrUnknownSymbol` (per-entry). A new symbol relayed by Band that
we haven't allow-listed is silently dropped — list lives in the
discovery doc + the package's symbol_resolver.

## Failure modes specific to Band

1. **`relay` / `force_relay` function rename** — the decoder's
   `(contract_id, function_name)` match key is the entry-point.
   Either function renamed → no calls dispatch to us → silent drop
   of every Band update.
2. **Function signature reorder** — e.g. `relay(symbol_rates, from,
   resolve_time, request_id)`. Positional decoder reads index 0 as
   the symbol_rates Vec → fails on type mismatch (Address vs Vec
   tuple). Per-call error, every call dropped under affected WASM.
3. **New optional arg added** — Soroban contracts can extend
   signatures by adding args. If a future relay accepts an extra
   `signer: Address` at index 4, our decoder ignores trailing args
   so we still extract the first 4 correctly. **Probably safe**,
   pending verification.
4. **Args swapped without signature change** (e.g. `(symbol, u64)`
   → `(u64, symbol)` inside the inner Vec) — silently produces
   wrong attribution. **No automated detection** — every new WASM
   hash needs source review.
5. **Rate scale change E9 → E18** — silently mis-reports every
   price. Caught only by cross-source divergence vs Reflector /
   Redstone.
6. **`u64` → `u128` rate type** — strict extraction errors per
   entry; every entry dropped.
7. **Rate sign change `u64` → `i64`** — possible if Band ever
   needed negative rates (e.g. pricing-model deltas). Strict u64
   extraction errors per entry.
8. **`from` Address required for force_relay** (adding gating) —
   would break our positional read of `force_relay` (index 0 would
   be Address instead of Vec). Per-call error.
9. **`get_ref_data` / `get_reference_data` semantics change** —
   doesn't affect our decoder (we don't emit pair rates from
   relay), but downstream consumers reading the pair-rate API
   would see different values. Out of scope for this audit.

## WASM timeline

(*to be filled in by the follow-up PR after `wasm-history` runs*)

## Per-hash review findings

Per-hash review for Band MUST verify the function signatures of
both `relay` and `force_relay` are unchanged, plus the inner Vec
tuple order `(Symbol, u64)`. Event-shape diffs are N/A here — Band
emits no events.

| hash (first 16) | active range | reviewer | finding |
| --- | --- | --- | --- |
| (pending) | (pending) | (pending) | (pending) |

## Decision

**`BackfillSafe: false`** — pending the per-hash review.

Band is structurally simpler than the other Soroban sources (no
event correlation, single-event-shape) but the positional op-args
decoder makes any signature-level drift a silent-failure risk.

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/band/{events,decode}.go`
- Discovery doc: `docs/discovery/oracles/band.md`
- Schema-evolution stance: `docs/architecture/contract-schema-evolution.md`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["band"].BackfillSafe`
