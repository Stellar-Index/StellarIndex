-- 0052 down — drop the completeness_snapshots watermark table.
BEGIN;
DROP TABLE IF EXISTS completeness_snapshots;
COMMIT;
