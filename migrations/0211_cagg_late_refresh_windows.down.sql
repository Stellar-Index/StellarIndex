-- 0211 down — drop the persisted late-write cagg refresh windows.
--
-- Dropping the table discards any pending window. Before migrating down,
-- deploy a binary that ignores the table, SELECT the rows, and refresh
-- each view by hand over [from_ts, to_ts] (non-forced
-- refresh_continuous_aggregate) so no late trade stays unmaterialised.

BEGIN;

DROP TABLE IF EXISTS cagg_late_refresh_windows;

COMMIT;
