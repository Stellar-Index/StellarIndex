# SDEX (classic Stellar DEX) — trade & source verification

> **What this page is:** SDEX is not a third-party contract, so there is
> no external team to confirm a contract set with. This page documents
> how Stellar Index extracts classic-DEX trades and why the attribution
> is trustworthy without a contract-identity gate.
>
> - **Enumeration method:** N/A — SDEX has no contracts. Trades are
>   extracted from the XDR operation results of a fixed set of classic
>   op types (see below), so the "gate" is the op-type set, not a
>   contract allowlist.
> - **Last verified:** 2026-07-06 (source: `internal/sources/sdex`).
> - **Gate status:** N/A (op-result decode; no contract identity to gate).

## What SDEX is

SDEX is the classic (pre-Soroban) Stellar decentralised exchange — the
native on-ledger order book plus the classic AMM liquidity pools. Unlike
every Soroban source, **SDEX emits no contract events**: trades are
implicit in the *results* of classic offer / path-payment operations.
Per ADR-0001 we do not run or proxy Horizon, so we parse the operation
result XDR directly from `LedgerCloseMeta`.

## Operations decoded

The dispatcher routes five classic op types to this decoder via its
`OpDecoder` seam. Each can produce zero or more trades (one per matched
resting offer) per op:

| Operation | Trade source in the result |
|---|---|
| `ManageSellOffer` | `OffersClaimed` |
| `ManageBuyOffer` | `OffersClaimed` |
| `CreatePassiveSellOffer` | `OffersClaimed` (shared `ManageSellOfferResult` arm) |
| `PathPaymentStrictReceive` | `OffersClaimed` |
| `PathPaymentStrictSend` | `OffersClaimed` |

The decoder yields one `canonical.Trade` per `ClaimAtom`, mirroring
order-book + liquidity-pool semantics: a single op can sweep multiple
resting offers and emit multiple trades.

### ClaimAtom variants

`xdr.ClaimAtom` is a three-arm union; the decoder handles two and
surfaces the third as a hard error:

| Variant | Status |
|---|---|
| `ClaimAtomTypeOrderBook` | decoded — standard order-book match |
| `ClaimAtomTypeLiquidityPool` | decoded — AMM-side fill against a classic pool (post-P18) |
| `ClaimAtomTypeV0` | `ErrUnknownClaimAtomType` — legacy pre-P18 shape; should not appear on a healthy modern stream. A sustained rate alerts via `stellarindex_source_decode_errors_total{source="sdex"}` |

"Op succeeded with zero trades" (e.g. a `ManageSellOffer` that posts a
resting order without matching) is **not** a decode error — it simply
emits no trades.

## Post-P23 unified events are orthogonal

Post-P23 (Whisk, mainnet 2025-09-03) every classic asset movement also
emits a unified transfer/mint/burn event with a 4th `sep0011_asset`
topic. **Those are routed to a different decoder** (the SEP-41 transfer /
supply observers) — they carry transfer semantics, not
match-against-offer semantics, and lack the cross-leg denomination a DEX
match has. SDEX still owns the trade-extraction path.

## Aggregator treatment — counted

Class `Exchange` / `IncludeInVWAP=true` (`external.Registry`). SDEX
produces real executed trades and is a **primary VWAP contributor**.
Amounts arrive at native Stellar precision (XLM at 7 decimals, classic
assets at issuer-declared decimals); the decoder stamps
`Trade.BaseAmount` / `QuoteAmount` at those precisions and the aggregator
contributes them at full precision (no scale rewrite).

## Backfill safety

`BackfillSafe = true` **unconditionally** — the op-result decoder has no
on-chain Soroban WASM dependency to audit, so it works the same over
historical ledgers as live. SDEX trades go back to genesis. The only
decode-error budget to watch is `ErrUnknownClaimAtomType`, which should
stay at zero on modern ledgers (a non-zero rate signals a protocol bump
we haven't audited or replay over very old pre-P18 ledgers).

## References

- Source package: `internal/sources/sdex/README.md`
- ADR-0001 (Horizon is not in our architecture — why we decode XDR directly)
- Soroban-DEX equivalent: [soroswap.md](soroswap.md)
