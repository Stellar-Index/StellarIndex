-- 0189 down — drop the seed-observations audit trail. The row is lost; the
-- next complete `supply seed-observations` pass after a re-up writes it
-- again. account_observations itself is untouched.

BEGIN;

DROP TABLE IF EXISTS account_observation_seed_provenance;

COMMIT;
