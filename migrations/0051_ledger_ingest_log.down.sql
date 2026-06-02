-- 0051 down — drop the ledger_ingest_log substrate-continuity record.
BEGIN;
DROP TABLE IF EXISTS ledger_ingest_log;
COMMIT;
