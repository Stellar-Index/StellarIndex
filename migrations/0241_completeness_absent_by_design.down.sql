-- 0241 down — drop the absent-by-design verdict columns.

BEGIN;

ALTER TABLE completeness_snapshots
    DROP COLUMN IF EXISTS absent_declared_but_present,
    DROP COLUMN IF EXISTS absent_by_design;

COMMIT;
