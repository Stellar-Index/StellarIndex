# Ingest/storage/completeness invariants — re-derivation (audit-2026-08-01, HEAD f8c099ee)

Cold, read-only, tests read by body. Tier = weakest writer.
(Full per-invariant ledger + soundness re-derivations in tasks/ae4b2e01fff2e3d1c.output.)

## Findings register (→ skeptic verification)

| ID | Sev | Class | One-line |
|---|---|---|---|
| W-1 | HIGH | weak tier | ADR-0032 "projector is SOLE writer" is CONVENTION (one ansible line, persist_per_source=true) for 15/17 sources; r1 runs Phase-3 DUAL-write. IsSoleWriterProjected = sep41 only. "Drift is structurally impossible" is false at running config. |
| F-2 | HIGH | incomplete remediation | ch-rebuild -write (+ backfill_router/chainlink/external) rewrite below-watermark served rows with NO dirty-window record; migration 0125 closed 2 of ≥5 writers; nothing structural forces the next. |
| F-4 | HIGH mech/MED live | latent dup | comet_liquidity + phoenix_liquidity + phoenix_stake_events carry the cctp 0-default-vs-true-index twin pattern BY EXPLICIT migration design (0059:20-22, 0060:22-24 "no DELETE needed" — repeats 0112's false inference). Phoenix worse: event_index is first-field index of multi-event reassembly (never 0 for 2nd action). Prevented at no tier; created by any re-derive over pre-migration range. |
| F-5 | MED | latent dup (new) | oracle_updates op_index fanout formula changed 2026-07-25 (f702914a) with no purge; PK excludes event_index; pre/post rows differ in PK for OperationIndex>0 → twins. These 4 sources are the aggregateReconcile ones that can NET a twin against a drop. |
| F-6 | HIGH | interaction | Dual-writer × two StateWriteKeys provenances (dispatcher tx-meta vs projector lake) on oracle_updates PK that EXCLUDES asset/price → nondeterministic redstone attribution + verifier-invisible phantoms; a lake coverage hole → CH keys nil → equal-arity corroboration SKIPPED while dispatcher runs it. NO live/lake parity test for the 3-clause rule. |
| F-7 | MED | fail-closed avail | ErrStateWriteFeedMismatch refuses whole redstone events on any extra value-changed ScString key; caught by BlindSpots (good) but serving consequence (feed goes dark) is WATCHER-only; 2026-07-24 precedent. |
| F-8 | HIGH | guard gap | ops_by_source reader guard proves EXISTENCE not COVERAGE (SELECT ... LIMIT 1); Step-2 backfill is a manual shell loop in a SQL comment → apply Step 1 + deploy without Step 2 = silently truncated self-sourced account history. |
| F-9 | LOW | semantics | ops_by_source tx arm broadens tx-sourced → any-op-sourced while header claims exact-semantics preservation. |
| F-10 | MED | watcher gap | stellarindex_sdex_orderbook_crossed_pairs — the named tripwire for the residual zombie class VerifyPending can't disprove — has NO alert rule; log-only. |
| D-1 | MED | ADR demotion | ADR-0033 Claim 2b tier-2 provenance anti-join UNIMPLEMENTED; only ReconcileCounts (count-only) exists; same-ledger drop+phantom nets; misses never localised. |
| N-1 | INFO | doc drift | migration 0125_*.sql headers say "0124 up/down"; projected_rebuild.go:219 cites "migration 0124" (0124 is freeze_reason_other). |
| N-2 | INFO | doc drift | reconciliation_catalogue.go:150-153 notes DefaultGapDetectorTargets still carries pre-correction cctp/rozo genesis (62_403_000). |
| N-3 | INFO | drift | ops_by_source sentinel 4294967295 duplicated across 3 sites, no shared constant/test. |

## VERIFIED-SOUND (do NOT re-flag)

- The dirty-window CLEAR is sound — re-derived, not trusted: dirtyWindowSatisfied gets
  projFrom not runFrom, projOK can be carry-true, but the carried prefix
  [servedFrom, runFrom-1] lies strictly BELOW max(w.From, genesis) in every case, so
  a carry can never clear a window. Bounded DELETE race-safe. Field-proven in evidence.
- Order-book quarantine CONVERGES: pending strictly non-increasing (every drawn ref
  deleted regardless of outcome), 2500/tick ≈ 4s of 60s, suspects unserved throughout
  (under-serve-never-over-serve). crossedPairCount int32 products stay in int64.
- The redstone OpArgs provenance gate is CORRECT on both sides (unconditional strkey
  equality, no fallback arm, tested both). Value-changed rule EMPIRICALLY anchored
  (r1 ledger 62056824). Lake poison-key-not-row + RMT latest-version dedup both subtle+right.
- lockstep_ast_test.go (AST walk registry↔IsProjectedEvent↔persist switch) = strongest
  guard in the subsystem. BlindSpots exported two-class tracker. projectionClaim/
  substrateClaim symmetric fail-closed ladders with verified range on the wire.

## ADR reconcile highlights
- ADR-0032 "drift structurally impossible" FALSIFIED by the redstone state-write work (F-6).
- ADR-0032 Phase 2 never shipped in config (W-1); ADR-0031 superseded-in-practice by
  0033 but still status:Accepted, superseded_by:null.
- ADR-0033 Claim 2b tier-2 anti-join unimplemented (D-1) — why the twin classes are
  only ever visible as a count delta.
- ADR-0034 "event_index collision impossible in the lake" HOLDS for the lake, but the
  SERVED tier's twin class is the mirror (silent OVER-projection) created by the cheap
  re-derives 0034 enables — recipe must record the dual.

## Recipe deltas
migrations 108→120 (head 0125). New traps: (a) "a PK-discriminator migration with
DEFAULT 0 saying 'no DELETE needed' arms a twin for the next re-derive" (0112 fired,
0059/0060 loaded); (b) "any tool writing a reconcile-target table below the completeness
watermark must RecordProjectionDirtyWindow first" (2 of ≥5 do). New seams: ops_by_source
(deploy-artifact schema, outside golang-migrate) + in-process SDEX order book
(process-local quarantine, no alert on its own invariant). Stop framing dispatcher↔
projector as transitional — Phase 3 is steady state, and ON CONFLICT no longer resolves
the race identically once the two provenances diverge.
