---
adr: 0047
title: Pre-P23 classic-movement reconstruction from the lake
status: Accepted
date: 2026-07-10
supersedes: []
superseded_by: null
---

# ADR-0047: Pre-P23 classic-movement reconstruction from the lake

## Context

Since Protocol 23 every classic asset movement emits a CAP-67 event we already capture; before it, payments, path payments, merges, claimable balances, clawbacks and LP deposits and withdrawals are reconstructed nowhere, and Horizon is banned (ADR-0001).
An explorer must cover that history; 11 of the 15 value-moving operation types reconstruct from `stellar.operations` and `stellar.operation_results` alone, and LP operations also need `ledger_entry_changes`.
Evidence: `docs/architecture/pre-p23-classic-movements-research.md`.

## Decision

Reconstruct pre-P23 classic movements from the ClickHouse lake, never from a MinIO walk or Horizon-derived code.

- **D1 Storage.** The archive is `stellar.account_movements` in ClickHouse, feed-shaped with two rows per movement (one per participant, with a direction discriminator). Postgres `classic_movements` was dropped. Movement kinds are `payment`, `create_account`, `path_payment`, `account_merge`, `clawback`, `claimable_balance_create`, `claimable_balance_claim`, `claimable_balance_clawback`, `liquidity_pool_deposit`, `liquidity_pool_withdraw`. Provenance is `classic_derived`; `cap67_event` is reserved.
- **D2 Writer.** `internal/sources/classicmovements` decodes operations and entry changes. It is historical-only, run by `stellarindex-ops classic-movements-backfill`, which hard-clamps its range below the P23 start ledger. It is not in the live dispatcher and writes nothing to `sep41_transfers`.
- **D3 Phasing**, each phase shippable alone. Phase 0 backfills `ledger_entry_changes` over `[38115806, 61999000]` (LP correctness needs nothing before P18). Phase 1 is Payment and CreateAccount, Phase 2 the payment framing of path payments (trade legs stay SDEX's), Phase 3 claimable balances and Clawback, Phase 4 AccountMerge, LP deposit and withdraw, and the CAP-0038 revocation edge (the last two gated on Phase 0). Fees are not movement rows; they are served from `stellar.transactions.fee_charged`. An operation type is covered all-or-nothing within its phase.
- **D4 Verification.** A static switch-coverage test per phase over the closed operation enum, and, from Phase 0 on, a reconcile of derived movement sums against `ledger_entry_changes` balance deltas (required for Phase 4).
- **D5 Precedent.** Build only on `go-stellar-sdk/ingest`; do not port Horizon's effects processors or inherit from `cdp-pipeline-workflow`.

## Invariant

- The backfill never writes at or above the P23 start ledger: `internal/ops/chops/classic_movements_backfill.go` (`classicMovementsP23StartLedger`).
- The supported operation set matches the in-scope set exactly and out-of-scope types are rejected loudly: `internal/sources/classicmovements/recognition_test.go`.
- A zero amount on an operation whose amount must be positive is a loud decode error, never a zero-value movement: `internal/sources/classicmovements/amount_guards_test.go`.
- Account activity is one feed merged at read time from `stellar.account_movements` and `sep41_transfers`; neither table knows the other at write time (ADR-0048 D5): `internal/api/v1/explorer/movements.go`.

## Consequences

- Genesis-to-tip account activity becomes answerable without Horizon.
- The archive is on the order of 10 to 11 billion rows; serve-all versus a recent window is deferred until Phase 1 measures row bytes.
- Phase 0 is a multi-day heavy job on the one-at-a-time schedule.
- The P23 boundary is a permanent, visible provenance line in the data.

## Evidence

`internal/sources/classicmovements/`, `deploy/clickhouse/tier1_schema.sql` (`stellar.account_movements`), `cmd/stellarindex-ops/help.go`.
