---
title: "C2-16 — content-level oracle projection reconcile (design proposal)"
status: proposed
finding: C2-16 (RFC-4)
date: 2026-07-17
---

# C2-16 — content-level oracle projection reconcile on a vintage-stable identity

**Finding (RFC-4):** the four event-re-derive oracle sources (`reflector-dex/cex/fx`, `redstone`) opt out of the CS-084 strict per-ledger reconcile via the `aggregateReconcile` field (`internal/ops/chops/reconciliation_catalogue.go:245`), falling back to **window-total netting** in `projectionDelta` (`internal/ops/chops/compute_completeness.go:846`). This re-opens the "discrepancies net to 0" bug class.

**This design supersedes the naive fix** (which would re-introduce false positives on every legacy oracle window — hence the original "flagged, not rushed" deferral). It is a *proposal*; ship it in **shadow mode** and validate before it gates any verdict.

## 1. The premise is worse than the finding states

`aggregateReconcile` is a `reconSource` field, not a function. When set, `projectionDelta` compares **Σ counts** (rows-per-ledger) instead of strict per-ledger counts. Two facts sharpen severity:

1. **It is a COUNT reconcile, not a VALUE reconcile.** Both sides are `map[uint32]int` (rows per ledger). A **wrong price on an existing row is invisible** regardless of netting — the reconcile never looks at price/asset/quote. So the finding's offsetting errors hide *two* ways: netting cancels count deltas, AND content is never checked.
2. **Netting hides real count drops** (`{drop @ L1, phantom @ L2}` nets to 0 — the exact CS-084 pattern), which `TestProjectionDelta_AggregateModeToleratesShift` pins as intended vintage tolerance but is indistinguishable from a real bug.

## 2. Why the naive fix is wrong — the vintage-keying constraint

`oracle_updates` PK is `(source, ledger, tx_hash, op_index, ts)`, where `ts` is the **oracle-published timestamp, not ledger close** (`migrations/0003`). The single divergent column across write-vintages is **`ledger`**:
- **Live path** stamps `Ledger: e.Ledger` (the **event ledger**) — `reflector/decode.go:110`, `redstone/decode.go:105`.
- **Legacy backfill** (subcommands now deleted) keyed `ledger` by the **oracle-timestamp's ledger** (per the CS-084 CHANGELOG note).

A strict per-ledger compare over a legacy window therefore produces equal-and-opposite deltas (`drop @ event-ledger` + `phantom @ timestamp-ledger`) that net to zero — which is exactly why netting was chosen, and why a strict switch would false-flag all pre-cutover history. Retention was removed (`migrations/0040`), so legacy rows persist forever.

## 3. Proposed fix — content reconcile on a vintage-stable identity

**Identity (exclude the divergent `ledger`):** `(source, asset, quote, ts)` → reconciled value = the **normalized price**. This mirrors the finding's `(oracle_id, round/phase, asset-pair, price-value)` shape and the pattern CS-084 already applied successfully to the ContractCall/SDEX censuses (`reDeriveContractCallCensus`, `reDeriveSDEXCensusViaDecoder`) — reconcile on a distinct stable identity, **absolute, never netted**.

**Algorithm (per source, over a `ts`-bounded chunk):**
- **Expected:** stream `contract_events` for the source's contracts (reuse `StreamContractEvents`), decode with the live `src.dec`, and for each update with `ts ∈ [T0,T1)` build `expected[(source,asset,quote,ts)] = multiset<normPrice>` (`normPrice = price·10^(D−decimals)`, exact `big.Int`).
- **Actual:** new store method `OracleContentByIdentity` → `SELECT asset,quote,ts,price,decimals FROM oracle_updates WHERE source=? AND ts>=? AND ts<?` — filtered by **`ts`, never `ledger`** ⇒ vintage-independent by construction.
- **Reconcile:** `Σ over keys |expected[key] − actual[key]|_multiset`. Flags missing, phantom, AND **wrong-price** (both the correct and wrong price surface); offsetting `+X/−X` land on different keys, each unbalanced → **no cross-key netting** (identical to the strict path's `Σ|per-ledger Δ|`).

**Wiring:** add an oracle-content branch in `reconcileProjectionAggregate` (`compute_completeness.go:475`) parallel to the census branches; delete `projectionDelta`'s netting branch once no source uses it; update the guard tests to assert the four sources opt into **content** reconcile (not netting).

## 4. Migration / back-compat — no data re-keying (deliberate)

Serving reads key on `(source,asset,quote,ts)` and use `ledger` only as an `ORDER BY` tiebreaker (`timescale/oracle.go`), so the legacy ledger keying does **not** affect price serving — only the reconcile, which this design stops keying on `ledger`. So: **fix the reconcile, leave the data.** The ts-keyed reconcile is correct for both vintages uniformly; no cutover, shim, or backfill.

**Do NOT re-key legacy rows:** `ledger` is in the PK, so the INV-3 `derive_generation` upsert (0109) would *not* match a legacy timestamp-ledger row from an event-ledger re-derive — `ON CONFLICT` misses and INSERTs a duplicate. Re-keying would need DELETE+re-derive (the treadmill we're escaping). Unnecessary once the reconcile ignores `ledger`.

## 5. Risks (validate in shadow mode before gating)

- **R1 — `ts`-fallback subset:** `SafeUnixMillis` falls back to ledger-close when the on-chain ts is missing; such rows' `ts` could differ across vintages → false positive. Size it via a shadow probe (expected tiny); fall back to a per-ts-day count check for that subset if needed.
- **R2 — `tx_hash`/`op_index` stability unknown from code.** If legacy preserved real tx_hash/op_index (only `ledger` diverged), `(source,tx_hash,op_index)` is a *tighter, simpler* identity with no ts-fuzz and no read margin. The shadow probe must test this; design the identity as one swappable function.
- **R3 — ledger read-margin `M`** (only if keying on `ts`): over-read `[L(T0)−M, L(T1)+M]` because ts ≠ event-close; size `M` empirically. Vanishes under R2.
- **R4 — decimals drift:** normalized-price compare correctly flags historical rows written at a wrong `decimals` (CS-040 class); classify decimals-only mismatches distinctly for triage.
- **R5 — memory:** chunk by one ts-day (mirror the 25k-ledger SDEX windowing).
- **R6 — legacy writer deleted:** cutover ledger, whether legacy rows still exist on R1, and tx_hash preservation are all inferred; the shadow probe recovers these facts.

## 6. Rollout — shadow mode first

Ship report-only: compute + log `Σ|per-key diff|` and mismatched identities, but do **not** flip the ADR-0033 `complete` verdict (the two-axis verdict separates `lake_complete` from the projection axis, so a shadow signal is easy to carry). Run across full oracle history on R1 and confirm: (1) vintage tolerance holds (Δ≈0 on legacy windows where naive strict would explode — run both side-by-side); (2) R1 fallback-subset sizing; (3) R2 tx_hash/op_index stability test; (4) R3 skew distribution; (5) inject a corrupted price + a phantom row and confirm it flags exactly those. Promote to gate the served-tier verdict only after (1)–(2) are clean.

**Bottom line:** stop reconciling oracles by `ledger`-keyed *counts* (netted); reconcile *content* (normalized price) per `(source,asset,quote,ts)`, absolute per identity — a strict upgrade that catches the offsetting-count AND wrong-value classes while being vintage-independent by construction. No data migration. Validate in shadow before gating.
