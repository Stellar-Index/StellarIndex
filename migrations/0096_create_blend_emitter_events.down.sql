-- 0095 down — drop the blend_emitter_events hypertable.
--
-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression settings in one statement. CASCADE is not needed —
-- nothing references blend_emitter_events (blend_emitter is the sole
-- writer; no CAGG or view is built on top of it).

BEGIN;

DROP TABLE IF EXISTS blend_emitter_events;

COMMIT;
