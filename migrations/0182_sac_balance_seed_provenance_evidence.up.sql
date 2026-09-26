-- 0182 up — record what a `supply seed-sac-balances` pass established,
-- not only which table it was asked to read (GH #713).
--
-- `sac_balance_seed_provenance` (0102) stamped `source` from the CLI flag
-- before any lake work ran, so a pass by a binary that seeded TTL-archived
-- balances as live (PHO's +157 %) and a pass by the current binary wrote
-- byte-identical rows, and a full-history walk across a lake hole was
-- stamped `full_history` like any other.
--
-- lake_verified_through: the ledger through which stellar.ledgers was
--   proven contiguous and hash-linked (SubstrateProblem) before the
--   full-history reduction emitted anything. NULL on a current_state row
--   (not a coverage claim) and on every row written before this migration;
--   a `full_history` row with NULL here predates the check and must be
--   re-seeded before it is trusted.
-- holders_retracted: removed or TTL-archived holders the pass wrote as
--   tombstones. NULL on rows written before this migration.
--
-- Rule 9: both columns are nullable with no default; the previous binary's
-- upsert names its columns explicitly and never touches these.

BEGIN;

ALTER TABLE sac_balance_seed_provenance
    ADD COLUMN IF NOT EXISTS lake_verified_through bigint
        CHECK (lake_verified_through IS NULL OR lake_verified_through >= 0),
    ADD COLUMN IF NOT EXISTS holders_retracted integer
        CHECK (holders_retracted IS NULL OR holders_retracted >= 0);

COMMENT ON COLUMN sac_balance_seed_provenance.lake_verified_through IS
    'Ledger through which the lake was proven contiguous and hash-linked '
    'before a full_history pass emitted anything. NULL on current_state rows '
    'and on rows written before migration 0182: a full_history row with NULL '
    'here predates lake verification and the archived-entry filter — re-seed.';
COMMENT ON COLUMN sac_balance_seed_provenance.holders_retracted IS
    'Removed or TTL-archived holders the pass wrote as is_removal tombstones. '
    'NULL on rows written before migration 0182.';

COMMIT;
