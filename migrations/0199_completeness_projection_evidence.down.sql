-- 0199 down — drop the projection evidence columns. Dev/local only
-- (migrations/README.md rule 9); the binary that writes them must be
-- rolled back first.

BEGIN;

ALTER TABLE completeness_snapshots
    DROP COLUMN IF EXISTS projection_evidenced_at,
    DROP COLUMN IF EXISTS projection_reconciled_from;

COMMIT;
