-- 0220 down — drop the trigger-kept current LP reserve. The 0220 reader
-- errors once these tables are gone, so take the reader back to a pre-0220
-- binary first.

BEGIN;

DROP TRIGGER IF EXISTS lp_reserve_current_truncate ON lp_reserve_observations;
DROP TRIGGER IF EXISTS lp_reserve_current_track ON lp_reserve_observations;
DROP FUNCTION IF EXISTS lp_reserve_current_truncate();
DROP FUNCTION IF EXISTS lp_reserve_current_track();
DROP FUNCTION IF EXISTS lp_reserve_current_invalidate(text, text);
DROP TABLE IF EXISTS lp_reserve_current_seeded;
DROP TABLE IF EXISTS lp_reserve_current;

COMMIT;
