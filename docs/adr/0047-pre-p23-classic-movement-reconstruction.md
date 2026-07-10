---
adr: 0047
title: Pre-P23 classic-movement reconstruction from the lake
status: Accepted
date: 2026-07-10
supersedes: []
superseded_by: null
---

# ADR-0047: Pre-P23 classic-movement reconstruction from the lake

Research basis: [docs/architecture/pre-p23-classic-movements-research.md](../architecture/pre-p23-classic-movements-research.md)
(2026-07-10; all factual claims below — lake sufficiency, XDR field
availability, volume estimates — are evidenced there, not repeated here).

## Context

Post-P23 (Whisk, 2025-09-03) every classic asset movement emits a
unified CAP-67 event our decoder captures into `sep41_transfers`.
Before P23 there are no such events, so classic movements — payments,
path payments, account merges, claimable balances, clawbacks, LP
deposits/withdrawals — are not reconstructed anywhere in our stack.
Horizon derives "effects" for this era; Horizon is banned from our
architecture (ADR-0001). Stellar Index is an explorer: the owner has
ruled the historical gap must close ("definitely need the historical
coverage, this is an explorer after all", 2026-07-10).

The research established the gap is narrower than feared: of 27
classic operation types, 15 move value; 11 of those reconstruct from
`stellar.operations` + `stellar.operation_results` alone — both fully
populated in the certified lake to genesis with exact 1:1 parity
against `stellar.ledgers.op_count`. Only `LiquidityPoolDeposit`/
`Withdraw` (plus the CAP-0038 revocation edge) require
`ledger_entry_changes`, whose full-fidelity rows currently start at
~ledger 61,996,000. SDEX trade legs (ClaimAtoms, all three variants)
are already decoded and are NOT part of this gap.

## Decision

Reconstruct pre-P23 classic movements **from the ClickHouse lake**
(`stellar.operations` / `operation_results` / `ledger_entry_changes`),
never a MinIO walk and never Horizon-derived code, phased as below.

### D1 — Storage: a new `classic_movements` table, unified at read time

A new hypertable purpose-built for two-party asset movements, NOT a
write into `sep41_transfers` (whose schema is Soroban-shaped and whose
writer is exclusively the projector — ADR-0031). Discriminators:

- `movement_kind`: `payment` / `create_account` / `path_payment` /
  `account_merge` / `clawback` / `claimable_balance_create` /
  `claimable_balance_claim` / `claimable_balance_clawback` /
  `liquidity_pool_deposit` / `liquidity_pool_withdraw`.
- `provenance`: `classic_derived` (everything this ADR writes);
  `cap67_event` is RESERVED for a possible future normalization of
  post-P23 rows into the same table — not used at first cut.

Natural key `(ledger, tx_hash, op_index [, leg_index])`; amounts
`NUMERIC` in Postgres, strings in JSON, per convention (classic
amounts fit int64 but are not special-cased). The account-activity
read surface (explorer account page; a future
`/v1/accounts/{g}/movements`) merges `classic_movements` with
`sep41_transfers`' post-P23 `transfer` rows into ONE chronological
feed — neither table knows about the other at write time. This is the
`trades`-style multi-writer/shared-read pattern, not the projector
pattern.

### D2 — Writer: a new non-projected decoder path

An `OpDecoder` + `LedgerEntryChangeDecoder` hybrid mirroring SDEX's
`OpContext` pattern, the sole writer into `classic_movements`
(one-writer-per-domain by construction). Backfill is `ch-rebuild`-style
derivation from the lake. Live tail: post-P23 movements arrive as
CAP-67 events, so the new decoder is HISTORICAL-ONLY — it runs in
backfill derivations, not the live dispatcher (nothing to decode live;
the P23 boundary is a hard upper bound on its range).

### D3 — Phasing (each phase independently shippable)

Per the research §6, adopted as-is:

- **Phase 0 (prerequisite, operator-scheduled heavy job):**
  `ch-backfill` to close the `ledger_entry_changes` fidelity gap —
  **window `[38115806, 61999000]`** (P18 → current fidelity floor),
  NOT genesis: AMMs did not exist before P18, so LP correctness needs
  nothing earlier, and this bounds the row-count risk. Extending to
  genesis is a separate operator decision after sampling the row-count
  cost (research open question 5), tracked but not blocking. Phase 0
  also partially addresses the pre-existing "ledger_entries_current
  ~62M floor" backlog item.
- **Phase 1:** `Payment` + `CreateAccount` (~4B rows; op body alone;
  no Phase-0 dependency; highest product value).
- **Phase 2:** `PathPaymentStrictReceive/Send` payment framing
  (~3.5B rows; result XDR `Last.Amount`; trade legs stay SDEX's).
- **Phase 3:** ClaimableBalance create/claim/clawback + `Clawback`
  (~2.9B rows; needs a self-referential `BalanceId` index, not entry
  changes).
- **Phase 4:** `AccountMerge` + LP deposit/withdraw + the CAP-0038
  trustline-revocation auto-liquidation edge case (the latter two
  hard-gated on Phase 0). The CAP-0038 edge is IN scope but ships
  here, not earlier (research open question 2: deferred-to-Phase-4,
  not dropped).
- **Fees (research Phase 5): REJECTED as movement rows** — served
  from `stellar.transactions.fee_charged` directly (open question 3
  resolved: final). No 8.8B-row materialization.

Within any phase, coverage of an op type is all-or-nothing (the
closed 27-value enum admits no partial-switch excuse — the every-event
principle applied to classic ops).

### D4 — Verification (ADR-0033 applied to derived data)

1. Substrate continuity: inherited free (ledgers hash-chained;
   ops/results reconcile exactly against `op_count`).
2. Recognition: a static switch-coverage test per phase (closed enum —
   no operator-seeded gating needed).
3. Projection reconcile: post-Phase-0, a periodic job compares derived
   movement sums per (account, asset, window) against
   `ledger_entry_changes` balance deltas; Phases 1–3 gain this as a
   cross-check, Phase 4 requires it.

### D5 — Precedent hygiene

Build ONLY on `go-stellar-sdk/ingest` (already a dependency).
Explicitly do NOT port Horizon's `ingest/processors` effects logic
(the stellar-etl approach — a soft ADR-0001 violation), and do NOT
inherit from `cdp-pipeline-workflow` (known-buggy, VERSIONS.md).
`stellar-expert/tx-meta-effects-parser` (MIT) may be consulted as an
independent reference implementation.

## Consequences

- The explorer gains a genesis-to-tip account-activity story; "what
  did this account send/receive in 2019" becomes answerable without
  Horizon.
- Postgres grows by roughly 10–11B rows across Phases 1–4 (order of
  magnitude; the research's stratified sampling caveats apply) — the
  served-tier retention question for this table (serve-all vs
  recent-window + lake-backed deep history) is deliberately deferred
  to Phase 1 implementation, informed by actual row sizes; ADR-0034's
  lake/served split already sanctions either answer.
- Phase 0 is a multi-day r1 heavy job competing with the existing
  replay queue; it enters the same one-at-a-time schedule.
- A permanent provenance boundary at P23 is encoded honestly in the
  data model rather than hidden behind a shape-shifting feed.
