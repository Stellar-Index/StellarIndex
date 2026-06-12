---
title: ADR-0031 + ADR-0032 rollout plan
last_verified: 2026-06-12
status: completed
---

# ADR-0031 + ADR-0032 rollout plan

**Last verified:** 2026-06-12
**Status:** Completed (shipped rc.91–rc.98 per ADR-0032 Phase 5; the
plan below is retained as the historical rollout record).
**Owners:** stellarindex eng

ADR-0031 (data-derived coverage signal) and ADR-0032 (per-source
tables as projections) are tightly coupled. ADR-0032 reshapes the
write path; ADR-0031 reshapes the read path. Together they
eliminate the entire class of "drift between coverage signals" bugs
the platform has been generating for weeks.

This document is the **sequenced, risk-graded rollout** — six
phases over ~2-3 weeks, each shippable independently with a clean
rollback if anything goes sideways.

## Sequencing principles

1. **Read path lands before write path.** ADR-0031 (data-derived
   density) ships first as a shadow alongside the cursor-derived
   number, then becomes primary. ADR-0032's projector only ships
   after we trust the data-derived signal — otherwise we're
   debugging two unknowns at once.
2. **No big-bang switchovers.** Every behaviour-changing phase has
   a parallel-mode predecessor where both old and new are running.
3. **Net code delete by Phase 6.** Phases 0-4 ADD; Phases 5-6
   REMOVE. The temporary additions in 0-4 are scaffolds, not
   permanent surface.
4. **One PR per phase.** Each phase is ≤ 500 LoC change, ships as
   one rc.X release, and can be rolled back by reverting that PR
   without affecting later phases that haven't shipped yet.

## Phase 0 — Foundation (low risk, ~rc.91)

**Goal:** Add the missing primitives without changing any behaviour.

**Files:**
- `internal/storage/timescale/source_coverage.go` (NEW) — defines
  `SourceCoverage` struct + `GetSourceCoverage(ctx, source) (SourceCoverage, error)`.
- `internal/storage/timescale/gap_detector.go` — extend
  `scanOneGapDetectorTarget` to ALSO query `COUNT(DISTINCT ledger)`
  alongside the LAG-gap query. Emits new gauge
  `stellarindex_ingest_distinct_ledgers{source, table}`.
- `internal/obs/metrics.go` — register
  `IngestSourceDistinctLedgers`.
- `internal/storage/timescale/per_source_gaps.go` — add a small
  `SourceClaim` table mapping each source's name to its topic-0
  symbol set (already implicit in classify(); just made queryable).

**Tests:**
- Unit test: `GetSourceCoverage` returns expected values when
  given canned gauge data.
- Integration test: gap-detector cycle emits both gauges.

**Risk:** Negligible — read-only additions, no existing path
touched.

**Rollback:** Revert the PR.

## Phase 1 — Shadow read path (low risk, ~rc.92)

**Goal:** Surface ADR-0031 data-derived density as a side-by-side
comparison with the cursor-derived number, so we can verify they
agree before switching.

**Files:**
- `internal/api/v1/diagnostics_ingestion.go` —
  `BackfillCoverageRow` grows two new fields:
  - `DensityV2Pct float64 `json:"density_v2_pct"`` — data-derived
  - `GapFreePct float64 `json:"gap_free_pct"`` — 1 - max_gap / expected
  
  `buildBackfillCoverage` populates the v2 fields by calling
  `GetSourceCoverage` (from Phase 0) per source.
- `web/explorer/` — status-page widget surfaces both numbers
  side-by-side ("data-derived: 99.9% | cursor-derived: 97.2%") so
  operators can SEE the disagreement.

**Verification window:** 7 days. For every source, log when
`|density_pct - density_v2_pct| > 0.001` and investigate. Expect
disagreements during transition; once they're explained
(e.g. cursor-write delay, sparse-source threshold) the divergence
is documentated, not bug.

**Risk:** Low — adds JSON fields, existing consumers unaffected.

**Rollback:** Revert the PR.

## Phase 2 — Switchover read path (moderate risk, ~rc.93-94)

**Goal:** Make the data-derived signal primary. Headline
`density_pct` IS `density_v2_pct`.

**Files:**
- `internal/api/v1/diagnostics_ingestion.go` —
  - `DensityPct` becomes the v2 computation
  - `DensityV2Pct` field deleted (rename happens in lockstep
    with status-page deploy)
  - `cursorCoverageInterval` DELETED
  - `computeSourceCoverage` DELETED
  - `parseBackfillSubFull` DELETED (no other callers — verify
    `grep -rn parseBackfillSubFull` is empty)
  - `cursorDensityProjection`, `coverageInterval` types and
    helpers DELETED
- `internal/api/v1/diagnostics_ingestion_density_test.go` —
  tests rewritten to use the v2 path.
