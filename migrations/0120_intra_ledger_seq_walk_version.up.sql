-- 0120 up — AMENDMENT to migration 0111's intra_ledger_seq contract
-- (C2-032 / C2-023 / C2-040 / R-A01-1, audit-2026-07-23).
--
-- Comment-only. No column is added, dropped, or rewritten; no row is
-- touched. It exists because 0111 is already applied on r1 and its column
-- comments — which an operator reads straight off the live schema with
-- `\d+ account_observations` — state an ORDER that is no longer the order
-- the writers produce, and omit an invariant that can silently discard a
-- corrective re-derive.
--
-- ─── WHAT 0111 GOT WRONG ────────────────────────────────────────────────
--
-- 0111 described the position as assigned "within each transaction
-- fee-changes, tx-changes-before, operations …, tx-changes-after" — a
-- PER-TRANSACTION walk — and claimed that was "the same canonical
-- within-ledger order internal/storage/clickhouse/entry_change_reader.go
-- uses". Both were false. stellar-core commits a ledger in LEDGER-WIDE
-- PHASES, which is what the SDK's ingest.LedgerChangeReader state machine
-- (feeChangesState -> metaChangesState -> postTxApplyState) reproduces:
--
--   phase 1  every tx's fee changes             (processFeeSeqNum charges
--                                                ALL fees before applying
--                                                ANY transaction)
--   phase 2  every tx's apply-phase meta        (tx-changes-before, per-op
--                                                changes in op_index /
--                                                change_index order,
--                                                tx-changes-after)
--   phase 3  every tx's post-apply fee changes  (protocol 23 moved the
--                                                Soroban fee REFUND into a
--                                                ledger-wide phase applied
--                                                after all transactions
--                                                execute; LCM V2 only)
--
-- Under the per-tx walk, an account touched by tx1's operations and tx2's
-- fee had the FEE ranked last. Since this guard keeps the HIGHEST position,
-- it preserved a fee-phase balance as that ledger's final observation —
-- exactly the class of wrong classic-supply component 0111 was written to
-- prevent. Failed transactions were additionally skipped altogether, so
-- their committed fee debits never produced an observation at all
-- (C2-023/C2-040).
--
-- ─── THE INVARIANT 0111 DID NOT STATE ───────────────────────────────────
--
-- POSITIONS ARE SCOPED TO A WALK VERSION (internal/dispatcher.EntryWalkVersion,
-- now 2). A position is only meaningful against another position produced by
-- the SAME walk. But intra_ledger_seq is PERSISTED and COMPARED ACROSS BINARY
-- VERSIONS by this table's `WHERE intra_ledger_seq <= EXCLUDED.intra_ledger_seq`
-- guard (and, in the lake, by ledger_entries_current_v2's ReplacingMergeTree
-- version `(ledger_seq << 32) | intra_ledger_seq`).
--
-- So a LEGACY ROW CAN OUTRANK A CORRECTION. Concretely: the v1 walk stamped
-- account A's ledger-final balance with position 6 (the C2-032 corruption —
-- a fee-phase change that sorted last); the v2 walk correctly places that
-- same final balance at position 3. A corrective re-derive writes 3, the
-- guard evaluates `6 <= 3` = FALSE, and the corrected write is silently
-- dropped. Re-running does not help: the re-derive is deterministic and
-- recomputes the same lower position every time. 0111 reasoned this exact
-- boundary through for the SEED path and left it unstated for a walk-order
-- change; this migration closes that.
--
-- ─── THE REPAIR PATH ────────────────────────────────────────────────────
--
-- Correcting rows written by an older walk version is NOT "replay the
-- changes". It is RECONSTRUCT-FINAL-THEN-SEED:
--
--   1. Repair the lake first (delete-then-replay the range — a lower RMT
--      version cannot displace a higher one either, so the ClickHouse
--      partition must be DROPPED before re-ingest, not merely re-inserted).
--   2. Derive the FINAL per-(key, ledger) state from the repaired lake and
--      write ONE row per key per ledger stamped
--      timescale.SeedIntraLedgerSeq (4294967295 = math.MaxUint32). The `<=`
--      guard always admits the sentinel regardless of what the legacy row
--      carries, and a re-run re-admits it (equal sentinel) so the repair is
--      idempotent-corrective. This is the mechanism 0111 already built for
--      the ops seed writers; it is being NAMED here as the general
--      cross-walk-version repair.
--
-- The sentinel is sound ONLY for a reconstructed FINAL state — exactly one
-- row per (key, ledger). Stamping it on a change-by-change replay would tie
-- every change in the ledger at MaxUint32, and `<=` would then admit any of
-- them in any order, re-opening the C2-6 last-writer-wins bug this column
-- exists to close.
--
-- Full operator procedure:
--   docs/operations/runbooks/entry-walk-renumbering.md
--
-- ─── FORWARD COMPOSITION (unchanged from 0111) ──────────────────────────
--
-- These five tables still do not carry derive_generation. If it is ever
-- added, the guard must compose with GENERATION AS THE OUTER key and SEQ AS
-- THE INNER — a higher generation wins regardless of position, and within a
-- generation the higher position wins:
--   WHERE (<t>.derive_generation, <t>.intra_ledger_seq)
--      <= (EXCLUDED.derive_generation, EXCLUDED.intra_ledger_seq)
-- (a lexicographic ROW comparison). Do NOT collapse the two into one column.
-- That would subsume the repair path above, since a bumped generation would
-- let a correction win on its own.

