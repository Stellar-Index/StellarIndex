-- Revert 0182: drop the seed-evidence columns. The evidence they held is
-- lost; re-seeding after a re-up restores it.

BEGIN;

ALTER TABLE sac_balance_seed_provenance
    DROP COLUMN IF EXISTS lake_verified_through,
    DROP COLUMN IF EXISTS holders_retracted;

COMMIT;
