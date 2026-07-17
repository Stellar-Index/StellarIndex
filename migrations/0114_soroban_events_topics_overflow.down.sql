-- 0114 down — drop `topics_xdr` from soroban_events, reverting the
-- schema half of the C2-11 full-topic-list fix.
--
-- The post-0114 binary reads/writes topics_xdr (the writer's INSERT
-- column list + StreamSorobanEvents / the topic_samples recognition
-- reads), so a rollback of this migration must be paired with a
-- rollback of the binary to the pre-0114 (topic_0..3-only) shape.
-- DROP COLUMN on a compressed hypertable is supported directly on
-- TimescaleDB 2.11+ (r1 runs 2.26) — same as 0111's down.
--
-- Data note: dropping topics_xdr discards the full-topic-list bytes for
-- any 5+-topic rows captured post-0114. topic_0..3 still hold the first
-- four; the lost tail is recoverable from the ClickHouse raw lake (as it
-- was pre-0114). down.sql is a local/dev iteration lever, not a
-- production rollback lever (README rule 9).

BEGIN;

ALTER TABLE soroban_events
    DROP COLUMN IF EXISTS topics_xdr;

COMMIT;
