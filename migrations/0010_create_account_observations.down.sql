-- 0010 down — drop account_observations.

BEGIN;

DROP TABLE IF EXISTS account_observations CASCADE;

COMMIT;
