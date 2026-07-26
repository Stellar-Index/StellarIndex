-- 0120 down — restore migration 0111's original intra_ledger_seq column
-- comments verbatim.
--
-- Comment-only in both directions: 0120 up added no column and touched no
-- row, so there is nothing to un-write. Rolling back restores the OLD
-- (per-transaction, and now known-false) description of the walk order —
-- which is correct for a rollback, because rolling this migration back
-- means rolling the binary back to a walk version that produced that order.

BEGIN;

COMMENT ON COLUMN account_observations.intra_ledger_seq IS
    'Within-ledger position of the change that produced this row, in the '
    'dispatcher''s canonical meta-walk order (audit-2026-07-16 C2-6). Guards '
    'the last-writer-wins upsert (intra_ledger_seq <= EXCLUDED.intra_ledger_seq) '
    'so an out-of-order PersistEvents worker can never persist a stale '
    'intra-ledger balance as the final observation. Ops seeds stamp '
    'math.MaxUint32 (the authoritative final state for the ledger).';
COMMENT ON COLUMN trustline_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';
COMMENT ON COLUMN claimable_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';
COMMENT ON COLUMN lp_reserve_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';
COMMENT ON COLUMN sac_balance_observations.intra_ledger_seq IS
    'Within-ledger change position (audit-2026-07-16 C2-6). See '
    'account_observations.intra_ledger_seq.';

COMMIT;
