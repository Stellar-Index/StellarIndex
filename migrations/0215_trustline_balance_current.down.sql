-- 0215 down — drop the trigger-kept current trustline balance. The 0215
-- reader errors once these tables are gone, so take the reader back to a
-- pre-0215 binary first.

BEGIN;

DROP TRIGGER IF EXISTS trustline_balance_current_truncate ON trustline_observations;
DROP TRIGGER IF EXISTS trustline_balance_current_track ON trustline_observations;
DROP FUNCTION IF EXISTS trustline_balance_current_truncate();
DROP FUNCTION IF EXISTS trustline_balance_current_track();
DROP FUNCTION IF EXISTS trustline_balance_current_invalidate(text, text);
DROP TABLE IF EXISTS trustline_balance_current_seeded;
DROP TABLE IF EXISTS trustline_balance_current;

COMMIT;
