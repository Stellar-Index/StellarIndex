-- 0222 down — drop the trigger-kept current SAC balance. The 0222 reader
-- errors once these tables are gone, so take the reader back to a pre-0222
-- binary first.

BEGIN;

DROP TRIGGER IF EXISTS sac_balance_current_truncate ON sac_balance_observations;
DROP TRIGGER IF EXISTS sac_balance_current_track ON sac_balance_observations;
DROP FUNCTION IF EXISTS sac_balance_current_truncate();
DROP FUNCTION IF EXISTS sac_balance_current_track();
DROP FUNCTION IF EXISTS sac_balance_current_invalidate(text, text, text);
DROP TABLE IF EXISTS sac_balance_current_seeded;
DROP TABLE IF EXISTS sac_balance_current;

COMMIT;
