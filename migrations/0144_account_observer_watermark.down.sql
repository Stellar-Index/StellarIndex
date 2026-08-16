-- 0144 down — drop the account observer watermark table.
--
-- Reverses 0144 up exactly. The account observer's freshness anchor falls back
-- to whatever the paired binary computes; on the released binary that predates
-- 0144 this table does not exist, so the drop restores the prior schema
-- cleanly. No data loss of consequence — the single row is a live cursor the
-- indexer re-establishes on its next tick.

BEGIN;

DROP TABLE IF EXISTS account_observer_watermark;

COMMIT;