- `cmd/stellarindex-ops/drain_cascade_window.go` —
  `writeDrainBackfillCursor` DELETED (the rc.89 cursor-credit
  fix is now redundant; cursors aren't a coverage signal).
- Delete the SQL row inserted as the operator-fix on
  2026-05-29 03:00 — not needed any more.

**Verification:**
- Pre-switchover: density_v2 has been stable for 7 days (Phase 1).
- Post-switchover: status-page density numbers identical to the
  previous day. Alert rule `stellarindex_ingest_gap_detected`
  unchanged — same source of truth.

**Risk:** Moderate. Status-page customers see numbers driven by a
new computation. Mitigated by Phase 1's parallel-mode validation.

**Rollback:** Revert PR. Cursor-derived path is preserved in git
history and can be restored if the data-derived path turns out to
have a tunable we missed.

**LoC delta:** -600 to -800 (delete cursor-derived projection,
delete cursor-credit fix).

## Phase 3 — Projector scaffold (moderate risk, ~rc.95)

**Goal:** Stand up the projector component **in parallel** with
the existing per-source sink. Both write to per-source tables.
ON CONFLICT DO NOTHING absorbs duplicates.

**Files:**
- `internal/projector/projector.go` (NEW) — main loop:
  - Reads cursor `source='projector', sub_source=<protocol>`
  - Streams `soroban_events` rows where `ledger > last_ledger`
  - For each row, calls `<protocol>.Decoder.Decode(events.Event)`
    (the SAME Go code the dispatcher invokes today)
  - Routes the resulting `consumer.Event`s through the existing
    `persist*` functions (`pipeline/sink.go`)
  - Updates the projector cursor in the same transaction as
    the per-source insert (single Postgres tx)
- `internal/projector/source_registry.go` (NEW) — maps source
  name → `Decoder` + topic filter (the same data
  `pipeline/dispatcher.go::buildDispatcherDecoders` uses, lifted
  into a shared registry).
- `internal/projector/projector_test.go` (NEW) — integration
  test against testcontainers postgres.
- `cmd/stellarindex-indexer/main.go` — wires the projector into
  the running indexer (one goroutine per source). New config:
  `[projector] enabled = true, parallelism = 8`.
- `internal/obs/metrics.go` — adds
  `projector_lag_ledgers{source}` gauge + 
  `projector_events_decoded_total{source, outcome}` counter.
- `deploy/monitoring/rules/projector.yml` (NEW) — alert rule
  `stellarindex_projector_lag_high` fires when
  `projector_lag_ledgers > 1000` sustained 15min.

**Verification:**
- Per-source row rates remain constant (both writers active,
  ON CONFLICT absorbing the dupe).
- `projector_lag_ledgers` is stable at low single digits or zero
  (projector keeps up with live ingest within seconds).
- Decoder-error rates from the projector match the dispatcher's
  rates (sanity: same code, same input, same output).

**Risk:** Moderate. New component. Mitigated by parallel mode —
if projector breaks, per-source sink still works.

**Rollback:** Set `[projector] enabled = false` in r1 config,
restart indexer.

## Phase 4 — Flip primary writer (highest risk, ~rc.96)

**Goal:** The projector becomes the sole per-source writer.
Dispatcher's decoder loop still runs (for metrics + discovery)
but its `consumer.Event` outputs are dropped.

**Files:**
- `internal/dispatcher/dispatcher.go::dispatchOne` — change
  return value semantics: still returns `[]consumer.Event`s for
  test compatibility, but the live pipeline ignores them.
- `internal/pipeline/sink.go::handleOneEvent` — the per-source
  routing remains in code but the caller doesn't invoke it.
  (Removal in Phase 5.)
- New config flag: `[indexer] persist_per_source = false` —
  default false post-Phase-4, the consumer.Event return path
  remains for tests.

**Verification:**
- Per-source row rates remain constant (only projector writing
  now; should be identical to Phase 3 since ON CONFLICT was
  absorbing duplicates).
- Live decoder error count visible in metrics but doesn't affect
  per-source data (raw landed, projector retries on next cycle
  if decoder is fixed).

**Risk:** Highest of all phases — single point of failure on
projector. Mitigated by:
- Projector lag alert (Phase 3) pages within 15min if anything
  wedges.
- Easy rollback: flip `persist_per_source = true`.
- Phase 3 ran in parallel mode for at least a week before this
  ships.

**Rollback:** One config flag + indexer restart. Per-source sink
code still present in binary.

## Phase 5 — Delete dead code (low risk, ~rc.97)

**Goal:** Remove the now-unused per-source write path.

**Files DELETED:**
- `internal/pipeline/sink.go` per-source `persist*` functions
  (lines 334-770 roughly — anything that calls
  `store.Insert<protocol>*`). The `persistTrade` function stays
  (out-of-scope — non-Soroban path).
- `cmd/stellarindex-ops/blend_backfill.go` (~165 LoC)
- `cmd/stellarindex-ops/cctp_backfill.go` (~160 LoC)
- `cmd/stellarindex-ops/comet_liquidity_backfill.go` (~165 LoC)
- `cmd/stellarindex-ops/phoenix_backfill.go` (~180 LoC)
- `cmd/stellarindex-ops/rozo_backfill.go` (~150 LoC)
- `cmd/stellarindex-ops/sep41_transfers_backfill.go` (~200 LoC)
- `cmd/stellarindex-ops/soroswap_skim_backfill.go` (~155 LoC)
- `cmd/stellarindex-ops/drain_cascade_window.go` (~280 LoC)
- `cmd/stellarindex-ops/drain_cascade_window_test.go` (~200 LoC)
- `cmd/stellarindex-ops/main.go` — `case "blend-backfill":`
  + 6 sibling cases + `case "drain-cascade-window":` all
  deleted from the subcommand switch.
- `docs/operations/runbooks/cascade-window-drain.md` —
  superseded by projector-replay runbook (NEW, separate file).

**Files ADDED / UPDATED:**
- `cmd/stellarindex-ops/projector.go` (NEW, ~150 LoC) — operator
  subcommand `stellarindex-ops projector --source X --replay
  --from N --to M`. Wraps the same projector package code.
- `docs/operations/runbooks/projector-replay.md` (NEW) —
  replaces cascade-window-drain.md.

**Risk:** Low — all deleted code paths have been unused for
≥ 2 weeks by this point.

**Rollback:** Revert PR (only relevant if a bug is found that's
specifically in code we deleted; given Phase 4 ran with deleted
code as dead-code, this should be ironclad).

**LoC delta:** -1500 to -2000.

## Phase 6 — Polish + ADR status promotion (low risk, ~rc.98)

**Goal:** Promote ADRs from Proposed → Accepted, update memory,
close the loop.

**Files:**
- `docs/adr/0031-data-derived-coverage-signal.md` — status flip
  to `Accepted`.
- `docs/adr/0032-per-source-tables-as-projections.md` — status
  flip to `Accepted`.
- `docs/adr/0029-soroban-events-landing-zone.md` — add
  "Superseded in spirit by ADR-0032" note at top (the landing
  zone is still the truth store; only the write-relationship
  changed).
- `docs/adr/0030-per-source-coverage-invariant.md` — note that
  it's now implemented via ADR-0031 (the lint guard remains; the
  data-derived signal it required is now in place).
