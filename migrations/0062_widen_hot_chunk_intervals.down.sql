-- 0062 down — intentionally a NO-OP.
--
-- Reverting the chunk_time_interval to 1 day would re-introduce the
-- lock-pressure / chunk-explosion pathology that 0062 fixes (audit-2026-06-14
-- A15; the trades 3445-chunk crisis). There is no legitimate reason to roll
-- back a chunk-width widening, and set_chunk_time_interval only affects future
-- chunks anyway, so a rollback could not "undo" the wider chunks already
-- written. Forward-only by design.
SELECT 1;
