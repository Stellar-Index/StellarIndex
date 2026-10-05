---
adr: 0033
title: Completeness verification — substrate continuity, recognition, projection reconciliation
status: Accepted
date: 2026-06-02
supersedes: []
superseded_by: null
---

# ADR-0033: Completeness verification — substrate continuity, recognition, projection reconciliation

## Context

Cursor-derived and density-derived coverage can read 100% over a real gap: a partial decode of a busy ledger and a halt inside a guessed sparsity threshold both look like quiet.
Coverage needs a data-derived verdict that is deterministic, re-runnable and localises any gap to a ledger, source or event.

## Decision

Implementation phases: 1 a real per-op `event_index` (fixed the multi-event silent loss), 2 the `ledger_ingest_log` substrate record, 3 the recognition check, 4 projection reconciliation, 5 SDEX reconciliation, 6 the watermark verdict (`completeness_snapshots`, `compute-completeness`).

A source is COMPLETE through ledger W if and only if three claims hold contiguously from the source's genesis to W. W is its completeness watermark, and coverage is `(W - genesis) / (tip - genesis)`. A failing ledger pins W and names what is missing. No threshold, no cursor trust.

- **Claim 1, substrate continuity.** Every ledger is present and the hash chain links (`prev_ledger_hash[N] == ledger_hash[N-1]`). The deployed check is `clickhouse.SubstrateProblem` over the lake. Postgres `ledger_ingest_log` carries the same contiguity and hash columns plus a decoder-independent census of the ledger's Soroban event count and classic trade-effect count, read from the `LedgerCloseMeta` without decoding bodies. The indexer writes that row once `ProcessLedger` has enqueued the ledger's events to the async sink, best-effort, so it is not a post-persist marker. `census-backfill` writes it with no persistence at all, and the projector writes projected rows later. The row proves substrate continuity only. Loss after the row is written is caught by Claims 2b and 3, never by the row's presence.
- **Claim 2a, recognition.** Every distinct `(contract_id, topic)` shape in the lake must be recognised by a decoder. `Dispatcher.Recognize` (`internal/dispatcher/recognize.go`) replays each shape through the same `Matches()` predicates live dispatch uses, so the claimed set cannot drift from what the decoders handle. A new topic from an in-place contract upgrade fails loudly instead of being skipped.
- **Claims 2b and 3, projection reconciliation.** Per ledger, the rows a source should have produced equal the rows in its served table. The oracle is deterministic recomputation: run the real decoder over the raw events and count. Soroban sources re-derive from raw events (`compute-completeness -ch`, the deployed form, from the ClickHouse lake). SDEX re-derives by running the SDEX decoder over the lake's operations and counting distinct trades that pass the served write filter (`reDeriveSDEXCensusViaDecoder`). The `classic_trade_effect_count` column is still written but is not the reconcile oracle, because it counts claims `trades` cannot hold. `verify-reconciliation` re-derives Soroban sources from Postgres `soroban_events`, and SDEX from the lake. The external Hubble anchor (`hubble-check`) is a defence-in-depth cross-check.
- **External CEX/FX sources** have no on-chain substrate. Their signal is freshness and liveness, reported as a distinct class and never folded into the on-chain completeness number.
- **Served verdict.** It is two-axis. `lake_complete` is substrate and recognition, genesis to tip. `complete` is additionally gated by the projection reconcile over the projected window (ADR-0034). A source anchored to pubnet contracts is `not_applicable` on a test net, not counted incomplete. Per-source watermarks live in `completeness_snapshots`.
- `/v1/coverage` sets `flags.stale` when the verdict is old, when the live `ledgerstream` cursor has not been written for 10 minutes, or when the cursor trails the verdict by too many ledgers (`coverageVerdictsStale`). `coverage = 1` means verified to where ingest stopped, not to the network tip.
- `MinGapSizeOverride` and `gap_free_pct` are alerting cadence only, and `density_pct` is descriptive. None is a coverage claim.

## Invariant

- The completeness verdict is the durability claim, and a clean per-ledger reconcile is the proof. A change that weakens its ability to see a hole is a regression. `internal/ops/chops/compute_completeness_test.go`, including `TestReconciliationCatalogue_OracleSourcesOptOut`, guards the catalogue.
- A `ledger_ingest_log` row is written once the ledger's events are enqueued, not persisted, and is never read as proof that rows landed. `TestLedgerIngestLogNotClaimedPostPersist` (`migrations/ledger_ingest_log_contract_test.go`) pins that this section says so.
- `/v1/coverage` flags `stale` under the conditions above. `scripts/ci/lint-completeness-staleness.sh` keeps the staleness constants calibrated to the deployed audit cadence.
- A projected-table reconcile seeds each gated source's contract registry the way live ingest does (ADR-0035), or it reports clean over polluted data (`TestApplyGatedOptions_EveryGatedSourceAdmitsARegistryOnlyContract`).
- A substrate problem at or above a source's genesis fails that source's Claim 1, and an empty or tail-truncated lake cannot green a source (`sourceSubstrateOK`).

## Consequences

- A partial decode of a busy ledger becomes a localised failure, and a quiet period is proven rather than assumed.
- The verdict is auditable and re-runnable by anyone with the lake.
- `internal/hashdb` detects drift against itself (the `stellarindex_hashdb_drift_total` counter, off by default). It does not feed the substrate verdict, and neither do `archivecompleteness` or `verify-archive`.
- Provenance-column anti-joins and a per-source handled-topics list were not built. The recompute-and-count reconcile and `Dispatcher.Recognize` replace them.
- Re-deriving costs a lake scan per verdict cycle.

## Evidence

`internal/dispatcher/census.go`, `internal/dispatcher/recognize.go`, `internal/storage/clickhouse/completeness.go`, `internal/ops/chops/compute_completeness.go`, `internal/ops/chops/verify_reconciliation.go`, `internal/api/v1/coverage_verdicts.go`, `migrations/0051_ledger_ingest_log.up.sql`, `migrations/0052_completeness_snapshots.up.sql`.
