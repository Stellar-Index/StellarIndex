-- 0001 down — reverse of 0001_create_trades_hypertable.up.sql.

BEGIN;

-- Retention + compression policies auto-drop with the hypertable.
-- We drop the hypertable via DROP TABLE (timescaledb hooks handle chunks).

DROP TABLE IF EXISTS ingestion_cursors;
DROP TABLE IF EXISTS trades;

-- Do NOT drop the timescaledb extension — other migrations may use it.

COMMIT;
