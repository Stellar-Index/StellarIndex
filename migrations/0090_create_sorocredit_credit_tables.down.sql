-- 0090 down — drop the sorocredit credit tables.
BEGIN;
DROP TABLE IF EXISTS credit_events;
DROP TABLE IF EXISTS credit_settlements;
DROP TABLE IF EXISTS credit_statements;
DROP TABLE IF EXISTS credit_positions;
COMMIT;
