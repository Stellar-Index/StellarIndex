-- 0047 down — drop the sep41_transfers hypertable.
BEGIN;
DROP TABLE IF EXISTS sep41_transfers;
COMMIT;
