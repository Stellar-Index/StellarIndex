-- 0199 up — stamp the entry-walk version on the five balance-observation
-- hypertables and make it the outer key of the last-writer-wins guard.
--
-- intra_ledger_seq is a position in one numbering of a ledger's entry
-- changes (internal/dispatcher.EntryWalkVersion). The guard 0111 added,
--   WHERE <t>.intra_ledger_seq <= EXCLUDED.intra_ledger_seq,
-- compares positions across binaries, so after a walk-order change a row
-- written under the old numbering can carry a HIGHER position than the
-- corrected row for the same (key, ledger) and silently reject it, on every
-- re-run (migration 0120). Until now nothing recorded which numbering a row
-- came from, so the guard could not tell the two apart.
--
-- WHAT THIS MIGRATION CHANGES
--
-- Adds `walk_version` to each table. The writers stamp
-- dispatcher.EntryWalkVersion and guard with a lexicographic row comparison,
-- version outer and position inner:
--   WHERE (<t>.walk_version, <t>.intra_ledger_seq)
--      <= (EXCLUDED.walk_version, EXCLUDED.intra_ledger_seq)
-- A re-derive under a newer walk replaces any older-walk row; within one
-- walk the higher position still wins, so the out-of-order PersistEvents
-- protection (C2-6) is unchanged. The positions stay in their own column,
-- as 0111 and 0120 require.
--
-- 0 means "not recorded": every existing row, and every row the previous
-- binary writes until it is replaced. It sorts below every real version, so
-- any stamped re-derive of a ledger outranks it. That is safe because a
-- re-derive walks the whole ledger: its own changes for the key are then
-- compared within one version, and the last one wins whatever the commit
-- order. An ops seed (intra_ledger_seq = MaxUint32) is stamped too, so a
-- seed stays unbeatable within its walk version but not by a later one.
--
-- RULE 9. One smallint column with a constant default per table:
-- catalog-only, no rewrite; TimescaleDB 2.11+ adds it to a compressed
-- hypertable directly (as 0111 did). The previous binary does not name the
-- column; its inserts get 0 and its guard ignores it, which is its own
-- behaviour, unchanged.

BEGIN;

ALTER TABLE account_observations
    ADD COLUMN IF NOT EXISTS walk_version smallint NOT NULL DEFAULT 0;

ALTER TABLE trustline_observations
    ADD COLUMN IF NOT EXISTS walk_version smallint NOT NULL DEFAULT 0;

ALTER TABLE claimable_observations
    ADD COLUMN IF NOT EXISTS walk_version smallint NOT NULL DEFAULT 0;

ALTER TABLE lp_reserve_observations
    ADD COLUMN IF NOT EXISTS walk_version smallint NOT NULL DEFAULT 0;

ALTER TABLE sac_balance_observations
    ADD COLUMN IF NOT EXISTS walk_version smallint NOT NULL DEFAULT 0;

COMMENT ON COLUMN account_observations.walk_version IS
    'dispatcher.EntryWalkVersion of the walk that numbered intra_ledger_seq; '
    '0 = not recorded. Outer key of the upsert guard '
    '(walk_version, intra_ledger_seq) <= EXCLUDED, so a re-derive under a '
    'newer walk replaces a row numbered by an older one.';
COMMENT ON COLUMN trustline_observations.walk_version IS
    'Entry-walk version of intra_ledger_seq; see account_observations.walk_version.';
COMMENT ON COLUMN claimable_observations.walk_version IS
    'Entry-walk version of intra_ledger_seq; see account_observations.walk_version.';
COMMENT ON COLUMN lp_reserve_observations.walk_version IS
    'Entry-walk version of intra_ledger_seq; see account_observations.walk_version.';
COMMENT ON COLUMN sac_balance_observations.walk_version IS
    'Entry-walk version of intra_ledger_seq; see account_observations.walk_version.';

COMMIT;
