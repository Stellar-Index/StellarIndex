-- 0035 down — drop the per-source entry tally.

BEGIN;

DROP TABLE IF EXISTS source_entry_counts;

COMMIT;
