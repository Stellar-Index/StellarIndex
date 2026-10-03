-- 0199 down — drop walk_version from the five balance-observation
-- hypertables. The guard falls back to intra_ledger_seq alone; no other
-- column is touched.

BEGIN;

ALTER TABLE sac_balance_observations
    DROP COLUMN IF EXISTS walk_version;

ALTER TABLE lp_reserve_observations
    DROP COLUMN IF EXISTS walk_version;

ALTER TABLE claimable_observations
    DROP COLUMN IF EXISTS walk_version;

ALTER TABLE trustline_observations
    DROP COLUMN IF EXISTS walk_version;

ALTER TABLE account_observations
    DROP COLUMN IF EXISTS walk_version;

COMMIT;
