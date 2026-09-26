-- Revert 0183: drop the claimable seed audit trail. The rows are lost; the
-- next complete `supply seed-claimable-balances` pass after a re-up writes
-- them again. claimable_observations itself is untouched.

BEGIN;

DROP TABLE IF EXISTS claimable_seed_provenance;

COMMIT;
