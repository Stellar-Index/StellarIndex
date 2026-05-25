-- 0041 down — drop the soroban_events hypertable (ADR-0029).
--
-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression settings in one statement. CASCADE is not needed —
-- nothing references soroban_events (the table is the raw-event
-- landing zone; no CAGG or view is built on top of it).

BEGIN;

DROP TABLE IF EXISTS soroban_events;

COMMIT;