BEGIN;

COMMENT ON COLUMN account_observations.intra_ledger_seq IS
    'Within-ledger position of the change that produced this row, in the '
    'dispatcher''s canonical LEDGER-WIDE THREE-PHASE walk order: all txs'' '
    'fee changes, then all txs'' apply-phase meta, then all txs'' post-apply '
    'fee changes (P23 Soroban refunds). Mirrors the SDK''s '
    'ingest.LedgerChangeReader. Guards the last-writer-wins upsert '
    '(intra_ledger_seq <= EXCLUDED.intra_ledger_seq) so an out-of-order '
    'PersistEvents worker can never persist a stale intra-ledger balance as '
    'the final observation (audit-2026-07-16 C2-6). Ops seeds stamp '
    'math.MaxUint32 (the authoritative final state for the ledger). '
    'POSITIONS ARE SCOPED TO dispatcher.EntryWalkVersion (now 2): the '
    'C2-032 fix renumbered every ledger, so a legacy row can outrank a '
    'correction and a change-replay re-derive will be silently dropped by '
    'this guard. Repair via reconstruct-final-then-seed — see migration '
    '0120 and docs/operations/runbooks/entry-walk-renumbering.md.';
COMMENT ON COLUMN trustline_observations.intra_ledger_seq IS
    'Within-ledger change position, ledger-wide three-phase walk order '
    '(audit-2026-07-16 C2-6; order corrected C2-032, audit-2026-07-23). '
    'Walk-version-scoped — see account_observations.intra_ledger_seq.';
COMMENT ON COLUMN claimable_observations.intra_ledger_seq IS
    'Within-ledger change position, ledger-wide three-phase walk order '
    '(audit-2026-07-16 C2-6; order corrected C2-032, audit-2026-07-23). '
    'Walk-version-scoped — see account_observations.intra_ledger_seq.';
COMMENT ON COLUMN lp_reserve_observations.intra_ledger_seq IS
    'Within-ledger change position, ledger-wide three-phase walk order '
    '(audit-2026-07-16 C2-6; order corrected C2-032, audit-2026-07-23). '
    'Walk-version-scoped — see account_observations.intra_ledger_seq.';
COMMENT ON COLUMN sac_balance_observations.intra_ledger_seq IS
    'Within-ledger change position, ledger-wide three-phase walk order '
    '(audit-2026-07-16 C2-6; order corrected C2-032, audit-2026-07-23). '
    'Walk-version-scoped — see account_observations.intra_ledger_seq.';

COMMIT;