- `CLAUDE.md` § Invariants — add ADR-0031 + ADR-0032 entries.
- Memory: write a new entry
  `project_projection_architecture` summarising the post-rollout
  model (soroban_events = truth, per-source = projection).
  Remove or rewrite memory entries that referenced the old
  cursor-derived coverage as authoritative.

**Risk:** None — documentation only.

## Risk matrix

| Phase | Risk | Customer impact if regression | Mitigation |
|---|---|---|---|
| 0 Foundation | Negligible | None — read-only additions | Revert PR |
| 1 Shadow read | Low | None — additive JSON field | Revert PR |
| 2 Switchover read | Moderate | Density numbers visibly change | 7-day Phase 1 validation |
| 3 Projector scaffold | Moderate | None (parallel mode) | Config flag disable |
| 4 Flip primary writer | **HIGHEST** | Per-source row latency 50ms → seconds | Lag alert + config flag rollback |
| 5 Delete dead code | Low | None (deleted code unused) | Revert PR |
| 6 Polish | None | None | n/a |

## Timeline

- **Week 1:** Phase 0 + Phase 1 ship. Phase 1 starts 7-day shadow window.
- **Week 2:** Phase 1 shadow completes; Phase 2 ships mid-week. Phase 3 ships end of week.
- **Week 3:** Phase 3 runs in parallel mode for full week. Phase 4 ships end of week.
- **Week 4:** Phase 5 ships. Phase 6 ships end of week.

Total: ~3-4 weeks calendar, ~4-6 PR-days engineering effort.

## Things this rollout does NOT do

- Does not change the `trades` hypertable's write path. CEX/FX/SDEX
  continue writing directly. They're not Soroban events.
- Does not change the API surface seen by customers. `/v1/price`,
  `/v1/contracts/{id}/transfers`, etc. all keep their existing
  shape. Only `/v1/diagnostics/ingestion`'s `density_pct`
  computation changes — and the number it reports gets MORE
  honest, not different.
- Does not change the decoder packages
  (`internal/sources/<protocol>/`). The Go code that interprets
  Soroban events is unchanged — it just gets called by a
  different driver.
- Does not introduce a new binary. The projector is a component
  inside `cmd/stellarindex-indexer/`.
- Does not change the alert surface. `stellarindex_ingest_gap_detected`
  continues firing on the same conditions; adds
  `stellarindex_projector_lag_high` as a complementary alert.

## Open questions

1. **Projector parallelism:** one goroutine per source vs one
   pool that fans out? Phase 3 will start with one goroutine per
   source (simpler reasoning); revisit if scan throughput is a
   bottleneck.
2. **Projector vs aggregator co-location:** projector should run
   on the indexer host (it reads MinIO via `soroban_events` which
   is local Postgres). Aggregator is a separate concern; no
   change.
3. **Read-after-write contract for `/v1/contracts/{id}/transfers`:**
   should the handler reflect data within 1s or 30s of the
   contract event? Today: 1s (live dispatcher writes directly).
   Post-Phase-4: 5-30s (projector lag). **Probably acceptable**
   given customer use case, but worth checking with usage data.
   If unacceptable, Phase 3 adds a real-time fast-path bypass
   for this specific handler.
